package turn_contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/agent/cursor"
	"github.com/chenhg5/cc-connect/core"
)

type cursorProgressPlatform struct {
	turnPlatform
	cardMu  sync.Mutex
	style   string
	opened  []string
	updated []string
}

func (p *cursorProgressPlatform) ProgressStyle() string             { return p.style }
func (p *cursorProgressPlatform) SupportsProgressCardPayload() bool { return true }
func (p *cursorProgressPlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.cardMu.Lock()
	defer p.cardMu.Unlock()
	p.opened = append(p.opened, content)
	return len(p.opened), nil
}
func (p *cursorProgressPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.cardMu.Lock()
	defer p.cardMu.Unlock()
	p.updated = append(p.updated, content)
	return nil
}

// The fixture is an actual agent-us Gemini 4 Argon stream (2026.10.22-27dcf5f),
// captured while it performed Glob and Grep in an isolated read-only workspace.
// Only the CLI subprocess and platform transport are replaced: parsing, engine
// event routing, and progress-card aggregation use production code.
func TestCursorActualGeminiStream_UsesOneReadableProgressCard(t *testing.T) {
	fixture, err := filepath.Abs("testdata/cursor_gemini_tools.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, style := range []string{"legacy", "card"} {
		t.Run(style, func(t *testing.T) {
			agent, err := cursor.New(map[string]any{
				"cmd":      []string{os.Args[0], "-test.run=^TestCursorProgressReplayCLI$", "--"},
				"work_dir": t.TempDir(), "mode": "ask", "model": "gemini-4-argon-high",
				"env": map[string]string{"CURSOR_PROGRESS_REPLAY": fixture},
			})
			if err != nil {
				t.Fatal(err)
			}
			platform := &cursorProgressPlatform{style: style}
			engine := core.NewEngine("cursor-replay", agent, []core.Platform{platform}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
			engine.SetStreamPreviewCfg(core.StreamPreviewCfg{Enabled: false})
			engine.SetDisplayConfig(core.DisplayCfg{ThinkingMessages: true, ToolMessages: true})
			t.Cleanup(func() { _ = engine.Stop() })
			engine.ReceiveMessage(platform, turnMessage("replay actual Gemini output"))
			deadline := time.Now().Add(6 * time.Second)
			var texts []string
			for time.Now().Before(deadline) {
				texts, _, _, _, _ = platform.snapshot()
				if strings.Contains(strings.Join(texts, "\n"), "CURSOR_DISPLAY_OK") {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !strings.Contains(strings.Join(texts, "\n"), "CURSOR_DISPLAY_OK") {
				t.Fatalf("missing final answer: %v", texts)
			}
			platform.cardMu.Lock()
			defer platform.cardMu.Unlock()
			if style == "legacy" {
				if len(texts) < 4 {
					t.Fatalf("expected separate thinking/tool messages: %v", texts)
				}
				return
			}
			if len(platform.opened) != 1 || len(texts) != 1 {
				t.Fatalf("progress fragmented: opens=%d, standalone=%v", len(platform.opened), texts)
			}
			updates := append(append([]string{}, platform.opened...), platform.updated...)
			seenGlob, seenGrep, completed := false, false, false
			for _, content := range updates {
				payload, ok := core.ParseProgressCardPayload(content)
				if !ok {
					t.Fatalf("invalid progress payload: %q", content)
				}
				for _, item := range payload.Items {
					if strings.Contains(item.Text, "globPattern") {
						t.Fatalf("raw argument JSON displayed: %s", item.Text)
					}
					if item.Tool == "Glob" && item.Text == "*.md" {
						seenGlob = true
					}
					if item.Tool == "Grep" && item.Text == "RoboDojo" {
						seenGrep = true
					}
				}
				completed = completed || payload.State == core.ProgressCardStateCompleted
			}
			if !seenGlob || !seenGrep || !completed {
				t.Fatalf("incomplete card: Glob=%v Grep=%v completed=%v", seenGlob, seenGrep, completed)
			}
		})
	}
}

func TestCursorProgressReplayCLI(t *testing.T) {
	fixture := os.Getenv("CURSOR_PROGRESS_REPLAY")
	if fixture == "" {
		return
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(data)
	os.Exit(0)
}
