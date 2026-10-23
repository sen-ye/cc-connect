package cursor

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCursorErrorEventNotDropped(t *testing.T) {
	s := newTestSession("ask")
	defer s.cancel()
	s.handleEvent(map[string]any{"type": "error", "error": map[string]any{"message": "Provider Error"}})
	select {
	case e := <-s.Events():
		if e.Type != core.EventError {
			t.Fatalf("got %s", e.Type)
		}
	default:
		t.Fatal("error event discarded")
	}
}

func TestCursorFailedResultIsNotSuccess(t *testing.T) {
	s := newTestSession("ask")
	defer s.cancel()
	s.handleEvent(map[string]any{"type": "result", "is_error": true, "result": "Provider Error"})
	e := <-s.Events()
	if e.Type != core.EventError {
		t.Fatalf("failed result became %s, done=%v", e.Type, e.Done)
	}
}

func TestCursorNonzeroExitWithoutStderrProducesFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("test requires sh")
	}
	s, err := newCursorSession(context.Background(), sh, []string{"-c", "exit 42", "--"}, t.TempDir(), "", "ask", "saved-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Send("probe", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	select {
	case e := <-s.Events():
		if e.Type != core.EventError {
			t.Fatalf("got %s", e.Type)
		}
	default:
		t.Fatal("failed process produced no terminal event")
	}
}
