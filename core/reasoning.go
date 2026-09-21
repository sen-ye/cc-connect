package core

import (
	"log/slog"
	"strings"
)

// Persist the same scope as the agent receiving the setting. Workspace agents
// are discarded by the idle reaper, so retaining only their in-memory value
// loses the selection even when the underlying conversation is resumed.
func (e *Engine) persistReasoningEffort(agent Agent, interactiveKey, sessionKey, effort string) {
	if e.projectState == nil {
		return
	}
	workspace := ""
	if e.multiWorkspace && agent != e.agent {
		workspace = workspaceModelOverrideKey(interactiveKey, sessionKey, agent)
		if workspace == "" {
			slog.Warn("reasoning: cannot persist selection without workspace", "session_key", sessionKey)
			return
		}
	}
	e.projectState.SetReasoningEffortOverride(workspace, effort)
	e.projectState.Save()
}

func (e *Engine) restoreReasoningEffort(agent Agent, workspace string) {
	if e.projectState == nil {
		return
	}
	switcher, ok := agent.(ReasoningEffortSwitcher)
	if !ok {
		return
	}
	effort := e.projectState.ReasoningEffortOverride(workspace)
	if effort == "" {
		return
	}
	for _, available := range switcher.AvailableReasoningEfforts() {
		if strings.EqualFold(available, effort) {
			switcher.SetReasoningEffort(available)
			return
		}
	}
	slog.Warn("reasoning: saved effort is not supported by agent", "workspace", workspace, "effort", effort)
}
