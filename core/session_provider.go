package core

import (
	"context"
	"fmt"
)

// startAgentSession resolves a binding for this conversation, not a mutable
// agent-wide selection. Timer jobs use the same path as ordinary messages.
func startAgentSession(ctx context.Context, agent Agent, session *Session, sessions *SessionManager, resumeID string) (AgentSession, error) {
	session.settingsMu.Lock()
	defer session.settingsMu.Unlock()
	var started AgentSession
	var err error
	if starter, ok := agent.(SessionProviderStarter); ok {
		defaultName := ""
		if switcher, ok := agent.(ProviderSwitcher); ok {
			if provider := switcher.GetActiveProvider(); provider != nil {
				defaultName = provider.Name
			}
		}
		name, changed := session.BindDefaultProvider(defaultName)
		if changed {
			sessions.Save()
		}
		started, err = starter.StartSessionWithProvider(ctx, resumeID, name)
	} else {
		restoreActiveProviderFromSession(agent, session)
		started, err = agent.StartSession(ctx, resumeID)
	}
	if err != nil {
		return nil, err
	}
	if err := restoreSessionServiceTier(started, session.GetServiceTier()); err != nil {
		if closeErr := started.Close(); closeErr != nil {
			return nil, fmt.Errorf("restore conversation service tier: %w; close session: %v", err, closeErr)
		}
		return nil, fmt.Errorf("restore conversation service tier: %w", err)
	}
	return started, nil
}

func providerForSession(switcher ProviderSwitcher, session *Session) *ProviderConfig {
	if _, ok := switcher.(SessionProviderStarter); !ok || session == nil {
		return switcher.GetActiveProvider()
	}
	name, bound := session.ProviderBinding()
	if !bound && name == "" && session.GetAgentSessionID() == "" {
		return switcher.GetActiveProvider()
	}
	if name == "" {
		return nil
	}
	for _, provider := range switcher.ListProviders() {
		if provider.Name == name {
			return &provider
		}
	}
	// Keep the saved alias visible even when its configuration was removed.
	return &ProviderConfig{Name: name}
}

// selectSessionProvider handles agents that support isolated account bindings.
// It never changes the project default, tears down a running task, or clears
// conversation history. Both commands and card actions use this operation.
func (e *Engine) selectSessionProvider(switcher ProviderSwitcher, sessions *SessionManager, sessionKey, name string) (string, bool) {
	if _, ok := switcher.(SessionProviderStarter); !ok {
		return "", false
	}
	if name != "" {
		found := false
		for _, provider := range switcher.ListProviders() {
			if provider.Name == name {
				found = true
				break
			}
		}
		if !found {
			return e.i18n.Tf(MsgProviderNotFound, name), true
		}
	}
	session := sessions.GetOrCreateActive(sessionKey)
	if !session.SelectProvider(name) {
		return e.i18n.T(MsgSessionProviderLocked), true
	}
	sessions.Save()
	if name == "" {
		return e.i18n.T(MsgSessionProviderDefault), true
	}
	return e.i18n.Tf(MsgSessionProviderSelected, name), true
}
