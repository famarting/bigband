package events

import "github.com/famarting/bigband/pkg/bigbandext"

// Payload structs are sourced from pkg/bigbandext so external integrations
// and the daemon decode the same shapes. Aliasing keeps internal call sites
// (e.g. `events.MustData(events.JobRunCompletedData{...})`) unchanged.
type (
	JobRunStartedData        = bigbandext.JobRunStartedData
	JobRunWorktreeReadyData  = bigbandext.JobRunWorktreeReadyData
	ClaudeSessionStartedData = bigbandext.ClaudeSessionStartedData
	ClaudeTurnCompletedData  = bigbandext.ClaudeTurnCompletedData
	ClaudeWakeupData         = bigbandext.ClaudeWakeupData
	JobRunCompletedData      = bigbandext.JobRunCompletedData
	JobRunPreFailedData      = bigbandext.JobRunPreFailedData
	ExtensionStartedData     = bigbandext.ExtensionStartedData
	ExtensionExitedData      = bigbandext.ExtensionExitedData
	ExtensionFailedData      = bigbandext.ExtensionFailedData
)
