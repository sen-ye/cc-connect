package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAgentFailure_FinalizesStreamingCardWithoutDuplicateMessage(t *testing.T) {
	card := &recordingStreamCard{}
	p := &recordingStreamCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, card: card}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:failure-card"
	session := e.sessions.GetOrCreateActive(key)
	agentSession := newControllableSession("saved-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	t.Cleanup(func() { _ = e.Stop() })
	agentSession.events <- Event{Type: EventRetry, Retry: &AgentRetry{Attempt: 2}}
	agentSession.events <- Event{Type: EventError, Error: &AgentFailure{Kind: FailureNoProgress, RetryCount: 2}}
	e.processInteractiveEvents(state, session, e.sessions, key, "message", time.Now(), nil, nil, state.replyCtx, 0)
	if !card.finalized() || !strings.Contains(card.finalContent(), "recovery attempts saved no new progress") || !strings.Contains(card.finalContent(), "saved-session") {
		t.Fatalf("card left running or missing failure: %s", card.finalContent())
	}
	for _, message := range p.getSent() {
		if strings.Contains(message, "recovery attempts saved no new progress") {
			t.Fatal("failure duplicated outside finalized card")
		}
	}
}

func TestAgentFailure_AllLanguagesPreserveDiagnostics(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		e := NewEngine("test", &cujAgent{}, nil, "", lang)
		t.Cleanup(func() { _ = e.Stop() })
		for _, kind := range []AgentFailureKind{FailureProvider, FailureConnection, FailureStalled, FailureNoProgress, FailureRateLimit, FailureAuth, FailureCancelled, FailureProcess} {
			failure := &AgentFailure{Kind: kind, Detail: "safe detail", Model: "model-a", RequestID: "request-a", ProviderStatus: "400", RetryCount: 2, RetryAfter: "30"}
			text := e.agentErrorMessage(fmt.Errorf("wrapped: %w", failure), "saved-session")
			if strings.Contains(text, "%!") || text == "" {
				t.Fatalf("invalid translation %s/%s: %s", lang, kind, text)
			}
			if kind != FailureCancelled && (!strings.Contains(text, "saved-session") || !strings.Contains(text, "request-a") || !strings.Contains(text, "400")) {
				t.Fatalf("lost diagnostics: %s", text)
			}
		}
	}
}

func TestAgentFailure_PreservesExistingSessionNotFoundAdvice(t *testing.T) {
	e := NewEngine("test", &cujAgent{}, nil, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	failure := &AgentFailure{Kind: FailureProcess, Detail: "Session not found: missing"}
	if got := e.agentErrorMessage(failure, "missing"); got != e.i18n.T(MsgSessionNotFound) {
		t.Fatalf("lost existing recovery advice: %s", got)
	}
}
