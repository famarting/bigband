package events

import "github.com/famarting/bigband/pkg/bigbandext"

// Payload structs are sourced from pkg/bigbandext so external integrations
// and the daemon decode the same shapes. Aliasing keeps internal call sites
// (e.g. `events.MustData(events.TaskRunCompletedData{...})`) unchanged.
type (
	TaskRunStartedData       = bigbandext.TaskRunStartedData
	TaskRunWorktreeReadyData = bigbandext.TaskRunWorktreeReadyData
	ClaudeSessionStartedData = bigbandext.ClaudeSessionStartedData
	ClaudeTurnCompletedData  = bigbandext.ClaudeTurnCompletedData
	ClaudeWakeupData         = bigbandext.ClaudeWakeupData
	TaskRunCompletedData     = bigbandext.TaskRunCompletedData
	TaskRunPreFailedData     = bigbandext.TaskRunPreFailedData
	ExtensionStartedData     = bigbandext.ExtensionStartedData
	ExtensionExitedData      = bigbandext.ExtensionExitedData
	ExtensionFailedData      = bigbandext.ExtensionFailedData
)
