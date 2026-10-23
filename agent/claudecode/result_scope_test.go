package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func scopedTestSession() (*claudeSession, *bytes.Buffer) {
	var input bytes.Buffer
	s := &claudeSession{stdin: nopWriteCloser{&input}, events: make(chan core.Event, 32), ctx: context.Background()}
	s.sessionID.Store("test-session")
	s.alive.Store(true)
	return s, &input
}

type nopWriteCloser struct{ *bytes.Buffer }

func (w nopWriteCloser) Close() error { return nil }

func sendScopedTestMessage(t *testing.T, s *claudeSession, input *bytes.Buffer, messageID string) string {
	t.Helper()
	input.Reset()
	if err := s.Send("continue", messageID, nil, nil); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(input.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	id, _ := sent["uuid"].(string)
	if id == "" {
		t.Fatal("outgoing user message has no correlation UUID")
	}
	return id
}

func scopedTestEvent(t *testing.T, s *claudeSession) core.Event {
	t.Helper()
	select {
	case event := <-s.events:
		return event
	default:
		t.Fatal("expected a synchronously emitted event")
		return core.Event{}
	}
}

func TestResultScope_ResumeNotificationDoesNotCompleteUserTurn(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "platform-message")
	s.handleReadLoopLine(`{"type":"command_lifecycle","command_uuid":"` + id + `","state":"queued"}`)
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "num_turns": float64(0), "result": "", "origin": map[string]any{"kind": "task-notification"}})
	if len(s.events) != 0 {
		t.Fatalf("empty resume notification emitted an event: %+v", scopedTestEvent(t, s))
	}
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "done", "user_message_uuid": id})
	if e := scopedTestEvent(t, s); !e.Done || e.Background || e.Content != "done" {
		t.Fatalf("real result = %+v", e)
	}
}

func TestResultScope_EmptyTaskNotificationDoesNotMutateCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		correlated bool
		binding    string
	}{
		{name: "unbound"},
		{name: "foreign UUID on legacy stream", binding: "foreign"},
		{name: "foreground UUID", correlated: true, binding: "foreground"},
		{name: "batched foreground and supplement UUIDs", correlated: true, binding: "batch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, input := scopedTestSession()
			id := sendScopedTestMessage(t, s, input, "user")
			ids := []any{id}
			if tc.binding == "batch" {
				ids = append(ids, sendScopedTestMessage(t, s, input, ""))
			}
			if tc.correlated {
				s.handleCommandLifecycle(map[string]any{"command_uuid": id, "state": "queued"})
			}
			before := s.turns
			before.pending = maps.Clone(before.pending)
			before.finished = maps.Clone(before.finished)
			before.results = maps.Clone(before.results)
			before.finishedOrder = slices.Clone(before.finishedOrder)
			before.resultOrder = slices.Clone(before.resultOrder)

			notification := map[string]any{
				"type": "result", "subtype": "success", "uuid": "notification-result",
				"result": "", "num_turns": float64(0),
				"origin": map[string]any{"kind": "task-notification"},
			}
			switch tc.binding {
			case "foreign":
				notification["user_message_uuid"] = "foreign-input"
			case "foreground":
				notification["user_message_uuid"] = id
			case "batch":
				notification["user_message_uuid"] = ids[len(ids)-1]
				notification["user_message_uuids"] = ids
			}
			s.handleResult(notification)
			if len(s.events) != 0 {
				t.Fatalf("empty notification emitted an event: %+v", scopedTestEvent(t, s))
			}
			if !reflect.DeepEqual(s.turns, before) {
				t.Fatalf("ignored notification changed correlation state:\nbefore: %+v\nafter: %+v", before, s.turns)
			}

			// A real result may also be empty and use zero model turns, as with
			// a local command. Its UUIDs must still complete the foreground.
			s.handleResult(map[string]any{
				"type": "result", "subtype": "success", "uuid": "foreground-result",
				"result": "", "num_turns": float64(0),
				"user_message_uuid": ids[len(ids)-1], "user_message_uuids": ids,
			})
			if e := scopedTestEvent(t, s); e.Type != core.EventResult || !e.Done || e.Background || e.Content != "" {
				t.Fatalf("legitimate empty foreground result = %+v", e)
			}
		})
	}
}

