package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAppServerSession_ServiceTierUpdatesOnlyItsThread(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[reject], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stdin := &lockedWriteCloser{}
			s := &appServerSession{ctx: ctx, stdin: stdin, events: make(chan core.Event, 4), serviceTier: "fast", currentTurn: "ongoing-turn"}
			s.threadID.Store("parent-thread")
			s.alive.Store(true)
			done := make(chan error, 1)
			go func() { done <- s.SetServiceTier("default") }()
			var request struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(waitForWrittenJSONLine(t, stdin)), &request); err != nil {
				t.Fatal(err)
			}
			if request.Method != "thread/settings/update" || request.Params["threadId"] != "parent-thread" || request.Params["serviceTier"] != "default" || len(request.Params) != 2 {
				t.Fatalf("unexpected request: %+v", request)
			}
			response := rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(`{}`)}
			if reject {
				response.Error = &rpcError{Code: -32601, Message: "method unsupported"}
			} else {
				// The server reports its effective setting, which may arrive
				// separately from the settings RPC acknowledgement.
				s.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"parent-thread","threadSettings":{"serviceTier":null}}`))
			}
			s.handleResponse(response)
			select {
			case err := <-done:
				if (err != nil) != reject {
					t.Fatalf("update error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("settings update did not finish")
			}
			want := "default"
			if reject {
				want = "fast"
			}
			if s.GetServiceTier() != want || s.currentTurn != "ongoing-turn" || s.CurrentSessionID() != "parent-thread" || len(s.events) != 0 {
				t.Fatal("settings update changed the active turn, identity, or error-state cache")
			}
			if strings.Count(stdin.String(), "\n") != 1 {
				t.Fatal("setting sent extra RPCs, such as a prompt or a global config update")
			}
		})
	}
}

func TestAppServerSession_ServiceTierValidationAndReportedDefault(t *testing.T) {
	s := &appServerSession{}
	for _, tier := range []string{"", "off", "typo"} {
		if err := s.SetServiceTier(tier); err == nil {
			t.Fatalf("accepted invalid tier %q", tier)
		}
	}
	s.applyReportedServiceTier(nil)
	if s.GetServiceTier() != "default" {
		t.Fatal("null native tier is standard processing")
	}
	tier := "fast"
	s.applyReportedServiceTier(&tier)
	if s.GetServiceTier() != "fast" {
		t.Fatal("lost the resolved native default")
	}
	s.threadID.Store("parent")
	s.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"child","threadSettings":{"serviceTier":null}}`))
	if s.GetServiceTier() != "fast" {
		t.Fatal("child thread changed the parent's service tier")
	}
	s.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"parent","threadSettings":{"serviceTier":"priority"}}`))
	if s.GetServiceTier() != "fast" {
		t.Fatal("native priority alias was not reported as Fast")
	}
	if (&Agent{backend: "exec"}).SupportsSessionServiceTier() || !(&Agent{backend: "app_server"}).SupportsSessionServiceTier() {
		t.Fatal("advertised Fast settings on the wrong backend")
	}
}

func TestAppServerSession_ServiceTierRequiresNativeConfirmation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{ctx: ctx, stdin: stdin, serviceTier: "fast"}
	s.threadID.Store("parent")
	s.alive.Store(true)
	done := make(chan error, 1)
	go func() { done <- s.SetServiceTier("default") }()
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(waitForWrittenJSONLine(t, stdin)), &request); err != nil {
		t.Fatal(err)
	}
	s.handleResponse(rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(`{}`)})
	// An unrelated thread's acknowledgement cannot confirm our setting.
	s.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"child","threadSettings":{"serviceTier":null}}`))
	cancel()
	select {
	case err := <-done:
		if err == nil || s.GetServiceTier() != "fast" {
			t.Fatal("RPC acknowledgement alone reported a tier change")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled settings request did not finish")
	}
}
