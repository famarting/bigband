package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/famarting/bigband/pkg/bigbandext"
)

// Slack is the minimal Slack client surface the router needs. Implemented in
// slack.go via the slack-go/slack library; tests can stub it.
type Slack interface {
	PostMessage(channel, text, threadTS string) (newThreadTS string, err error)
	AddReaction(channel, emoji, timestamp string) error
}

// Router glues bigband events to Slack and Slack messages to bigband IPC.
// It owns no goroutines — start.go drives both directions and calls into
// Router methods to handle individual events / messages.
//
// cfg is held behind cfgMu so SIGHUP-driven hot reload can swap it in without
// racing with HandleEvent / HandleSlackMessage. All cfg reads go through
// snapshotCfg.
//
// bb is the bigband daemon client — every IPC call (submit, run, etc.) goes
// through it. Using pkg/bigbandext.Client (rather than internal/ipc) keeps
// this integration honest as a reference: it depends only on what an
// external integration would have access to.
type Router struct {
	store *Store
	slack Slack
	bb    *bigbandext.Client

	cfgMu sync.RWMutex
	cfg   *Config
}

// snapshotCfg returns the currently active config. Callers must not mutate it.
func (r *Router) snapshotCfg() *Config {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// SetConfig swaps in a new config atomically. Used by SIGHUP-driven reload.
func (r *Router) SetConfig(cfg *Config) {
	r.cfgMu.Lock()
	r.cfg = cfg
	r.cfgMu.Unlock()
}

// HandleEvent processes one bigband lifecycle envelope. Currently it only acts
// on JobRunCompleted; other event types are ignored silently. We do not
// special-case JobRunStarted because Slack users only care about the result.
func (r *Router) HandleEvent(env bigbandext.Envelope) {
	log.Printf("bigband-slack: event type=%s job=%s run=%s", env.Type, env.JobName, env.RunID)
	switch env.Type {
	case bigbandext.TypeClaudeSessionStarted:
		// Capture the session id early so thread replies can resume even if
		// the run hasn't completed yet (we still wait for completion before
		// posting, but the session id is now safely persisted).
		var data bigbandext.ClaudeSessionStartedData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			log.Printf("bigband-slack: unmarshal %s event: %v", env.Type, err)
			return
		}
		if data.SessionID == "" {
			return
		}
		// Persist on the run mapping if we already have one, otherwise on the
		// job mapping. Either way the thread reply path can find it.
		_ = r.store.LinkJobMeta(env.JobName, "", "")
		_ = r.store.SetJobSessionID(env.JobName, data.SessionID)

	case bigbandext.TypeJobRunWorktreeReady:
		var data bigbandext.JobRunWorktreeReadyData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			log.Printf("bigband-slack: unmarshal %s event: %v", env.Type, err)
			return
		}
		_ = r.store.LinkJobMeta(env.JobName, "", data.WorktreePath)

	case bigbandext.TypeJobRunCompleted:
		r.handleCompleted(env)
	}
}

