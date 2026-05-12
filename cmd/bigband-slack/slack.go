package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// slackClient implements the Slack interface in router.go using slack-go/slack.
// PostMessage opens a new thread when threadTS is "", and returns the message
// timestamp so callers can persist the thread mapping.
//
// channelNames caches channel ID → name lookups so config rules written with
// human channel names (the common case) match incoming messages, which Slack
// delivers keyed by ID. Lazily populated; one Slack API call per unique
// channel seen.
type slackClient struct {
	api    *slack.Client
	botUID string

	chMu         sync.Mutex
	channelNames map[string]string
	failureCache map[string]time.Time // channel id → time of last failed lookup
}

func newSlackClient(cfg *Config) (*slackClient, *socketmode.Client, error) {
	app := cfg.Slack.ResolvedAppToken()
	bot := cfg.Slack.ResolvedBotToken()
	if app == "" || bot == "" {
		return nil, nil, errors.New("slack.app_token and slack.bot_token are required")
	}
	api := slack.New(
		bot,
		slack.OptionAppLevelToken(app),
	)
	auth, err := api.AuthTest()
	if err != nil {
		return nil, nil, fmt.Errorf("slack auth: %w", err)
	}
	sm := socketmode.New(api)
	return &slackClient{api: api, botUID: auth.UserID, channelNames: map[string]string{}, failureCache: map[string]time.Time{}}, sm, nil
}

// channelName returns the name of the given channel ID, hitting Slack's
// conversations.info endpoint on cache miss. Returns "" when lookup fails
// (insufficient scopes, archived, DM, etc.) — callers fall back to ID match.
// Repeated failures are suppressed for 5 minutes via a failure cache so a
// bad channel ID doesn't hammer the Slack API.
func (c *slackClient) channelName(id string) string {
	if id == "" {
		return ""
	}
	c.chMu.Lock()
	if n, ok := c.channelNames[id]; ok {
		c.chMu.Unlock()
		return n
	}
	if t, ok := c.failureCache[id]; ok && time.Since(t) < 5*time.Minute {
		c.chMu.Unlock()
		return ""
	}
	c.chMu.Unlock()
	info, err := c.api.GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: id})
	if err != nil {
		log.Printf("bigband-slack: cannot resolve channel %s: %v", id, err)
		c.chMu.Lock()
		c.failureCache[id] = time.Now()
		c.chMu.Unlock()
		return ""
	}
	c.chMu.Lock()
	c.channelNames[id] = info.Name
	delete(c.failureCache, id)
	c.chMu.Unlock()
	return info.Name
}

func (c *slackClient) PostMessage(channel, text, threadTS string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := c.api.PostMessage(channel, opts...)
	if err != nil {
		return "", err
	}
	return ts, nil
}

// runSocketMode blocks running the Slack socket-mode loop. Each Slack message
// event is decoded into a SlackMessage and passed to the Router. The function
// returns when ctx is cancelled or on non-recoverable error.
func runSocketMode(ctx context.Context, _ *Config, sm *socketmode.Client, sc *slackClient, router *Router) error {
	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				log.Println("bigband-slack: connecting to slack...")
			case socketmode.EventTypeConnected:
				log.Println("bigband-slack: connected to slack")
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				sm.Ack(*evt.Request)
				if eventsAPIEvent.Type != slackevents.CallbackEvent {
					continue
				}
				switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
				case *slackevents.MessageEvent:
					if ev.SubType != "" || ev.BotID != "" {
						continue // ignore edits, joins, bot-loops
					}
					msg := SlackMessage{
						Channel:     ev.Channel,
						ChannelName: sc.channelName(ev.Channel),
						User:        ev.User,
						Text:        ev.Text,
						TS:          ev.TimeStamp,
						ThreadTS:    ev.ThreadTimeStamp,
						Mentioned:   containsMention(ev.Text, sc.botUID),
					}
					if !router.HandleSlackMessage(msg) {
						label := ev.Channel
						if msg.ChannelName != "" {
							label = "#" + msg.ChannelName + " (" + ev.Channel + ")"
						}
						log.Printf("bigband-slack: ignored message in %s", label)
					}
				case *slackevents.AppMentionEvent:
					msg := SlackMessage{
						Channel:     ev.Channel,
						ChannelName: sc.channelName(ev.Channel),
						User:        ev.User,
						Text:        ev.Text,
						TS:          ev.TimeStamp,
						ThreadTS:    ev.ThreadTimeStamp,
						Mentioned:   true,
					}
					router.HandleSlackMessage(msg)
				}
			default:
				// Other event types (slash commands, interactivity) ignored in v1.
			}
		}
	}()
	return sm.RunContext(ctx)
}

func containsMention(text, uid string) bool {
	if uid == "" {
		return false
	}
	return strings.Contains(text, "<@"+uid+">")
}
