package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCursorDiagnostics_ReplaysProviderAndNoProgressFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      core.AgentFailureKind
		retries   int
		status    string
		retryable bool
	}{
		{"provider_error", core.FailureProvider, 1, "400", false},
		{"no_progress", core.FailureNoProgress, 2, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.name + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			d := &cursorDiagnostics{}
			retries := 0
			for _, line := range strings.Split(string(data), "\n") {
				if d.observe(line, "session-a") != nil {
					retries++
				}
			}
			if !d.failed || d.failure == nil || d.failure.Kind != tc.kind || d.failure.ProviderStatus != tc.status || d.failure.RetryCount != tc.retries || retries != tc.retries || d.failure.Retryable == nil || *d.failure.Retryable != tc.retryable {
				t.Fatalf("failure=%+v; retry notices=%d", d.failure, retries)
			}
			for _, line := range strings.Split(string(data), "\n") {
				if d.observe(line, "session-a") != nil {
					t.Fatal("duplicate retry notification")
				}
			}
		})
	}
}

func diagnosticLine(message string, meta map[string]any) string {
	data, _ := json.Marshal(map[string]any{"message": message, "metadata": meta})
	return "[2026-10-23T15:00:00Z] logger " + string(data)
}

func TestCursorDiagnostics_IgnoresOtherRequestsAndTelemetry(t *testing.T) {
	d := &cursorDiagnostics{}
	line := diagnosticLine("[AGENT_ERROR_DIAGNOSTICS] recovery", map[string]any{"willRetry": true, "attempt": 0, "originalRequestId": "parent", "requestId": "request-1", "conversationId": "session-a"})
	if d.observe(line, "session-a") == nil {
		t.Fatal("missing real retry")
	}
	for _, noise := range []string{
		strings.ReplaceAll(line, "parent", "other-request"),
		strings.ReplaceAll(line, "session-a", "session-b"),
		diagnosticLine("tool output says Provider Error", map[string]any{"error_code": "PROVIDER_ERROR"}),
		"[2026-10-23T15:00:00Z] otel.error span ended twice",
		`[2026-10-23T15:00:00Z] logger {"broken":`,
	} {
		if d.observe(noise, "session-a") != nil || d.failed {
			t.Fatal("unrelated output affected turn")
		}
	}
	d.observe(diagnosticLine("[AGENT_ERROR_DIAGNOSTICS] cancel", map[string]any{"willRetry": false, "abortOrigin": "owner", "originalRequestId": "parent"}), "session-a")
	if d.failure.Kind != core.FailureCancelled {
		t.Fatalf("owner cancellation=%+v", d.failure)
	}
}

func TestCursorFailureClassification_DoesNotGuess429OrCancellation(t *testing.T) {
	for _, tc := range []struct {
		message, status string
		kind            core.AgentFailureKind
	}{
		{"[resource_exhausted] Error", "", ""}, {"[canceled] operation aborted", "", ""},
		{"Provider Error", "400", core.FailureProvider}, {"Provider Error (not rate limited)", "400", core.FailureProvider}, {"Provider Error", "429", core.FailureRateLimit},
		{"unauthenticated", "", core.FailureAuth}, {"Provider Error", "401", core.FailureAuth},
	} {
		if got := failureKind("", tc.message, tc.status); got != tc.kind {
			t.Fatalf("%q/%q: %s", tc.message, tc.status, got)
		}
	}
}

func TestCursorLogTail_PartialAndOversizedLinesDoNotHideFailure(t *testing.T) {
	var tail cursorLogTail
	var lines []string
	consume := func(s string) { lines = append(lines, s) }
	tail.consume([]byte("part"), consume)
	tail.consume([]byte("ial\n"), consume)
	tail.consume([]byte(strings.Repeat("x", 300*1024)), consume)
	tail.consume([]byte("tail\nfinal failure\n"), consume)
	if strings.Join(lines, "|") != "partial|final failure" || len(tail.pending) != 0 {
		t.Fatalf("lines=%v", lines)
	}
}

func TestCursorProcessLog_BindsPIDAndLaunchTime(t *testing.T) {
	start := time.Date(2026, 10, 23, 14, 49, 8, 0, time.UTC)
	name := "session-2026-10-23T14-49-08-943Z-123-1.log"
	if !cursorProcessLog(name, 123, start) || cursorProcessLog(name, 124, start) || cursorProcessLog(name, 123, start.Add(time.Minute)) || cursorProcessLog("latest.log", 123, start) {
		t.Fatal("incorrect log attribution")
	}
}

func TestCursorErrorRedaction_BoundsDetailsAndMasksCredentials(t *testing.T) {
	detail := "\x1b[31mProvider Error\x1b[0m token-value-012345 sk-examplecredential Authorization: Bearer another-secret api_key=third-secret https://user:password@example.com/path"
	got := safeCursorDetail(detail, []string{"CURSOR_API_KEY=token-value-012345"})
	for _, secret := range []string{"token-value-012345", "sk-examplecredential", "another-secret", "third-secret", "user:password", "\x1b"} {
		if strings.Contains(got, secret) {
			t.Fatalf("leaked %s in %s", secret, got)
		}
	}
	if len(safeCursorDetail(strings.Repeat("x", 100000), nil)) > 1300 {
		t.Fatal("unbounded error")
	}
}

