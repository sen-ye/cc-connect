package cursor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt-in: exercises the installed CLI's actual headless error path against a
// local endpoint with a dummy credential. Never uses the user's real API key.
func TestNativeCursor_HeadlessFailure(t *testing.T) {
	bin := os.Getenv("CC_CONNECT_TEST_CURSOR_BIN")
	if bin == "" {
		t.Skip("set CC_CONNECT_TEST_CURSOR_BIN for native CLI validation")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"Audit: authentication rejected"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	s, err := newCursorSession(ctx, bin, []string{"--endpoint", server.URL, "--api-key", "cc-connect-offline-test"}, t.TempDir(), "gemini-4-argon-high", "ask", "", []string{"TMPDIR=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "AGENT_JUMP_PROXY=" + server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Send("Reply OK without using tools.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-s.Events():
			if event.Type == core.EventResult {
				t.Fatal("CLI authentication failure became success")
			}
			if event.Type == core.EventError {
				if requests.Load() == 0 {
					t.Fatal("CLI failed before contacting the local endpoint")
				}
				if event.Error == nil {
					t.Fatal("no failure details")
				}
				t.Logf("native headless failure: %v", event.Error)
				return
			}
		case <-ctx.Done():
			t.Fatal("native CLI produced no failure before timeout")
		}
	}
}

// Separately opt-in because this checks a real, authenticated model request.
func TestNativeCursor_SuccessAndResume(t *testing.T) {
	bin := os.Getenv("CC_CONNECT_TEST_CURSOR_BIN")
	if bin == "" || os.Getenv("CC_CONNECT_TEST_CURSOR_LIVE") != "1" {
		t.Skip("native live test is opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := newCursorSession(ctx, bin, nil, t.TempDir(), "gemini-4-argon-high", "ask", "", []string{"TMPDIR=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	saved := ""
	for _, want := range []string{"CURSOR_ERROR_FIX_OK", "CURSOR_RESUME_OK"} {
		if err = s.Send("Reply exactly "+want+". Do not call tools.", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		finished := false
		for !finished {
			select {
			case event := <-s.Events():
				if event.Type == core.EventError {
					t.Fatalf("native failure: %v", event.Error)
				}
				if event.Type == core.EventResult {
					if !strings.Contains(event.Content, want) {
						t.Fatalf("unexpected answer: %q", event.Content)
					}
					if event.SessionID == "" || saved != "" && saved != event.SessionID {
						t.Fatal("resume changed session ID")
					}
					saved = event.SessionID
					finished = true
				}
			case <-ctx.Done():
				t.Fatal("native CLI timed out")
			}
		}
	}
	t.Log("native two-turn response and session ID preserved")
}
