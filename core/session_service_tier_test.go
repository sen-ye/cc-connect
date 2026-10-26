package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type serviceTierTestAgent struct {
	*providerBindingTestAgent
	dir     string
	fail    bool
	started *serviceTierTestSession
}

func newServiceTierTestAgent() *serviceTierTestAgent {
	return &serviceTierTestAgent{providerBindingTestAgent: newProviderBindingTestAgent("key-a")}
}
func (*serviceTierTestAgent) Name() string                     { return "service-tier-test" }
func (*serviceTierTestAgent) SupportsSessionServiceTier() bool { return true }
func (a *serviceTierTestAgent) GetWorkDir() string             { return a.dir }
func (a *serviceTierTestAgent) StartSessionWithProvider(ctx context.Context, id, name string) (AgentSession, error) {
	base, err := a.providerBindingTestAgent.StartSessionWithProvider(ctx, id, name)
	if err != nil {
		return nil, err
	}
	s := &serviceTierTestSession{providerBindingTestSession: base.(*providerBindingTestSession), tier: "fast", fail: a.fail}
	a.mu.Lock()
	a.started = s
	a.mu.Unlock()
	return s, nil
}

type serviceTierTestSession struct {
	*providerBindingTestSession
	tier string // protected by cujAgentSession.mu
	fail bool
}

func (s *serviceTierTestSession) GetServiceTier() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tier
}
func (s *serviceTierTestSession) SetServiceTier(tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return fmt.Errorf("service tier RPC rejected")
	}
	s.tier = tier
	return nil
}
func (s *serviceTierTestSession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.mu.Lock()
	s.reply = "runtime tier: " + s.tier + " native: " + s.id
	s.mu.Unlock()
	return s.cujAgentSession.Send(prompt, id, images, files)
}

func TestSessionServiceTier_SaveReloadAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	s := sm.GetOrCreateActive("test:one")
	s.SetAgentSessionID("native-one", "test")
	s.AddHistory("user", "keep history")
	if err := sm.setSessionServiceTier(s, "default"); err != nil {
		t.Fatal(err)
	}
	reloaded := NewSessionManager(path).GetOrCreateActive("test:one")
	if reloaded.GetServiceTier() != "default" || reloaded.GetAgentSessionID() != "native-one" || reloaded.HistoryLen() != 1 {
		t.Fatal("service tier persistence changed native identity/history or lost the override")
	}
	if sm.GetOrCreateActive("test:two").GetServiceTier() != "" {
		t.Fatal("new conversation inherited another conversation's service tier")
	}
	// A directory cannot be atomically replaced by a session snapshot.
	sm.storePath = t.TempDir()
	controller := &serviceTierTestSession{providerBindingTestSession: &providerBindingTestSession{cujAgentSession: newCUJAgentSession()}, tier: "default"}
	if err := updateSessionServiceTier(sm, s, controller, "fast"); err == nil {
		t.Fatal("persistence failure reported success")
	}
	if s.GetServiceTier() != "default" || controller.GetServiceTier() != "default" {
		t.Fatal("persistence failure did not restore both runtime and stored settings")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestSessionServiceTier_FailedRPCDoesNotSave(t *testing.T) {
	sm := NewSessionManager("")
	s := sm.GetOrCreateActive("test:one")
	controller := &serviceTierTestSession{providerBindingTestSession: &providerBindingTestSession{cujAgentSession: newCUJAgentSession()}, tier: "fast", fail: true}
	if err := updateSessionServiceTier(sm, s, controller, "default"); err == nil {
		t.Fatal("RPC failure reported success")
	}
	if s.GetServiceTier() != "" || controller.GetServiceTier() != "fast" {
		t.Fatal("failed RPC changed the saved or runtime setting")
	}
}

func TestStartAgentSession_ServiceTierFailureClosesBeforePrompt(t *testing.T) {
	a := newServiceTierTestAgent()
	a.fail = true
	sm := NewSessionManager("")
	s := sm.GetOrCreateActive("test:one")
	s.SetAgentSessionID("native-one", a.Name())
	if err := sm.setSessionServiceTier(s, "default"); err != nil {
		t.Fatal(err)
	}
	if started, err := startAgentSession(context.Background(), a, s, sm, "native-one"); err == nil || started != nil {
		t.Fatal("ignored a failed restore and returned a billable session")
	}
	if a.started.Alive() || len(a.started.getSentPrompts()) != 0 || s.GetAgentSessionID() != "native-one" {
		t.Fatal("restore failure started a prompt or lost the native session")
	}
}

func TestFastCommand_ValidationAndNoPrompt(t *testing.T) {
	for _, tc := range []struct {
		command string
		agent   Agent
		want    string
	}{
		{"/fast off extra", newServiceTierTestAgent(), "Usage: /fast"},
		{"/fast turbo", newServiceTierTestAgent(), "Usage: /fast"},
		{"/fast off", &stubAgent{}, "does not support"},
		{"/fast", newServiceTierTestAgent(), "inherit the agent default"},
	} {
		t.Run(tc.command+tc.want, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", tc.agent, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			e.ReceiveMessage(p, &Message{SessionKey: "test:one", Platform: "test", Content: tc.command})
			if !strings.Contains(strings.Join(p.getSent(), "\n"), tc.want) {
				t.Fatalf("replies = %v", p.getSent())
			}
			if len(e.interactiveStates) != 0 {
				t.Fatal("local Fast command started an agent/model request")
			}
		})
	}
}

func TestTimerJob_UsesConversationServiceTier(t *testing.T) {
	p := &cujReplyCtxPlatform{&stubPlatformEngine{n: "test"}}
	e := NewEngine("test", newServiceTierTestAgent(), []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Platform: "test", UserID: "user", Content: "/fast off", ReplyCtx: "ctx"})
	p.clearSent()
	if err := e.ExecuteTimerJob(&TimerJob{ID: "standard-timer", Project: "test", SessionKey: "test:topic", Prompt: "check progress"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "runtime tier: default") {
		t.Fatalf("timer lost the conversation's speed setting: %s", got)
	}
}
