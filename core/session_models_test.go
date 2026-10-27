package core

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sessionModelMenuTestAgent struct {
	*providerBindingTestAgent
	model string
}

func newSessionModelMenuTestAgent() *sessionModelMenuTestAgent {
	a := &sessionModelMenuTestAgent{providerBindingTestAgent: newProviderBindingTestAgent("key-a")}
	a.SetProviders([]ProviderConfig{
		{Name: "key-a", Models: []ModelOption{{Name: "a-small", Alias: "small"}, {Name: "a-large", Alias: "large"}}},
		{Name: "key-b", Models: []ModelOption{{Name: "b-large", Alias: "large"}, {Name: "b-small", Alias: "small"}}},
	})
	return a
}

func (a *sessionModelMenuTestAgent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
}

func (a *sessionModelMenuTestAgent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (*sessionModelMenuTestAgent) AvailableModels(context.Context) []ModelOption {
	return []ModelOption{{Name: "catalog-default", Alias: "catalog"}}
}

func (a *sessionModelMenuTestAgent) GetActiveProvider() *ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, provider := range a.providers {
		if provider.Name == a.defaultName {
			return &provider
		}
	}
	return nil
}

func TestCmdModel_UsesBoundProviderMenuAndSelection(t *testing.T) {
	a := newSessionModelMenuTestAgent()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	for _, tc := range []struct{ key, provider, first, aliasTarget string }{
		{"test:a", "key-a", "a-small", "a-large"},
		{"test:b", "key-b", "b-large", "b-large"},
	} {
		s := e.sessions.GetOrCreateActive(tc.key)
		s.SelectProvider(tc.provider)
		s.SetAgentSessionID("native-"+tc.provider, a.Name())
		send := func(content string) string {
			p.clearSent()
			e.ReceiveMessage(p, &Message{SessionKey: tc.key, Platform: "test", UserID: tc.provider, Content: content, ReplyCtx: "ctx"})
			return strings.Join(p.getSent(), "\n")
		}
		a.SetModel("")
		menu := send("/model")
		if !strings.Contains(menu, tc.first) || strings.Contains(menu, "catalog-default") {
			t.Fatalf("%s menu used wrong catalog: %s", tc.key, menu)
		}
		other := "b-"
		if tc.provider == "key-b" {
			other = "a-"
		}
		if strings.Contains(menu, other) {
			t.Fatalf("%s menu leaked another provider: %s", tc.key, menu)
		}
		if reply := send("/model switch 1"); !strings.Contains(reply, tc.first) || a.GetModel() != tc.first {
			t.Fatalf("%s numeric selection: reply=%s model=%s", tc.key, reply, a.GetModel())
		}
		if reply := send("/model switch LARGE"); !strings.Contains(reply, tc.aliasTarget) || a.GetModel() != tc.aliasTarget {
			t.Fatalf("%s alias selection: reply=%s model=%s", tc.key, reply, a.GetModel())
		}
		if s.GetActiveProvider() != tc.provider || s.GetAgentSessionID() != "native-"+tc.provider {
			t.Fatal("model menu selection changed the conversation binding")
		}
	}
	if a.GetActiveProvider().Name != "key-a" {
		t.Fatal("model menu selection changed the project provider")
	}
}

