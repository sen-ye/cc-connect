package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AgentFailureKind describes a turn failure independently of its CLI or platform.
type AgentFailureKind string

const (
	FailureProvider   AgentFailureKind = "provider"
	FailureStalled    AgentFailureKind = "stalled"
	FailureConnection AgentFailureKind = "connection"
	FailureNoProgress AgentFailureKind = "no_progress"
	FailureRateLimit  AgentFailureKind = "rate_limit"
	FailureAuth       AgentFailureKind = "authentication"
	FailureCancelled  AgentFailureKind = "cancelled"
	FailureProcess    AgentFailureKind = "process"
)

// AgentFailure contains sanitized diagnostics, never credentials or raw logs.
// Retryable describes whether a user can retry, not whether the CLI is retrying.
type AgentFailure struct {
	Kind            AgentFailureKind
	Detail          string
	Model           string
	ProviderStatus  string
	RequestID       string
	RetryCountKnown bool
	RetryCount      int
	Retryable       *bool
	RetryAfter      string
}

func (f *AgentFailure) Error() string {
	if f.Detail != "" {
		return f.Detail
	}
	return "agent turn failed: " + string(f.Kind)
}

// AgentRetry is progress for an automatic recovery attempt, never a turn result.
type AgentRetry struct {
	Attempt   int
	RequestID string
}

func (e *Engine) agentDisplayName() string {
	if info, ok := e.agent.(AgentDoctorInfo); ok {
		return info.CLIDisplayName()
	}
	return e.agent.Name()
}

func (e *Engine) agentErrorMessage(err error, sessionID string) string {
	// Keep existing recovery advice when an adapter adds structured metadata
	// around an error that already had a dedicated user-facing handler.
	if err != nil {
		for _, h := range agentErrorHandlers {
			if strings.Contains(err.Error(), h.contains) {
				return e.i18n.T(h.msgKey)
			}
		}
	}
	var failure *AgentFailure
	if !errors.As(err, &failure) || failure == nil {
		return fmt.Sprintf(e.i18n.T(MsgError), err)
	}
	key := MsgAgentFailureProcess
	switch failure.Kind {
	case FailureProvider:
		key = MsgAgentFailureProvider
	case FailureConnection:
		key = MsgAgentFailureConnection
	case FailureStalled:
		key = MsgAgentFailureStalled
	case FailureNoProgress:
		key = MsgAgentFailureNoProgress
	case FailureRateLimit:
		key = MsgAgentFailureRateLimit
	case FailureAuth:
		key = MsgAgentFailureAuth
	case FailureCancelled:
		return e.i18n.Tf(MsgAgentFailureCancelled, e.agentDisplayName())
	}
	value := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	retries := "—"
	if failure.RetryCountKnown || failure.RetryCount > 0 {
		retries = strconv.Itoa(failure.RetryCount)
	}
	message := e.i18n.Tf(key, e.agentDisplayName())
	message += "\n" + e.i18n.Tf(MsgAgentFailureContext, value(failure.Model), value(sessionID), retries, value(failure.ProviderStatus), value(failure.RequestID))
	if failure.RetryAfter != "" {
		message += "\n" + e.i18n.Tf(MsgAgentFailureRetryAfter, failure.RetryAfter)
	}
	if failure.Detail != "" {
		message += "\n" + e.i18n.Tf(MsgAgentFailureDetail, failure.Detail)
	}
	return message
}
