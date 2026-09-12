package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type rejectedProgressContent struct{}

func (rejectedProgressContent) Error() string         { return "message content rejected" }
func (rejectedProgressContent) ContentRejected() bool { return true }

type contentRejectingProgressPlatform struct {
	stubPlatformEngine
	rejectAll bool
}

func (p *contentRejectingProgressPlatform) ProgressStyle() string             { return "card" }
func (p *contentRejectingProgressPlatform) SupportsProgressCardPayload() bool { return true }
func (p *contentRejectingProgressPlatform) SendPreviewStart(ctx context.Context, target any, content string) (any, error) {
	if err := p.UpdateMessage(ctx, target, content); err != nil {
		return nil, err
	}
	return "progress-card", nil
}
func (p *contentRejectingProgressPlatform) UpdateMessage(ctx context.Context, target any, content string) error {
	if p.rejectAll || strings.Contains(content, "blocked-detail") {
		return fmt.Errorf("update: %w", rejectedProgressContent{})
	}
	return p.stubPlatformEngine.Send(ctx, target, content)
}

func TestCompactProgressWriter_ContentRejectionKeepsCardUpdating(t *testing.T) {
	for _, stage := range []string{"start", "update", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			p := &contentRejectingProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
			if stage != "start" && !w.AppendEvent(ProgressEntryToolUse, "accepted command", "shell", "accepted command") {
				t.Fatal("initial progress failed")
			}
			if stage == "finalize" {
				w.minUpdateInterval = time.Hour
			}
			if !w.AppendEvent(ProgressEntryToolUse, "blocked-detail", "Patch", "blocked-detail") {
				t.Fatal("content rejection disabled the card and requested a raw-text fallback")
			}
			if stage == "finalize" && !w.Finalize(ProgressCardStateCompleted) {
				t.Fatal("content rejection prevented finalizing the card")
			}
			w.minUpdateInterval = 0
			if !w.AppendEvent(ProgressEntryToolResult, "later result", "shell", "later result") {
				t.Fatal("later tool result fell back to raw text")
			}
			sent := p.getSent()
			payload, ok := ParseProgressCardPayload(sent[len(sent)-1])
			if !ok {
				t.Fatal("last message is no longer a structured progress card")
			}
			joined := strings.Join(payload.Entries, "\n")
			if strings.Contains(joined, "blocked-detail") || !strings.Contains(joined, "omitted") || !strings.Contains(joined, "later result") {
				t.Fatalf("unexpected visible progress: %q", joined)
			}
			if stage != "start" && !strings.Contains(joined, "accepted command") {
				t.Fatal("previously accepted details were unnecessarily removed")
			}
		})
	}
}

func TestCompactProgressWriter_RejectedRedactionNeverReplaysRawDetails(t *testing.T) {
	p := &contentRejectingProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
	w.Append("accepted progress")
	p.rejectAll = true
	if !w.AppendEvent(ProgressEntryToolUse, "blocked-detail", "shell", "blocked-detail") {
		t.Fatal("rejected redaction must not replay the original content through fallback")
	}
	p.rejectAll = false
	if !w.Append("recovered progress") {
		t.Fatal("writer did not recover for subsequent progress")
	}
	for _, sent := range p.getSent() {
		if strings.Contains(sent, "blocked-detail") {
			t.Fatal("rejected details were sent again")
		}
	}
}

type progressJourneySession struct{ *controllableAgentSession }

func (s *progressJourneySession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.events <- Event{Type: EventToolUse, ToolName: "shell", ToolInput: "accepted command"}
	if strings.Contains(prompt, "rejected") {
		s.events <- Event{Type: EventToolUse, ToolName: "Patch", ToolInput: "blocked-detail"}
	}
	s.events <- Event{Type: EventToolResult, ToolName: "shell", ToolResult: "later result", ToolStatus: "completed"}
	s.events <- Event{Type: EventResult, Content: "finished: " + prompt, Done: true}
	return nil
}

func testProgressContentRejectionJourney(t *testing.T) {
	p := &contentRejectingProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := &progressJourneySession{newControllableSession("progress-journey")}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.SetStreamPreviewCfg(StreamPreviewCfg{})
	e.SetReplyFooterEnabled(false)
	e.SetDisplayConfig(DisplayCfg{Mode: "full", ToolMessages: true})
	defer e.Stop()
	for _, prompt := range []string{"first task", "rejected task", "next task"} {
		p.clearSent()
		e.ReceiveMessage(p, &Message{SessionKey: "test:topic:user", Platform: "test", UserID: "user", Content: prompt, ReplyCtx: "topic"})
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(strings.Join(p.getSent(), "\n"), "finished: "+prompt) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		sent := p.getSent()
		if !strings.Contains(strings.Join(sent, "\n"), "finished: "+prompt) {
			t.Fatalf("final response missing for %q: %v", prompt, sent)
		}
		var progress *ProgressCardPayload
		for _, content := range sent {
			if payload, ok := ParseProgressCardPayload(content); ok {
				progress = payload
			} else if strings.Contains(content, "blocked-detail") || strings.Contains(content, "🧾") {
				t.Fatalf("tool progress escaped into raw fallback: %q", content)
			}
		}
		if progress == nil || progress.State != ProgressCardStateCompleted || !strings.Contains(strings.Join(progress.Entries, "\n"), "later result") {
			t.Fatalf("user did not receive a completed process card with the later tool result: %+v", progress)
		}
	}
}