func (r *Router) handleCompleted(env bigbandext.Envelope) {
	runMapping := r.store.LookupRun(env.RunID)

	var data bigbandext.JobRunCompletedData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		log.Printf("bigband-slack: cannot decode job_run.completed for %s: %v", env.JobName, err)
		return
	}

	// parentName is the stable identity used for store keys. For follow-ups
	// the run mapping already names the parent; for everything else we fall
	// back to the ephemeral job name on the event itself.
	parentName := runMapping.JobName
	if parentName == "" {
		parentName = env.JobName
	}

	// Persist folder + worktree + session id under parentName so subsequent
	// thread replies and status checks always see current values. Use the
	// authoritative setter so an empty WorktreePath clears any stale path
	// recorded by a previous worktreed run.
	_ = r.store.SetJobFolderWorktree(parentName, data.Folder, data.WorktreePath)
	_ = r.store.SetJobSessionID(parentName, data.SessionID)

	// A run is "slack-originated" when something on the Slack side recorded a
	// run mapping with channel+thread for it — either submitOneOffRaw (top
	// level message in a trigger channel) or submitFollowup (thread reply).
	// Such runs always post into that thread regardless of mirror rules; the
	// user's Slack action *was* the rule. Other runs (cron, CLI) need an
	// explicit mirror rule to opt in.
	slackOriginated := runMapping.Channel != "" && runMapping.ThreadTS != ""

	var (
		rule     *MirrorRule
		channel  string
		threadTS string
	)
	if slackOriginated {
		rule = &MirrorRule{OnFailure: true, IncludeStatus: true, AllowReplies: true}
		channel = runMapping.Channel
		threadTS = runMapping.ThreadTS
	} else {
		cfg := r.snapshotCfg()
		rule = cfg.MatchJob(parentName)
		if rule == nil {
			// No mirror rule AND no slack-originated run mapping. Usually
			// expected for cron jobs the user hasn't opted in. But when the
			// run *was* triggered from Slack (env.TriggeredBy starts with
			// "slack:") the mapping should have been there — surface that loudly
			// because it means state.json lost the LinkRun row (most often: two
			// bigband-slack daemons racing on the store).
			if strings.HasPrefix(env.TriggeredBy, "slack:") {
				log.Printf("bigband-slack: DROP completion for slack-triggered job=%s run=%s triggered_by=%s — no run mapping and no mirror rule. Likely cause: a second bigband-slack instance overwrote state.json. Check `ps -ef | grep bigband-slack`.", parentName, env.RunID, env.TriggeredBy)
			} else {
				log.Printf("bigband-slack: skip completion job=%s run=%s — no mirror rule and not slack-originated", parentName, env.RunID)
			}
			return
		}
		channel = rule.Channel
		if channel == "" {
			channel = cfg.Slack.DefaultChannel
		}
		if channel == "" {
			log.Printf("bigband-slack: no channel for job %s — skipping", parentName)
			return
		}
		threadTS = runMapping.ThreadTS // empty for first run of a configured job
	}

	if data.Status != "ok" && !rule.OnFailure {
		return
	}

	text := formatCompletion(env, data, rule)
	mode := "rule"
	if slackOriginated {
		mode = "slack-originated"
	}
	log.Printf("bigband-slack: posting completion job=%s parent=%s channel=%s thread=%q status=%s mode=%s msg_len=%d", env.JobName, parentName, channel, threadTS, data.Status, mode, len(data.FinalMessage))
	posted, err := r.slack.PostMessage(channel, text, threadTS)
	if err != nil {
		log.Printf("bigband-slack: PostMessage failed for job=%s channel=%q: %v", parentName, channel, err)
		if strings.Contains(err.Error(), "channel_not_found") {
			log.Printf("bigband-slack: hint — channel_not_found usually means the bot isn't a member of %s. In Slack, run `/invite @<your-app-name>` in that channel.", channel)
		}
		return
	}
	if posted == "" {
		posted = threadTS
	}
	log.Printf("bigband-slack: posted job=%s channel=%s thread=%s", parentName, channel, posted)
	if err := r.store.LinkRun(env.RunID, parentName, channel, posted, data.SessionID, rule.AllowReplies); err != nil {
		log.Printf("bigband-slack: persist mapping: %v", err)
	}
}

// formatCompletion renders the message text. Falls back to a "(no text)"
// placeholder when Claude finished on a tool call rather than a message.
func formatCompletion(_ bigbandext.Envelope, data bigbandext.JobRunCompletedData, rule *MirrorRule) string {
	body := strings.TrimSpace(data.FinalMessage)
	if body == "" {
		body = "_(no final message — run ended on a tool call)_"
	}
	if rule.IncludeStatus {
		dur := time.Duration(data.DurationMS) * time.Millisecond
		statusLine := fmt.Sprintf("`%s` in %s", data.Status, dur.Round(time.Second))
		return body + "\n" + statusLine
	}
	return body
}