func errorTestSession(t *testing.T, mode, tmp string) *cursorSession {
	t.Helper()
	fixtures, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	s, err := newCursorSession(context.Background(), os.Args[0], []string{"-test.run=^TestCursorErrorProcessHelper$", "--"}, t.TempDir(), "test-model", "ask", "", []string{"GO_WANT_CURSOR_ERROR_HELPER=1", "CURSOR_ERROR_MODE=" + mode, "TMPDIR=" + tmp, "CURSOR_ERROR_FIXTURES=" + fixtures})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func collectCursorTurn(t *testing.T, s *cursorSession) []core.Event {
	t.Helper()
	var out []core.Event
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatal("session unexpectedly closed")
			}
			out = append(out, e)
			if e.Type == core.EventError || e.Type == core.EventResult {
				return out
			}
		case <-timer.C:
			t.Fatal("turn never reached a terminal event")
		}
	}
}

func TestCursorProcessFailures_OneTerminalAndResume(t *testing.T) {
	for _, mode := range []string{"provider_error", "no_progress", "silent", "empty_success", "structured", "late_failure", "no_init"} {
		t.Run(mode, func(t *testing.T) {
			s := errorTestSession(t, mode, t.TempDir())
			if err := s.Send("fail", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			events := collectCursorTurn(t, s)
			last := events[len(events)-1]
			if last.Type != core.EventError {
				t.Fatalf("false success: %+v", last)
			}
			failure, ok := last.Error.(*core.AgentFailure)
			if !ok {
				t.Fatalf("unclassified error: %T", last.Error)
			}
			if mode == "provider_error" && (failure.Kind != core.FailureProvider || failure.ProviderStatus != "400" || failure.RetryCount != 1) {
				t.Fatalf("provider error=%+v", failure)
			}
			if mode == "no_progress" && (failure.Kind != core.FailureNoProgress || failure.RetryCount != 2) {
				t.Fatalf("recovery error=%+v", failure)
			}
			if last.SessionID != "session-a" || !s.Alive() {
				t.Fatal("failed turn lost resumable session")
			}
			if err := s.Send("continue", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			events = collectCursorTurn(t, s)
			last = events[len(events)-1]
			if last.Type != core.EventResult || last.Content != "resumed session-a" {
				t.Fatalf("stale failure or lost resume: %+v", last)
			}
			select {
			case extra := <-s.Events():
				t.Fatalf("duplicate terminal: %+v", extra)
			default:
			}
		})
	}
}

func TestCursorConcurrentProcesses_DoNotShareDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	failed := errorTestSession(t, "no_progress", tmp)
	success := errorTestSession(t, "success", tmp)
	if err := failed.Send("fail", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := success.Send("work", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	a := collectCursorTurn(t, failed)
	b := collectCursorTurn(t, success)
	if a[len(a)-1].Type != core.EventError || b[len(b)-1].Type != core.EventResult {
		t.Fatal("cross-process diagnostic contamination")
	}
}

func TestCursorClose_CancellationHasNoProviderError(t *testing.T) {
	s := errorTestSession(t, "wait", t.TempDir())
	if err := s.Send("work", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("helper not ready")
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
	for e := range s.Events() {
		if e.Type == core.EventError {
			t.Fatalf("user stop became error: %+v", e)
		}
	}
	if err := s.Send("after close", "", nil, nil); err == nil {
		t.Fatal("closed session restarted")
	}
}

// Test subprocess implements the real --print/--output-format contract, without
// network calls. Its per-PID diagnostic files follow the installed CLI format.
func TestCursorErrorProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CURSOR_ERROR_HELPER") != "1" {
		return
	}
	write := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	if os.Getenv("CURSOR_ERROR_MODE") != "no_init" {
		write(map[string]any{"type": "system", "session_id": "session-a"})
	}
	if os.Args[len(os.Args)-1] == "continue" {
		joined := strings.Join(os.Args, " ")
		if !strings.Contains(joined, "--resume session-a") {
			os.Exit(9)
		}
		write(map[string]any{"type": "result", "subtype": "success", "result": "resumed session-a"})
		os.Exit(0)
	}
	mode := os.Getenv("CURSOR_ERROR_MODE")
	if mode == "provider_error" || mode == "no_progress" || mode == "no_init" {
		fixture := mode
		if fixture == "no_init" {
			fixture = "provider_error"
		}
		data, err := os.ReadFile(filepath.Join(os.Getenv("CURSOR_ERROR_FIXTURES"), fixture+".jsonl"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(8)
		}
		if mode == "no_init" {
			data = append([]byte(diagnosticLine("Agent client stream abort", map[string]any{"conversationId": "session-a", "requestId": "request-0"})+"\n"), data...)
		}
		dir := cursorLogDirectory(os.Environ())
		_ = os.MkdirAll(dir, 0o700)
		name := fmt.Sprintf("session-%s-%d-1.log", strings.ReplaceAll(time.Now().UTC().Format("2006-01-02T15-04-05.000Z"), ".", "-"), os.Getpid())
		_ = os.WriteFile(filepath.Join(dir, name), data, 0o600)
		if mode == "no_progress" {
			time.Sleep(700 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "opaque CLI failure")
		os.Exit(1)
	}
	switch mode {
	case "silent":
		os.Exit(42)
	case "empty_success":
		os.Exit(0)
	case "structured":
		write(map[string]any{"type": "result", "is_error": true, "result": "Provider Error", "session_id": "session-a"})
		fmt.Fprintln(os.Stderr, "Provider Error")
		os.Exit(1)
	case "late_failure":
		write(map[string]any{"type": "result", "result": "premature success"})
		os.Exit(3)
	case "wait":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	write(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Discussing Provider Error is ordinary answer text"}}}})
	fmt.Fprintln(os.Stderr, "Provider Error mentioned in non-fatal diagnostics")
	write(map[string]any{"type": "result", "result": "ok"})
	os.Exit(0)
}
