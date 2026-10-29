package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type isolatedModelTestAgent struct {
	*sessionModelMenuTestAgent
	effort string
}

func newIsolatedModelTestAgent() *isolatedModelTestAgent {
	a := &isolatedModelTestAgent{sessionModelMenuTestAgent: newSessionModelMenuTestAgent()}
	a.SetModel("global-default")
	a.SetProviders([]ProviderConfig{
		{Name: "key-a", Model: "gpt-6-astra", Models: []ModelOption{{Name: "gpt-6.1-sol", Alias: "sol"}}},
		{Name: "key-b", Model: "other-provider-default"},
	})
	return a
}

func (*isolatedModelTestAgent) SupportsSessionServiceTier() bool  { return true }
func (*isolatedModelTestAgent) DefaultSessionServiceTier() string { return "default" }
func (a *isolatedModelTestAgent) ModelForProvider(name string) string {
	for _, p := range a.ListProviders() {
		if p.Name == name {
			return p.Model
		}
	}
	return a.GetModel()
}
func (a *isolatedModelTestAgent) StartSessionWithModel(ctx context.Context, id, provider, model string) (AgentSession, error) {
	base, err := a.StartSessionWithProvider(ctx, id, provider)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = a.ModelForProvider(provider)
	}
	return &isolatedModelTestSession{
		serviceTierTestSession: &serviceTierTestSession{providerBindingTestSession: base.(*providerBindingTestSession), tier: "fast"},
		model:                  model,
	}, nil
}
func (a *isolatedModelTestAgent) SetReasoningEffort(effort string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.effort = effort
}
func (a *isolatedModelTestAgent) GetReasoningEffort() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.effort
}
func (*isolatedModelTestAgent) AvailableReasoningEfforts() []string { return []string{"high"} }
func (*isolatedModelTestAgent) ReasoningEffortsForModel(model string) []string {
	if model == "gpt-6.1-sol" {
		return []string{"high", "ultra"}
	}
	return []string{"high"}
}

type isolatedModelTestSession struct {
	*serviceTierTestSession
	model string
}

func (s *isolatedModelTestSession) GetModel() string { return s.model }
func (s *isolatedModelTestSession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.mu.Lock()
	s.reply = fmt.Sprintf("model=%s tier=%s native=%s", s.model, s.tier, s.id)
	s.mu.Unlock()
	return s.cujAgentSession.Send(prompt, id, images, files)
}

func TestSessionModelSelection_CardsAndCommandsDoNotChangeProviderDefaults(t *testing.T) {
	a := newIsolatedModelTestAgent()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	s := e.sessions.GetOrCreateActive("test:a")
	s.SelectProvider("key-a")
	s.SetAgentSessionID("original-native", a.Name())
	s.AddHistory("user", "keep original task")
	sibling := e.sessions.GetOrCreateActive("test:sibling")
	sibling.SelectProvider("key-a")
	other := e.sessions.GetOrCreateActive("test:b")
	other.SelectProvider("key-b")
	for _, command := range []string{"/model switch 1", "/model sol", "/reasoning ultra"} {
		e.ReceiveMessage(p, &Message{SessionKey: "test:a", Platform: "test", Content: command})
		if command == "/model switch 1" && (s.GetAgentSessionID() != "original-native" || s.HistoryLen() != 1) {
			t.Fatal("model switch lost native identity/history")
		}
	}
	if a.GetReasoningEffort() != "ultra" {
		t.Fatal("reasoning menu validated the provider default instead of the selected model")
	}
	if s.GetModelOverride() != "gpt-6.1-sol" || !strings.Contains(e.renderModelCard("test:a").RenderText(), "gpt-6.1-sol") {
		t.Fatal("selected model missing from conversation and card")
	}
	if a.GetModel() != "global-default" || a.ListProviders()[0].Model != "gpt-6-astra" {
		t.Fatal("mutated shared model/provider defaults")
	}
	if sessionModelName(a, sibling) != "gpt-6-astra" || sessionModelName(a, other) != "other-provider-default" {
		t.Fatal("model leaked into another conversation")
	}
	if card := e.handleModelCardAction("switch custom-model", "test:a"); !strings.Contains(card.RenderText(), "custom-model") || s.GetModelOverride() != "custom-model" {
		t.Fatal("card action did not save the conversation model")
	}
	e.performModelSwitchAsync("test:a", nil, a, e.sessions, "gpt-6.1-sol")
	if s.GetModelOverride() != "gpt-6.1-sol" {
		t.Fatal("async card action did not use conversation selection")
	}
}