// HandleSlackMessage processes one inbound Slack message. Returns true when
// the message was acted on, false when it was ignored. The caller is expected
// to ack the message regardless.
func (r *Router) HandleSlackMessage(msg SlackMessage) bool {
	channelLabel := msg.Channel
	if msg.ChannelName != "" {
		channelLabel = "#" + msg.ChannelName + "(" + msg.Channel + ")"
	}
	log.Printf("bigband-slack: inbound message channel=%s user=%s ts=%s thread=%q mention=%v len=%d", channelLabel, msg.User, msg.TS, msg.ThreadTS, msg.Mentioned, len(msg.Text))
	cfg := r.snapshotCfg()
	// Thread reply: route to follow-up if enabled and the mirror rule permits replies.
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		if !cfg.Threads.Enabled {
			log.Printf("bigband-slack: thread reply ignored — threads.enabled=false")
			return false
		}
		jobName, snap, ok := r.store.LookupThread(msg.ThreadTS)
		if !ok {
			log.Printf("bigband-slack: thread reply ignored — no mapping for thread=%s (was the parent run posted by this integration?)", msg.ThreadTS)
			return false
		}
		if !snap.AllowReplies {
			log.Printf("bigband-slack: thread reply ignored — allow_replies=false for job=%s", jobName)
			return false
		}
		log.Printf("bigband-slack: routing thread reply to followup parent=%s session=%s", jobName, snap.SessionID)
		return r.submitFollowup(jobName, snap, msg)
	}

	// Top-level message in a configured trigger channel.
	for i := range cfg.TriggerChannels {
		ch := &cfg.TriggerChannels[i]
		if !channelMatches(ch.Channel, msg.Channel, msg.ChannelName) {
			continue
		}
		if ch.RequireMention && !msg.Mentioned {
			return false
		}
		text := stripMention(msg.Text)
		// Try explicit commands first.
		for _, cmd := range ch.Commands {
			re, err := regexp.Compile(cmd.Match)
			if err != nil {
				log.Printf("bigband-slack: invalid command regex %q: %v", cmd.Match, err)
				continue
			}
			match := re.FindStringSubmatch(text)
			if match == nil {
				continue
			}
			r.runChannelCommand(ch, &cmd, re, match, msg)
			return true
		}
		if ch.AllowFreeformPrompt && text != "" {
			return r.submitOneOff(ch, text, msg)
		}
	}
	return false
}

func (r *Router) runChannelCommand(ch *TriggerChannel, cmd *TriggerCommand, re *regexp.Regexp, match []string, msg SlackMessage) {
	groups := namedGroups(re, match)
	switch cmd.Action {
	case "run":
		if name, ok := groups["job"]; ok {
			log.Printf("bigband-slack: inbound run job=%s channel=%s user=%s", name, msg.Channel, msg.User)
			if err := r.bb.Run(name); err != nil {
				r.ack(msg, fmt.Sprintf("❌ run %s failed: %v", name, err))
				return
			}
			r.react(msg, "eyes")
		}
	case "submit":
		folder := cmd.Folder
		if folder == "" {
			folder = ch.Folder
		}
		worktree := cmd.Worktree
		if worktree == nil {
			worktree = ch.Worktree
		}
		preExec := cmd.PreExec
		if preExec == nil {
			preExec = ch.PreExec
		}
		postExec := cmd.PostExec
		if postExec == nil {
			postExec = ch.PostExec
		}
		name := groups["name"]
		prompt := groups["prompt"]
		if prompt == "" {
			prompt = strings.TrimSpace(strings.Join(match[1:], " "))
		}
		if name == "" || prompt == "" {
			r.ack(msg, "❌ command requires both a name and a prompt")
			return
		}
		r.submitOneOffRaw(folder, prompt, name, worktree, preExec, postExec, msg)
	}
}

func (r *Router) submitOneOff(ch *TriggerChannel, prompt string, msg SlackMessage) bool {
	return r.submitOneOffRaw(ch.Folder, prompt, "", ch.Worktree, ch.PreExec, ch.PostExec, msg) != ""
}

// submitOneOffRaw submits a fresh ephemeral run. When the daemon replies with
// a run_id, we acknowledge in-thread and remember the thread → run mapping so
// the eventual completion event lands in the same thread. worktree, when
// non-nil, forces the daemon's per-run worktree setting (nil = inherit default).
// preExec / postExec are passed through verbatim; nil/empty means "no hooks".
func (r *Router) submitOneOffRaw(folder, prompt, name string, worktree *bool, preExec, postExec []string, msg SlackMessage) string {
	if folder == "" || prompt == "" {
		r.ack(msg, "❌ folder and prompt required")
		return ""
	}
	req := bigbandext.SubmitRunRequest{
		Name:        name,
		Folder:      folder,
		Prompt:      prompt,
		PreExec:     preExec,
		PostExec:    postExec,
		Worktree:    worktree,
		Ephemeral:   true,
		TriggeredBy: fmt.Sprintf("slack:msg:%s/%s", msg.Channel, msg.TS),
	}
	log.Printf("bigband-slack: inbound submit channel=%s user=%s folder=%s prompt_len=%d", msg.Channel, msg.User, folder, len(prompt))
	out, err := r.bb.Submit(req)
	if err != nil {
		log.Printf("bigband-slack: submit failed: %v", err)
		r.ack(msg, fmt.Sprintf("❌ submit failed: %v", err))
		return ""
	}
	log.Printf("bigband-slack: submitted job=%s run=%s", out.JobName, out.RunID)
	r.react(msg, "eyes")
	threadTS := msg.TS
	if err := r.store.LinkRun(out.RunID, out.JobName, msg.Channel, threadTS, "", true); err != nil {
		log.Printf("bigband-slack: WARN failed to persist thread mapping run=%s ts=%s channel=%s: %v — thread replies will not route", out.RunID, threadTS, msg.Channel, err)
	}
	_ = r.store.LinkJobMeta(out.JobName, folder, "")
	return out.RunID
}

