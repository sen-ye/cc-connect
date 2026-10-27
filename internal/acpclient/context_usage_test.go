package acpclient

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestContextUsage_NotificationsKeepSessionIsolation(t *testing.T) {
	s := runtimeTestSession(t, "original-session", "normal")
	if s.GetContextUsage() != nil {
		t.Fatal("reported usage before receiving any usage signal")
	}
	notify := func(session string, used int) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"sessionId": session, "update": map[string]any{
			"sessionUpdate": "usage_update", "used": used, "size": 128,
		}})
		if err != nil {
			t.Fatal(err)
		}
		s.onNotification("session/update", raw)
	}
	notify("child-session", 100)
	if s.GetContextUsage() != nil {
		t.Fatal("foreign session usage reached the parent")
	}
	notify("original-session", 42)
	usage := s.GetContextUsage()
	if usage == nil || usage.UsedTokens != 42 || usage.ContextWindow != 128 {
		t.Fatalf("context usage = %+v", usage)
	}
	usage.UsedTokens = 999
	if s.GetContextUsage().UsedTokens != 42 {
		t.Fatal("caller could mutate the stored usage snapshot")
	}
}

func TestContextUsage_PromptResponseAvailableBeforeResult(t *testing.T) {
	for _, interruptible := range []bool{false, true} {
		t.Run(fmt.Sprintf("interruptible_%v", interruptible), func(t *testing.T) {
			s := runtimeTestSession(t, "original-session", "usage")
			s.interruptible = interruptible
			if err := s.Send("finish", "usage-test", nil, nil); err != nil {
				t.Fatal(err)
			}
			awaitRuntimeEvent(t, s, core.EventResult)
			usage := s.GetContextUsage()
			if usage == nil || usage.UsedTokens != 64 || usage.ContextWindow != 128 ||
				usage.InputTokens != 100 || usage.OutputTokens != 20 ||
				usage.TotalTokens != 120 || usage.CachedInputTokens != 60 {
				t.Fatalf("result delivered before its context usage was stored: %+v", usage)
			}
		})
	}
}
