package cursor

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Uses a private CURSOR_CONFIG_DIR containing an operator-provided credential
// copy. Never resumes a production chat or changes the live bot configuration.
func TestNativeCursorACP_MigrateAppendAndResume(t *testing.T) {
	bin, configDir := os.Getenv("CC_CONNECT_TEST_CURSOR_BIN"), os.Getenv("CC_CONNECT_TEST_CURSOR_CONFIG")
	if bin == "" || configDir == "" || os.Getenv("CC_CONNECT_TEST_CURSOR_LIVE") != "1" {
		t.Skip("native ACP check is opt-in with isolated config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	workDir := t.TempDir()
	opts := map[string]any{"cmd": bin, "work_dir": workDir, "mode": "ask", "model": "gemini-4-argon-high", "env": map[string]string{"CURSOR_CONFIG_DIR": configDir, "TMPDIR": t.TempDir()}}
	create := func(backend, id string) core.AgentSession {
		t.Helper()
		opts["backend"] = backend
		a, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.StartSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	await := func(s core.AgentSession) string {
		t.Helper()
		var answer strings.Builder
		for {
			select {
			case event, ok := <-s.Events():
				if !ok {
					t.Fatal("session exited")
				}
				switch event.Type {
				case core.EventTurnSuperseded:
					answer.Reset()
				case core.EventText:
					answer.WriteString(event.Content)
				case core.EventResult:
					if event.Content != "" {
						return event.Content
					}
					return answer.String()
				case core.EventError:
					t.Fatalf("native ACP error: %v", event.Error)
				case core.EventToolUse:
					t.Fatal("no-tool probe unexpectedly requested a tool")
				}
			case <-ctx.Done():
				t.Fatal("native check timed out")
			}
		}
	}
	const marker = "CURSOR_ACP_MIGRATION_20261025"
	printSession := create("print", "")
	if err := printSession.Send("Remember the marker "+marker+". Reply exactly READY. Do not use tools.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(await(printSession), "READY") {
		t.Fatal("initial print request failed")
	}
	id := printSession.CurrentSessionID()
	if id == "" {
		t.Fatal("missing print session ID")
	}
	if err := printSession.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(configDir, "chats", workspaceHash(workDir), id, "store.db")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	s := create("acp", id)
	if err := s.Send("Reply only with the marker I asked you to remember. Do not use tools.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(await(s), marker) {
		t.Fatal("print history was not migrated")
	}
	after, err := os.ReadFile(source)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("original print database changed")
	}
	t.Log("print-to-ACP history and session ID preserved; original database unchanged")
	if err := s.Send("Write 250 numbered lines, each containing its number and the sentence: temporary streaming test. Do not use tools.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	waiting := true
	for waiting {
		select {
		case event := <-s.Events():
			if event.Type == core.EventError {
				t.Fatalf("stream failed: %v", event.Error)
			}
			if event.Type == core.EventResult {
				t.Fatal("request finished before supplement")
			}
			if event.Type == core.EventText && event.Content != "" {
				waiting = false
			}
		case <-ctx.Done():
			t.Fatal("no streaming output")
		}
	}
	appender, ok := s.(core.MessageAppender)
	if !ok {
		t.Fatal("ACP appender missing")
	}
	if err := appender.AppendMessage("Stop the list. Reply with the original marker followed by APPEND_OK. Do not use tools.", "ps", nil, nil); err != nil {
		t.Fatal(err)
	}
	answer := await(s)
	if !strings.Contains(answer, marker) || !strings.Contains(answer, "APPEND_OK") {
		t.Fatal("supplement lost the original context")
	}
	if s.CurrentSessionID() != id {
		t.Fatal("supplement created a new session")
	}
	t.Log("mid-generation append completed in the original session")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = create("acp", id)
	if err := s.Send("Reply only with the original remembered marker. Do not use tools.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(await(s), marker) || s.CurrentSessionID() != id {
		t.Fatal("ACP process restart lost conversation")
	}
	t.Log("ACP restart and resume passed")
}

func TestNativeCursorACP_GrokModelAliases(t *testing.T) {
	bin, configDir := os.Getenv("CC_CONNECT_TEST_CURSOR_BIN"), os.Getenv("CC_CONNECT_TEST_CURSOR_CONFIG")
	if bin == "" || configDir == "" || os.Getenv("CC_CONNECT_TEST_CURSOR_LIVE") != "1" {
		t.Skip("native ACP check is opt-in with isolated config")
	}
	for _, model := range []string{"grok-4.7-high", "grok-4.7-xhigh-fast"} {
		t.Run(model, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			a, err := New(map[string]any{"cmd": bin, "backend": "acp", "model": model, "mode": "force", "work_dir": t.TempDir(), "env": map[string]string{"CURSOR_CONFIG_DIR": configDir, "TMPDIR": t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.StartSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.(interface{ GetModel() string }).GetModel() != cursorACPModel(model) {
				t.Fatal("model selection changed")
			}
		})
	}
}