func (r *Router) submitFollowup(jobName string, snap ThreadSnapshot, msg SlackMessage) bool {
	if snap.SessionID == "" {
		r.ack(msg, "❌ no session id known for this thread — cannot follow up")
		return false
	}
	folder := snap.Worktree
	if folder == "" {
		folder = snap.Folder
	}
	if folder == "" {
		r.ack(msg, "❌ no folder known for this thread — cannot follow up")
		return false
	}
	// The parent run defines the workspace: followups always run in the parent's
	// folder (either the worktree it created, or its plain folder if it had none)
	// and never create their own worktree on top.
	noWorktree := false
	req := bigbandext.SubmitRunRequest{
		Folder:          folder,
		Prompt:          msg.Text,
		ParentSessionID: snap.SessionID,
		Worktree:        &noWorktree,
		Ephemeral:       true,
		TriggeredBy:     fmt.Sprintf("slack:thread:%s", msg.ThreadTS),
	}
	log.Printf("bigband-slack: inbound followup parent=%s session=%s thread=%s prompt_len=%d", jobName, snap.SessionID, msg.ThreadTS, len(msg.Text))
	out, err := r.bb.Submit(req)
	if err != nil {
		log.Printf("bigband-slack: followup failed: %v", err)
		r.ack(msg, fmt.Sprintf("❌ followup failed: %v", err))
		return false
	}
	log.Printf("bigband-slack: followup submitted job=%s run=%s parent=%s", out.JobName, out.RunID, jobName)
	_ = r.store.LinkRun(out.RunID, jobName, msg.Channel, msg.ThreadTS, snap.SessionID, snap.AllowReplies)
	r.react(msg, "eyes")
	return true
}

func (r *Router) ack(msg SlackMessage, text string) {
	threadTS := msg.ThreadTS
	if threadTS == "" {
		threadTS = msg.TS
	}
	if _, err := r.slack.PostMessage(msg.Channel, text, threadTS); err != nil {
		log.Printf("bigband-slack: ack failed: %v", err)
	}
}

func (r *Router) react(msg SlackMessage, emoji string) {
	if err := r.slack.AddReaction(msg.Channel, emoji, msg.TS); err != nil {
		log.Printf("bigband-slack: react failed: %v", err)
	}
}

// SlackMessage is the slim representation the router needs from inbound
// messages. Defined here (not in slack.go) so tests can construct it.
//
// Channel is the channel ID Slack delivers (e.g. "C0B2QBTFLUU"). ChannelName
// is the human name resolved at receive time ("my-buddy"). Either may be
// empty if resolution failed.
type SlackMessage struct {
	Channel     string
	ChannelName string
	User        string
	Text        string
	TS          string
	ThreadTS    string
	Mentioned   bool
}

// channelMatches returns true when a config rule matches an inbound message.
// Users typically write the channel name in YAML, but Slack delivers events
// keyed by ID — so we accept either. Leading "#" on names is stripped.
func channelMatches(rule, channelID, channelName string) bool {
	rule = strings.TrimPrefix(rule, "#")
	channelName = strings.TrimPrefix(channelName, "#")
	if rule == "" {
		return false
	}
	return rule == channelID || rule == channelName
}

func stripMention(text string) string {
	// Naive strip: remove leading <@U...> tokens. Slack delivers mentions as
	// "<@USERID>" prefix in the text.
	for strings.HasPrefix(text, "<@") {
		end := strings.Index(text, ">")
		if end < 0 {
			break
		}
		text = strings.TrimSpace(text[end+1:])
	}
	return text
}

func namedGroups(re *regexp.Regexp, match []string) map[string]string {
	out := map[string]string{}
	for i, name := range re.SubexpNames() {
		if i == 0 || name == "" || i >= len(match) {
			continue
		}
		out[name] = match[i]
	}
	return out
}
