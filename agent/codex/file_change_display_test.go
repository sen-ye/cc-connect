package codex

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAppServerSession_FileChangeDisplaysPathsInsteadOfRawPatchJSON(t *testing.T) {
	s := &appServerSession{events: make(chan core.Event, 4)}
	item := map[string]any{
		"type": "fileChange", "id": "patch-1", "status": "completed",
		"changes": []any{
			map[string]any{"path": "scoring/depth_service.py", "kind": map[string]any{"type": "update"}, "diff": strings.Repeat("+code with ``` and \\\"quotes\\\"\n", 1000)},
			map[string]any{"path": "scoring/run.py", "kind": map[string]any{"type": "add"}, "diff": "+new code"},
		},
	}
	s.handleItemStarted(item)
	event := <-s.events
	if event.ToolName != "Patch" || event.ToolInput != "scoring/depth_service.py\nscoring/run.py" {
		t.Fatalf("file change should display changed paths without patch JSON, got %q", truncate(event.ToolInput, 200))
	}
	s.handleItemCompleted(item)
	select {
	case event := <-s.events:
		if event.Type != core.EventToolResult || event.ToolName != "Patch" || event.ToolSuccess == nil || !*event.ToolSuccess || event.ToolResult != "scoring/depth_service.py\nscoring/run.py" {
			t.Fatalf("missing file-change result/status: %#v", event)
		}
	default:
		t.Fatal("file-change completion was dropped")
	}
}