func TestSessionModelSelection_SaveFailureKeepsPreviousChoice(t *testing.T) {
	a := newIsolatedModelTestAgent()
	e := NewEngine("test", a, nil, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	s := e.sessions.GetOrCreateActive("test:a")
	if _, err := e.switchSessionModel(a, e.sessions, "test:a", "previous"); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = t.TempDir()
	if _, err := e.switchSessionModel(a, e.sessions, "test:a", "gpt-6.1-sol"); err == nil {
		t.Fatal("reported success after persistence failure")
	}
	if s.GetModelOverride() != "previous" {
		t.Fatal("failed save lost previous choice")
	}
}

func sessionModelAndFastCUJ(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	newEngine := func() (*Engine, *isolatedModelTestAgent, *stubPlatformEngine) {
		a := newIsolatedModelTestAgent()
		p := &stubPlatformEngine{n: "test"}
		e := NewEngine("test", a, []Platform{p}, path, LangEnglish)
		t.Cleanup(func() { _ = e.Stop() })
		return e, a, p
	}
	e, a, p := newEngine()
	send := func(key, content string) {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: key, Content: content, ReplyCtx: key})
	}
	wait := func(key, want string) {
		t.Helper()
		env := &cujEnv{t: t, engine: e, plat: p}
		env.waitFor(want, 3*time.Second, func() bool {
			return strings.Contains(strings.Join(p.getSent(), "\n"), want) && !e.sessions.GetOrCreateActive(key).Busy()
		})
	}
	send("test:a", "/provider switch key-a")
	send("test:a", "/model switch gpt-6.1-sol")
	send("test:a", "/reasoning ultra")
	send("test:a", "first task")
	wait("test:a", "model=gpt-6.1-sol tier=default native=native-key-a-1")
	// A second conversation retains its own provider/model and can opt into Fast.
	send("test:b", "/provider switch key-b")
	send("test:b", "/fast on")
	send("test:b", "second task")
	wait("test:b", "model=other-provider-default tier=fast native=native-key-b-2")
	if a.GetModel() != "global-default" {
		t.Fatal("conversation changed shared agent model")
	}
	_ = e.Stop()
	e, _, p = newEngine()
	send("test:a", "resume first task")
	wait("test:a", "model=gpt-6.1-sol tier=default native=native-key-a-1")
	send("test:b", "resume second task")
	wait("test:b", "model=other-provider-default tier=fast native=native-key-b-2")
	p.clearSent()
	send("test:a", "/history")
	if text := strings.Join(p.getSent(), "\n"); !strings.Contains(text, "first task") || strings.Contains(text, "second task") {
		t.Fatalf("wrong history after resume: %s", text)
	}
}

func TestSessionFastDefault_ExplicitChoiceAndPersistenceFailure(t *testing.T) {
	a := newIsolatedModelTestAgent()
	sm := NewSessionManager("")
	s := sm.GetOrCreateActive("test:a")
	if err := ensureSessionServiceTier(a, sm, s); err != nil {
		t.Fatal(err)
	}
	if s.GetServiceTier() != "default" {
		t.Fatal("Fast should be off before the first turn")
	}
	if err := sm.setSessionServiceTier(s, "fast"); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionServiceTier(a, sm, s); err != nil || s.GetServiceTier() != "fast" {
		t.Fatal("default overwrote explicit Fast selection")
	}
	sm.storePath = t.TempDir()
	other := sm.GetOrCreateActive("test:b")
	if started, err := startAgentSession(context.Background(), a, other, sm, ""); err == nil || started != nil {
		t.Fatal("started a billable turn after failing to save the default tier")
	}
}
