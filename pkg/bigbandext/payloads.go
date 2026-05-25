package bigbandext

// Typed payload shapes for Envelope.Data. One struct per event type. All
// optional fields use omitempty so events.jsonl stays compact and additive
// schema changes don't break older consumers.
//
// To decode an envelope's Data:
//
//	var d bigbandext.JobRunCompletedData
//	if err := json.Unmarshal(env.Data, &d); err != nil { ... }

// JobRunStartedData accompanies TypeJobRunStarted.
type JobRunStartedData struct {
	Folder     string `json:"folder,omitempty"`
	Schedule   string `json:"schedule,omitempty"`
	OneOff     bool   `json:"one_off,omitempty"`
	PromptHash string `json:"prompt_hash,omitempty"` // sha256 prefix; reserved
	Worktree   bool   `json:"worktree,omitempty"`
	Resume     bool   `json:"resume,omitempty"`      // true when ParentSessionID was set
	ResumeFrom string `json:"resume_from,omitempty"` // session id we resumed
	Ephemeral  bool   `json:"ephemeral,omitempty"`
}

// JobRunWorktreeReadyData accompanies TypeJobRunWorktreeReady.
type JobRunWorktreeReadyData struct {
	WorktreePath string `json:"worktree_path"`
	RunDir       string `json:"run_dir,omitempty"`
}

// ClaudeSessionStartedData accompanies TypeClaudeSessionStarted.
type ClaudeSessionStartedData struct {
	SessionID string `json:"session_id"`
}

// ClaudeTurnCompletedData accompanies TypeClaudeTurnCompleted.
type ClaudeTurnCompletedData struct {
	Subtype      string  `json:"subtype,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	DurationMS   int     `json:"duration_ms,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	FinalMessage string  `json:"final_message,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`
}

// ClaudeWakeupData accompanies TypeClaudeWakeup.
type ClaudeWakeupData struct {
	DelaySeconds int    `json:"delay_seconds"`
	Prompt       string `json:"prompt,omitempty"`
}

// JobRunCompletedData accompanies TypeJobRunCompleted. Folder and
// WorktreePath together let an integration follow up on the run without
// re-querying state — see cmd/bigband-slack for a worked example.
type JobRunCompletedData struct {
	Status       string `json:"status"`
	FinalMessage string `json:"final_message,omitempty"`
	LogPath      string `json:"log_path,omitempty"`
	ReplyFile    string `json:"reply_file,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Folder       string `json:"folder,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
}

// JobRunPreFailedData accompanies TypeJobRunPreFailed.
type JobRunPreFailedData struct {
	Command string `json:"command,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ExtensionStartedData accompanies TypeExtensionStarted.
type ExtensionStartedData struct {
	Name    string   `json:"name"`
	PID     int      `json:"pid"`
	Command []string `json:"command,omitempty"`
}

// ExtensionExitedData accompanies TypeExtensionExited.
type ExtensionExitedData struct {
	Name        string `json:"name"`
	PID         int    `json:"pid,omitempty"`
	ExitCode    int    `json:"exit_code"`
	Signal      string `json:"signal,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	WillRestart bool   `json:"will_restart,omitempty"`
}

// ExtensionFailedData accompanies TypeExtensionFailed. Emitted when the
// supervisor circuit-breaks (too many consecutive failures), or when a
// manifest is invalid / cannot be parsed.
type ExtensionFailedData struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// ConfigReloadedData accompanies TypeConfigReloaded. The counts are a quick
// observability signal for subscribers; the canonical config remains
// ~/.bigband/config.yaml.
type ConfigReloadedData struct {
	JobCount        int `json:"job_count"`
	ScheduledCount  int `json:"scheduled_count"`
	OneOffCount     int `json:"one_off_count"`
	DisabledCount   int `json:"disabled_count"`
	TemplatesCount  int `json:"templates_count,omitempty"`
}
