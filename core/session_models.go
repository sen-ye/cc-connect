package core

import "context"

// sessionAvailableModels keeps menus and selection indices aligned with an
// isolated conversation's provider. Providers without an explicit catalog retain
// the agent's existing model discovery and fallback behavior.
func sessionAvailableModels(ctx context.Context, agent Agent, switcher ModelSwitcher, sessions *SessionManager, sessionKey string) []ModelOption {
	if _, scoped := agent.(SessionProviderStarter); scoped {
		if providers, ok := agent.(ProviderSwitcher); ok {
			var session *Session
			if sessions != nil && sessionKey != "" {
				session = sessions.GetOrCreateActive(sessionKey)
			}
			if provider := providerForSession(providers, session); provider != nil && len(provider.Models) > 0 {
				return append([]ModelOption(nil), provider.Models...)
			}
		}
	}
	return switcher.AvailableModels(ctx)
}