func TestModelCard_BoundProviderOptionsAndCallbacks(t *testing.T) {
	a := newSessionModelMenuTestAgent()
	p := &stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	for _, tc := range []struct{ key, provider, first, aliasTarget string }{
		{"test:a", "key-a", "a-small", "a-large"},
		{"test:b", "key-b", "b-large", "b-large"},
	} {
		e.sessions.GetOrCreateActive(tc.key).SelectProvider(tc.provider)
		card := e.renderModelCard(tc.key)
		var options []CardSelectOption
		for _, element := range card.Elements {
			if selectMenu, ok := element.(CardSelect); ok {
				options = selectMenu.Options
			}
		}
		if len(options) != 2 || !strings.Contains(options[0].Text, tc.first) {
			t.Fatalf("%s options = %+v", tc.key, options)
		}
		result := e.handleCardNav(options[0].Value, tc.key)
		if result == nil || !strings.Contains(result.RenderText(), tc.first) || a.GetModel() != tc.first {
			t.Fatalf("%s numeric card callback: card=%+v model=%s", tc.key, result, a.GetModel())
		}
		result = e.handleCardNav("act:/model switch LARGE", tc.key)
		if result == nil || !strings.Contains(result.RenderText(), tc.aliasTarget) || a.GetModel() != tc.aliasTarget {
			t.Fatalf("%s alias card callback: card=%+v model=%s", tc.key, result, a.GetModel())
		}
		before := len(p.getRefreshedCards())
		e.executeCardAction("/model", "switch 1", tc.key)
		deadline := time.Now().Add(3 * time.Second)
		for len(p.getRefreshedCards()) == before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		refreshed := p.getRefreshedCards()
		if len(refreshed) != before+1 || !strings.Contains(refreshed[before].RenderText(), tc.first) || a.GetModel() != tc.first {
			t.Fatalf("%s async card callback did not select %s", tc.key, tc.first)
		}
	}
}

func TestSessionAvailableModels_PreservesFallbackAndProviderConfig(t *testing.T) {
	t.Run("explicit catalog is copied", func(t *testing.T) {
		a := newSessionModelMenuTestAgent()
		sm := NewSessionManager("")
		sm.GetOrCreateActive("test:a").SelectProvider("key-a")
		models := sessionAvailableModels(context.Background(), a, a, sm, "test:a")
		models[0].Name = "changed"
		if a.ListProviders()[0].Models[0].Name != "a-small" {
			t.Fatal("caller changed configured model catalog")
		}
	})
	t.Run("new conversation uses default provider catalog", func(t *testing.T) {
		a := newSessionModelMenuTestAgent()
		sm := NewSessionManager("")
		models := sessionAvailableModels(context.Background(), a, a, sm, "test:new")
		if !reflect.DeepEqual(models, a.ListProviders()[0].Models) {
			t.Fatalf("new conversation models = %+v", models)
		}
		if _, bound := sm.GetOrCreateActive("test:new").ProviderBinding(); bound {
			t.Fatal("opening a model menu locked provider selection")
		}
	})
	for _, state := range []string{"empty catalog", "CLI default", "legacy native", "missing provider"} {
		t.Run(state, func(t *testing.T) {
			a := newSessionModelMenuTestAgent()
			sm := NewSessionManager("")
			s := sm.GetOrCreateActive("test:topic")
			switch state {
			case "empty catalog":
				a.SetProviders([]ProviderConfig{{Name: "key-b"}})
				s.SelectProvider("key-b")
			case "CLI default":
				s.SelectProvider("")
			case "legacy native":
				s.SetAgentSessionID("old-native", a.Name())
			case "missing provider":
				s.SelectProvider("removed")
			}
			models := sessionAvailableModels(context.Background(), a, a, sm, "test:topic")
			if !reflect.DeepEqual(models, a.AvailableModels(context.Background())) {
				t.Fatalf("fallback models = %+v", models)
			}
		})
	}
	t.Run("agent without isolated providers keeps discovery", func(t *testing.T) {
		a := &stubStrictModelAgent{
			stubModelModeAgent: stubModelModeAgent{active: "key-a", providers: []ProviderConfig{{Name: "key-a", Models: []ModelOption{{Name: "explicit"}}}}},
			models:             []ModelOption{{Name: "discovered"}},
		}
		models := sessionAvailableModels(context.Background(), a, a, NewSessionManager(""), "test:topic")
		if !reflect.DeepEqual(models, a.models) || a.calls != 1 {
			t.Fatalf("non-isolated agent models = %+v, calls = %d", models, a.calls)
		}
	})
}
