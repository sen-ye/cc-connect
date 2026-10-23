package codex

import (
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Async questions and ordinary progress share item/completed. Keep structured
// questions interactive without letting child or stale turns reach the parent.
func TestAppServerSession_AsyncQuestionsPreserveTurnIsolationAndProgress(t *testing.T) {
	s := &appServerSession{events: make(chan core.Event, 8), currentTurn: "active"}
	s.threadID.Store("parent")
	question := map[string]any{
		"type": "agentMessage", "id": "question", "delivery": "async",
		"text": "Which database?",
		"questions": []any{map[string]any{
			"title": "Which database?", "options": []string{"PostgreSQL", "SQLite"},
		}},
	}
	notify := func(threadID, turnID string, item map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
		if err != nil {
			t.Fatal(err)
		}
		s.handleNotification("item/completed", raw)
	}
	notify("child", "active", question)
	notify("parent", "previous", question)
	if len(s.events) != 0 {
		t.Fatal("child or stale-turn question reached the parent")
	}

	notify("parent", "active", question)
	select {
	case event := <-s.events:
		if event.Type != core.EventUserInputRequest || event.RequestID != "question" ||
			len(event.Questions) != 1 || len(event.Questions[0].Options) != 2 ||
			event.Questions[0].Options[1].Label != "SQLite" {
			t.Fatalf("question event = %#v", event)
		}
	default:
		t.Fatal("active question was buffered instead of sent immediately")
	}

	notify("parent", "active", map[string]any{
		"type": "agentMessage", "content": []any{map[string]any{"type": "text", "text": "Checking the schema"}},
	})
	select {
	case event := <-s.events:
		if event.Type != core.EventText || event.Content != "Checking the schema" {
			t.Fatalf("progress event = %#v", event)
		}
	default:
		t.Fatal("ordinary progress after the question was not delivered immediately")
	}
	if len(s.events) != 0 || len(s.pendingMsgs) != 0 || s.currentTurn != "active" {
		t.Fatal("question changed the active turn or duplicated text into pending progress")
	}
}
