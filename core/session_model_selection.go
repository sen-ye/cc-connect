package core

import "fmt"

func (s *Session) GetModelOverride() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ModelOverride
}

func sessionModelName(agent Agent, session *Session) string {
	if models, ok := agent.(SessionModelStarter); ok && session != nil {
		if model := session.GetModelOverride(); model != "" {
			return model
		}
		provider := ""
		if switcher, ok := agent.(ProviderSwitcher); ok {
			if selected := providerForSession(switcher, session); selected != nil {
				provider = selected.Name
			}
		}
		return models.ModelForProvider(provider)
	}
	if models, ok := agent.(ModelSwitcher); ok {
		return models.GetModel()
	}
	return ""
}

func sessionReasoningEfforts(agent Agent, session *Session) []string {
	if models, ok := agent.(ModelReasoningEffortProvider); ok && session != nil {
		return models.ReasoningEffortsForModel(sessionModelName(agent, session))
	}
	if reasoning, ok := agent.(ReasoningEffortSwitcher); ok {
		return reasoning.AvailableReasoningEfforts()
	}
	return nil
}

// Persist before reporting success or closing a native session. The manager
// lock keeps concurrent snapshots from saving a tentative selection.
func (e *Engine) switchSessionModel(agent Agent, sessions *SessionManager, key, target string) (string, error) {
	if _, ok := agent.(SessionModelStarter); !ok {
		return e.switchModelOnAgent(agent, target, agent == e.agent)
	}
	session := sessions.GetOrCreateActive(key)
	session.settingsMu.Lock()
	defer session.settingsMu.Unlock()
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	session.mu.Lock()
	previous := session.ModelOverride
	session.ModelOverride = target
	session.mu.Unlock()
	if err := sessions.saveLockedWithError(); err != nil {
		session.mu.Lock()
		session.ModelOverride = previous
		session.mu.Unlock()
		return "", fmt.Errorf("save conversation model: %w", err)
	}
	return target, nil
}
