package core

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type providerBindingTestAgent struct {
	mu          sync.Mutex
	providers   []ProviderConfig
	defaultName string
	nextID      int
}

func newProviderBindingTestAgent(defaultName string) *providerBindingTestAgent {
	return &providerBindingTestAgent{defaultName: defaultName, providers: []ProviderConfig{{Name: "key-a"}, {Name: "key-b"}}}
}
func (*providerBindingTestAgent) Name() string { return "provider-binding-test" }
func (*providerBindingTestAgent) Stop() error  { return nil }
func (*providerBindingTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (*providerBindingTestAgent) StartSession(context.Context, string) (AgentSession, error) {
	return nil, fmt.Errorf("engine used the shared provider instead of a session binding")
}
func (a *providerBindingTestAgent) StartSessionWithProvider(_ context.Context, resumeID, name string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	found := name == ""
	for _, provider := range a.providers {
		found = found || provider.Name == name
	}
	if !found {
		return nil, fmt.Errorf("provider %q unavailable", name)
	}
	if resumeID == "" {
		a.nextID++
		resumeID = fmt.Sprintf("native-%s-%d", name, a.nextID)
	}
	session := newCUJAgentSession()
	session.reply = "API=" + name + " session=" + resumeID
	return &providerBindingTestSession{cujAgentSession: session, id: resumeID}, nil
}
func (a *providerBindingTestAgent) SetProviders(providers []ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = append([]ProviderConfig(nil), providers...)
}
func (a *providerBindingTestAgent) ListProviders() []ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ProviderConfig(nil), a.providers...)
}
func (a *providerBindingTestAgent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.defaultName = name
	return true
}
func (a *providerBindingTestAgent) GetActiveProvider() *ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.defaultName == "" {
		return nil
	}
	return &ProviderConfig{Name: a.defaultName}
}

type providerBindingTestSession struct {
	*cujAgentSession
	id string
}

func (s *providerBindingTestSession) CurrentSessionID() string { return s.id }

func TestSelectProvider_ProtectsStartedConversation(t *testing.T) {
	for _, state := range []string{"history", "native-id", "busy"} {
		t.Run(state, func(t *testing.T) {
			s := &Session{}
			if !s.SelectProvider("key-a") {
				t.Fatal("initial selection failed")
			}
			switch state {
			case "history":
				s.AddHistory("user", "keep my work")
			case "native-id":
				s.SetAgentSessionID("native-original", "test")
			case "busy":
				s.TryLock()
			}
			if s.SelectProvider("key-b") || s.SelectProvider("") {
				t.Fatal("changed a started conversation's account")
			}
			if !s.SelectProvider("key-a") {
				t.Fatal("reselecting the same account should be harmless")
			}
		})
	}
}

func TestSessionProvider_DefaultAndLegacyBindings(t *testing.T) {
	for _, tc := range []struct {
		name, initial, native, want string
		bound                       bool
	}{
		{name: "new snapshots default", want: "key-a"},
		{name: "explicit CLI default", bound: true},
		{name: "legacy native keeps CLI routing", native: "old-native"},
		{name: "legacy named provider", initial: "key-b", native: "old-native", want: "key-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewSessionManager(filepath.Join(t.TempDir(), "sessions.json"))
			s := sm.GetOrCreateActive("test:topic")
			s.ActiveProvider, s.ProviderBound, s.AgentSessionID = tc.initial, tc.bound, tc.native
			if got, _ := s.BindDefaultProvider("key-a"); got != tc.want {
				t.Fatalf("provider=%q, want %q", got, tc.want)
			}
			sm.Save()
			reloaded := NewSessionManager(sm.StorePath()).GetOrCreateActive("test:topic")
			if got, _ := reloaded.BindDefaultProvider("key-b"); got != tc.want {
				t.Fatalf("restart changed provider to %q", got)
			}
		})
	}
}

func TestProviderCard_BindsOnlyItsConversationAndPreservesHistory(t *testing.T) {
	a := newProviderBindingTestAgent("key-a")
	e := NewEngine("test", a, nil, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	card := e.handleCardNav("act:/provider key-b", "test:topic")
	if card == nil || !strings.Contains(card.RenderText(), "bound to **key-b**") {
		t.Fatalf("binding card: %+v", card)
	}
	s := e.sessions.GetOrCreateActive("test:topic")
	s.AddHistory("user", "preserve this")
	s.SetAgentSessionID("original", a.Name())
	card = e.handleCardNav("act:/provider key-a", "test:topic")
	if !strings.Contains(card.RenderText(), "already started") {
		t.Fatalf("missing binding guard: %s", card.RenderText())
	}
	if s.GetAgentSessionID() != "original" || s.HistoryLen() != 1 {
		t.Fatal("card erased conversation")
	}
	if a.GetActiveProvider().Name != "key-a" {
		t.Fatal("card changed project default")
	}
	if got := e.renderProviderCard("test:topic").RenderText(); !strings.Contains(got, "key-b") {
		t.Fatalf("wrong current provider: %s", got)
	}
}

func TestProjectProviderDefault_PreservesBoundConversations(t *testing.T) {
	a := newProviderBindingTestAgent("key-a")
	e := NewEngine("test", a, nil, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	s := e.sessions.GetOrCreateActive("test:topic")
	s.SelectProvider("key-a")
	s.AddHistory("user", "original history")
	s.SetAgentSessionID("original-native", a.Name())
	// The upstream persist-before-activate rule must also apply when native
	// sessions have independent provider bindings.
	e.providerSaveFunc = func(string) error { return fmt.Errorf("configuration is read-only") }
	failed := httptest.NewRecorder()
	(&ManagementServer{}).handleProjectProviders(failed, httptest.NewRequest("POST", "/providers/key-b/activate", nil), e, "key-b/activate")
	if failed.Code != 500 || a.GetActiveProvider().Name != "key-a" {
		t.Fatal("failed persistence changed the default provider")
	}
	if s.GetAgentSessionID() != "original-native" || s.HistoryLen() != 1 || s.GetActiveProvider() != "key-a" {
		t.Fatal("failed persistence changed the existing conversation")
	}
	e.providerSaveFunc = nil
	r := httptest.NewRequest("POST", "/providers/key-b/activate", nil)
	w := httptest.NewRecorder()
	(&ManagementServer{}).handleProjectProviders(w, r, e, "key-b/activate")
	if w.Code != 200 {
		t.Fatalf("activate: %d", w.Code)
	}
	if s.GetAgentSessionID() != "original-native" || s.HistoryLen() != 1 || s.GetActiveProvider() != "key-a" {
		t.Fatal("project default change reset an existing binding")
	}
}

func TestTimerJob_UsesConversationProviderBinding(t *testing.T) {
	p := &cujReplyCtxPlatform{&stubPlatformEngine{n: "test"}}
	a := newProviderBindingTestAgent("key-b")
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Platform: "test", UserID: "user", Content: "/provider switch key-a", ReplyCtx: "ctx"})
	p.clearSent()
	if err := e.ExecuteTimerJob(&TimerJob{ID: "timer-a", Project: "test", SessionKey: "test:topic", Prompt: "check progress"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "API=key-a") {
		t.Fatalf("timer used wrong provider: %s", got)
	}
}
