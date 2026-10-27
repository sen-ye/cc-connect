package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestACPAppendHelperProcess(t *testing.T) {
	mode := os.Getenv("CC_ACP_APPEND_HELPER")
	if mode == "" {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	var held any
	sid := "fixture-session"
	update := func(kind, text string) {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": sid, "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}},
		}})
	}
	respond := func(id any, result any) { _ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var q struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &q) != nil {
			os.Exit(2)
		}
		switch q.Method {
		case "initialize":
			respond(q.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}})
		case "authenticate", "session/set_mode", "session/set_config_option":
			respond(q.ID, map[string]any{})
		case "session/new", "session/load":
			if q.Method == "session/load" {
				if mode == "missing" {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32602, "message": "missing session"}})
					continue
				}
				sid, _ = q.Params["sessionId"].(string)
				for range 300 {
					update("agent_message_chunk", "REPLAY_MUST_NOT_APPEAR")
				}
			}
			respond(q.ID, map[string]any{"sessionId": sid, "modes": map[string]any{"currentModeId": "ask", "availableModes": []any{map[string]any{"id": "ask", "name": "Ask"}}}, "models": map[string]any{"currentModelId": "fixture-model"}})
		case "session/cancel":
			if held != nil {
				update("agent_message_chunk", "CANCELLED_DRAFT")
				respond(held, map[string]any{"stopReason": "cancelled"})
				held = nil
			}
		case "session/prompt":
			if held != nil {
				os.Exit(3)
			} // replacement must wait for cancellation
			blocks, _ := q.Params["prompt"].([]any)
			text, _ := blocks[0].(map[string]any)["text"].(string)
			if strings.Contains(text, "finish") {
				images := 0
				for _, raw := range blocks {
					if raw.(map[string]any)["type"] == "image" {
						images++
					}
				}
				update("agent_thought_chunk", "First half ")
				update("agent_thought_chunk", "second half")
				update("agent_message_chunk", fmt.Sprintf("NEW_ANSWER session=%s images=%d", sid, images))
				result := map[string]any{"stopReason": "end_turn"}
				if mode == "usage" {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
						"sessionId": sid, "update": map[string]any{"sessionUpdate": "usage_update", "used": 64, "size": 128},
					}})
					result["usage"] = map[string]any{"inputTokens": 100, "outputTokens": 20, "totalTokens": 120, "cachedReadTokens": 60}
				}
				respond(q.ID, result)
			} else {
				held = q.ID
				update("agent_message_chunk", "OLD_DRAFT")
				if mode == "permission" {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{
						"sessionId": sid, "toolCall": map[string]any{"toolCallId": "tool-1", "title": "Read", "kind": "read"}, "options": []any{map[string]any{"optionId": "allow", "kind": "allow_once", "name": "Allow"}},
					}})
				}
			}
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
		}
	}
	os.Exit(0)
}

func runtimeTestSession(t *testing.T, resume, mode string) *acpSession {
	t.Helper()
	s, err := newACPSession(context.Background(), acpSessionConfig{
		command: os.Args[0], args: []string{"-test.run=^TestACPAppendHelperProcess$", "--"},
		extraEnv: []string{"CC_ACP_APPEND_HELPER=" + mode}, workDir: t.TempDir(),
		resumeSessionID: resume, strictResume: true, interruptible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func awaitRuntimeEvent(t *testing.T, s *acpSession, kind core.EventType) []core.Event {
	t.Helper()
	var result []core.Event
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case e := <-s.Events():
			result = append(result, e)
			if e.Type == core.EventError {
				t.Fatalf("unexpected error: %v", e.Error)
			}
			if e.Type == kind {
				return result
			}
		case <-deadline.C:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestAppendMessage_CancelledPromptCannotCompleteReplacement(t *testing.T) {
	s := runtimeTestSession(t, "original-session", "normal")
	<-s.Events() // resumable identity, with replay suppressed
	if err := s.Send("hold", "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitRuntimeEvent(t, s, core.EventText)
	if err := s.appendMessage("finish with image", "supplement", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("image")}}, nil); err != nil {
		t.Fatal(err)
	}
	events := awaitRuntimeEvent(t, s, core.EventResult)
	reset, summary, answer, terminals := false, false, false, 0
	for _, e := range events {
		if e.Type == core.EventTurnSuperseded {
			reset = true
		}
		if e.Type == core.EventThinking && e.Content == "First half second half" {
			summary = true
		}
		if e.Type == core.EventText && strings.Contains(e.Content, "NEW_ANSWER session=original-session images=1") {
			answer = true
		}
		if e.Type == core.EventResult {
			terminals++
			if !reset || !answer {
				t.Fatal("cancelled request completed the task")
			}
		}
	}
	if !summary || terminals != 1 || s.CurrentSessionID() != "original-session" {
		t.Fatalf("wrong replacement events: %+v", events)
	}
	if err := s.Send("finish next", "third", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitRuntimeEvent(t, s, core.EventResult)
}

func TestResumeReplay_IsSuppressedAndCannotFillEventBuffer(t *testing.T) {
	s := runtimeTestSession(t, "original-session", "normal")
	e := <-s.Events()
	if e.Type != core.EventText || e.Content != "" || e.SessionID != "original-session" {
		t.Fatalf("replayed history leaked: %+v", e)
	}
	select {
	case e := <-s.Events():
		t.Fatalf("unexpected replay event: %+v", e)
	default:
	}
}

func TestStrictResume_DoesNotCreateEmptySession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := newACPSession(ctx, acpSessionConfig{command: os.Args[0], args: []string{"-test.run=^TestACPAppendHelperProcess$", "--"}, extraEnv: []string{"CC_ACP_APPEND_HELPER=missing"}, workDir: t.TempDir(), resumeSessionID: "original", strictResume: true})
	if s != nil {
		_ = s.Close()
	}
	if err == nil {
		t.Fatal("failed resume silently became a new session")
	}
}

func TestClose_InterruptsPromptAndIsIdempotent(t *testing.T) {
	s := runtimeTestSession(t, "", "normal")
	<-s.Events()
	if err := s.Send("hold", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitRuntimeEvent(t, s, core.EventText)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Alive() {
		t.Fatal("session still alive")
	}
}

func TestAppendMessage_InvalidatesOldPermission(t *testing.T) {
	s := runtimeTestSession(t, "", "permission")
	<-s.Events()
	if err := s.Send("hold", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := awaitRuntimeEvent(t, s, core.EventPermissionRequest)
	permission := events[len(events)-1]
	if permission.RequestDone == nil {
		t.Fatal("permission has no cancellation signal")
	}
	if err := s.appendMessage("finish", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-permission.RequestDone:
	default:
		t.Fatal("cancelled permission still active")
	}
	awaitRuntimeEvent(t, s, core.EventResult)
	if err := s.RespondPermission(permission.RequestID, core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("old approval reached the replacement request")
	}
}