func TestResultScope_BatchedUserUUIDsAndLegitimateEmptyResult(t *testing.T) {
	s, input := scopedTestSession()
	first := sendScopedTestMessage(t, s, input, "first")
	second := sendScopedTestMessage(t, s, input, "") // /ps supplement
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "", "num_turns": float64(0), "user_message_uuid": second, "user_message_uuids": []any{first, second}})
	if e := <-s.events; !e.Done || e.Background {
		t.Fatalf("legitimate empty result = %+v", e)
	}
}

func TestResultScope_ForeignResultDoesNotConsumePendingRequest(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "first")
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "background done", "user_message_uuid": "another-request"})
	if e := <-s.events; !e.Background {
		t.Fatalf("foreign result = %+v", e)
	}
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "real done", "user_message_uuid": id})
	if e := <-s.events; !e.Done || e.Background {
		t.Fatalf("foreground result = %+v", e)
	}
}

func TestResultScope_UnboundNotificationOnCorrelatedStream(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "new-request")
	s.handleReadLoopLine(`{"type":"command_lifecycle","command_uuid":"` + id + `","state":"queued"}`)
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "result": "", "num_turns": float64(0)})
	if e := <-s.events; !e.Background {
		t.Fatalf("unbound result completed request: %+v", e)
	}
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": id, "result": "finished"})
	if e := <-s.events; e.Background || !e.Done {
		t.Fatalf("request did not finish: %+v", e)
	}
}

func TestResultScope_CompactionDoesNotConsumeCorrelation(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "request")
	s.handleResult(map[string]any{"type": "result", "subtype": "compact", "user_message_uuid": id})
	if e := <-s.events; e.Done {
		t.Fatal("intermediate compaction completed request")
	}
	s.handleResult(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": id, "result": "done"})
	if e := <-s.events; e.Background || !e.Done {
		t.Fatalf("final result = %+v", e)
	}
}

func TestResultScope_LateSupplementCannotCompleteNewRequest(t *testing.T) {
	s, input := scopedTestSession()
	first := sendScopedTestMessage(t, s, input, "first")
	supplement := sendScopedTestMessage(t, s, input, "")
	s.handleResult(map[string]any{"type": "result", "user_message_uuid": first, "result": "first done"})
	<-s.events
	current := sendScopedTestMessage(t, s, input, "next")
	s.handleResult(map[string]any{"type": "result", "user_message_uuid": supplement, "result": "late supplement"})
	if e := <-s.events; !e.Background {
		t.Fatalf("late supplement completed new request: %+v", e)
	}
	s.handleResult(map[string]any{"type": "result", "user_message_uuid": current, "result": "next done"})
	if e := <-s.events; e.Background || !e.Done {
		t.Fatalf("new result = %+v", e)
	}
}

func TestResultScope_LegacyAndSessionFailureStillTerminate(t *testing.T) {
	for _, failure := range []bool{false, true} {
		s, input := scopedTestSession()
		id := sendScopedTestMessage(t, s, input, "request")
		if failure {
			s.handleReadLoopLine(`{"type":"command_lifecycle","command_uuid":"` + id + `","state":"started"}`)
		}
		s.handleResult(map[string]any{"type": "result", "subtype": "success", "is_error": failure, "errors": []any{"provider unavailable"}})
		if e := <-s.events; e.Background || !e.Done {
			t.Fatalf("terminal result = %+v", e)
		}
	}
}

func TestResultScope_CancelAndDuplicateResult(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "request")
	s.handleReadLoopLine(`{"type":"command_lifecycle","command_uuid":"` + id + `","state":"cancelled"}`)
	if e := <-s.events; !e.Done || e.Background || e.Content != "context canceled" {
		t.Fatalf("cancel = %+v", e)
	}
	s.handleResult(map[string]any{"type": "result", "user_message_uuid": id, "is_error": true})
	select {
	case e := <-s.events:
		t.Fatalf("duplicate completion emitted: %+v", e)
	default:
	}
}

func TestResultScope_RecordedResumeSequence(t *testing.T) {
	s, input := scopedTestSession()
	id := sendScopedTestMessage(t, s, input, "user")
	data, err := os.ReadFile("testdata/resume_task_notification.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		s.handleReadLoopLine(strings.ReplaceAll(line, "CLIENT_UUID", id))
	}
	if len(s.events) != 1 {
		t.Fatalf("got %d events, want only the foreground result", len(s.events))
	}
	if event := scopedTestEvent(t, s); event.Background || !event.Done || event.Content != "Context ready" {
		t.Fatalf("user result = %+v", event)
	}
}
