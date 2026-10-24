package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// These seven argument payloads were read from the real "cursor gemini，sim doc"
// Feishu thread. They are the JSON that the old adapter displayed verbatim.
func TestCursorToolInput_RealSimDocGlobMessages(t *testing.T) {
	data, err := os.ReadFile("testdata/sim_doc_glob.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal(data, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 7 {
		t.Fatalf("actual fixture count = %d", len(messages))
	}
	s := newTestSession("ask")
	defer s.cancel()
	for _, message := range messages {
		message["type"] = "tool_call"
		message["subtype"] = "started"
		s.handleEvent(message)
		event := <-s.Events()
		args := message["tool_call"].(map[string]any)["globToolCall"].(map[string]any)["args"].(map[string]any)
		want := filepath.Join(args["targetDirectory"].(string), args["globPattern"].(string))
		if event.Type != core.EventToolUse || event.ToolName != "Glob" || event.ToolInput != want {
			t.Fatalf("actual message produced %+v; want Glob %q", event, want)
		}
		if strings.Contains(event.ToolInput, "globPattern") || strings.HasPrefix(event.ToolInput, "{") {
			t.Fatal("raw JSON leaked into tool summary")
		}
	}
}

func TestCursorToolInput_AliasesAndCommandPreservation(t *testing.T) {
	dir := filepath.Join("workspace", "sim-doc")
	command := "python3 - <<'PY'\nprint(\"quoted command\")\nPY"
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"Glob", map[string]any{"glob_pattern": "**/*.md", "target_directory": dir}, filepath.Join(dir, "**/*.md")},
		{"Glob", map[string]any{"pattern": "*.go"}, "*.go"},
		{"Glob", map[string]any{"globPattern": "", "pattern": "*.go", "targetDirectory": dir}, filepath.Join(dir, "*.go")},
		{"Grep", map[string]any{"pattern": "RoboDojo|unify_sim", "path": dir}, dir + " · RoboDojo|unify_sim"},
		{"Grep", map[string]any{"pattern": "RoboDojo"}, "RoboDojo"},
		{"Read", map[string]any{"filePath": "README.md"}, "README.md"},
		{"List", map[string]any{"path": dir}, dir},
		{"Search", map[string]any{"query": "RoboDojo evaluation"}, "RoboDojo evaluation"},
		{"WebFetch", map[string]any{"url": "https://example.com/eval"}, "https://example.com/eval"},
		{"Bash", map[string]any{"command": command}, command},
	} {
		t.Run(tc.name+tc.want, func(t *testing.T) {
			if got := extractToolInput(tc.name, map[string]any{"args": tc.args}); got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
		})
	}
}
