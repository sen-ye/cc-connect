package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type recoveryProgressPlatform struct {
	stubPlatformEngine
	err      error
	attempts int
	starts   int
	startIDs []string
}

type classifiedProgressFailure struct{ kind MessageErrorKind }

func (e classifiedProgressFailure) Error() string                      { return "delivery failed: private-error-detail" }
func (e classifiedProgressFailure) MessageErrorKind() MessageErrorKind { return e.kind }

func TestCompactProgressWriter_RecoveryIsBoundedAndNeverLeaksDetails(t *testing.T) {
	for _, kind := range []MessageErrorKind{MessageErrorUnknown, MessageErrorPermanent, MessageErrorTargetUnavailable} {
		t.Run(string(kind), func(t *testing.T) {
			p := &recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangChinese, nil)
			w.Append("accepted progress")
			p.err = classifiedProgressFailure{kind}
			for i := 0; i < 20; i++ {
				w.retryAt = time.Time{}
				if !w.AppendEvent(ProgressEntryToolUse, "raw-tool-detail", "shell", "raw-tool-detail") {
					t.Fatal("failure requested raw tool fallback")
				}
			}
			if p.attempts > 4 || p.starts > 2 {
				t.Fatalf("unbounded retry/rebuild: attempts=%d starts=%d", p.attempts, p.starts)
			}
			notices := 0
			for _, sent := range p.getSent() {
				if _, ok := ParseProgressCardPayload(sent); ok {
					continue
				}
				if strings.Contains(sent, "raw-tool-detail") || strings.Contains(sent, "private-error-detail") {
					t.Fatalf("private details leaked: %q", sent)
				}
				if sent == NewI18n(LangChinese).T(MsgProgressUnavailable) {
					notices++
				}
			}
			if notices != 1 {
				t.Fatalf("notices=%d, want 1", notices)
			}
		})
	}
}

func TestCompactProgressWriter_TransientOutageRecoversAfterBackoff(t *testing.T) {
	p := &recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
	w.Append("accepted progress")
	p.err = classifiedProgressFailure{MessageErrorTransient}
	for i := 0; i < 4; i++ {
		w.retryAt = time.Time{}
		if !w.Append("buffered progress") {
			t.Fatal("temporary outage requested fallback")
		}
	}
	attempts := p.attempts
	for i := 0; i < 50; i++ {
		w.Append("latest progress")
	}
	if p.attempts != attempts {
		t.Fatal("backoff did not throttle updates")
	}
	p.err = nil
	w.retryAt = time.Time{}
	w.Append("recovered progress")
	w.Finalize(ProgressCardStateCompleted)
	sent := p.getSent()
	payload, ok := ParseProgressCardPayload(sent[len(sent)-1])
	if !ok || payload.State != ProgressCardStateCompleted || !strings.Contains(strings.Join(payload.Entries, "\n"), "recovered progress") {
		t.Fatalf("card did not recover: %v", sent)
	}
}

type replacedProgressPlatform struct {
	recoveryProgressPlatform
	missingOnce bool
}

func (p *replacedProgressPlatform) UpdateMessage(ctx context.Context, target any, content string) error {
	if !p.missingOnce {
		p.missingOnce = true
		return classifiedProgressFailure{MessageErrorTargetUnavailable}
	}
	return p.recoveryProgressPlatform.UpdateMessage(ctx, target, content)
}

func TestCompactProgressWriter_RecreatesMissingCardWithLatestProgress(t *testing.T) {
	p := &replacedProgressPlatform{recoveryProgressPlatform: recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
	w.Append("accepted progress")
	if !w.Append("latest progress") || !w.Finalize(ProgressCardStateCompleted) {
		t.Fatal("card recreation requested fallback")
	}
	if p.starts != 2 {
		t.Fatalf("created %d cards, want 2", p.starts)
	}
	sent := p.getSent()
	payload, ok := ParseProgressCardPayload(sent[len(sent)-1])
	if !ok || payload.State != ProgressCardStateCompleted || len(payload.Entries) != 2 {
		t.Fatalf("recreated card lost progress: %v", sent)
	}
}

func TestCompactProgressWriter_APITimeoutBoundsLongParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	w := &compactProgressWriter{ctx: ctx}
	callCtx, finish := w.withAPITimeout()
	defer finish()
	deadline, ok := callCtx.Deadline()
	if !ok || time.Until(deadline) > compactProgressAPITimeout {
		t.Fatal("API request inherited an unbounded parent timeout")
	}
}

type recoveryJourneySession struct{ *controllableAgentSession }

func (s *recoveryJourneySession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.events <- Event{Type: EventToolUse, ToolName: "shell", ToolInput: "accepted command"}
	if strings.Contains(prompt, "fault") {
		s.events <- Event{Type: EventToolResult, ToolName: "shell", ToolResult: "raw-tool-detail"}
	}
	s.events <- Event{Type: EventToolResult, ToolName: "shell", ToolResult: "later result"}
	s.events <- Event{Type: EventResult, Content: "finished: " + prompt, Done: true}
	return nil
}

type recoveryJourneyPlatform struct {
	recoveryProgressPlatform
	fault   error
	faulted bool
}

func (p *recoveryJourneyPlatform) UpdateMessage(ctx context.Context, target any, content string) error {
	if !p.faulted && strings.Contains(content, "raw-tool-detail") {
		p.faulted = true
		return p.fault
	}
	return p.recoveryProgressPlatform.UpdateMessage(ctx, target, content)
}

func testProgressRecoveryJourney(t *testing.T) {
	for _, kind := range []MessageErrorKind{MessageErrorTransient, MessageErrorUnknown, MessageErrorPermanent} {
		t.Run(string(kind), func(t *testing.T) {
			p := &recoveryJourneyPlatform{recoveryProgressPlatform: recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}, fault: classifiedProgressFailure{kind}}
			s := &recoveryJourneySession{newControllableSession("recovery-journey")}
			e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
			e.SetStreamPreviewCfg(StreamPreviewCfg{})
			e.SetReplyFooterEnabled(false)
			e.SetDisplayConfig(DisplayCfg{Mode: "full", ToolMessages: true})
			defer e.Stop()
			for _, prompt := range []string{"first task", "fault task", "next task"} {
				p.clearSent()
				e.ReceiveMessage(p, &Message{SessionKey: "test:topic:user", Platform: "test", UserID: "user", Content: prompt, ReplyCtx: "topic"})
				deadline := time.Now().Add(3 * time.Second)
				for !strings.Contains(strings.Join(p.getSent(), "\n"), "finished: "+prompt) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				sent := p.getSent()
				if !strings.Contains(strings.Join(sent, "\n"), "finished: "+prompt) {
					t.Fatalf("final reply missing: %v", sent)
				}
				var progress *ProgressCardPayload
				for _, message := range sent {
					if payload, ok := ParseProgressCardPayload(message); ok {
						progress = payload
						continue
					}
					if strings.Contains(message, "raw-tool-detail") || strings.Contains(message, "🧾") || strings.Contains(message, "private-error-detail") {
						t.Fatalf("raw progress leaked: %q", message)
					}
				}
				if !(kind == MessageErrorPermanent && strings.Contains(prompt, "fault")) && (progress == nil || progress.State != ProgressCardStateCompleted) {
					t.Fatalf("missing completed progress card: %v", sent)
				}
			}
		})
	}
}

func (p *recoveryProgressPlatform) ProgressStyle() string             { return "card" }
func (p *recoveryProgressPlatform) SupportsProgressCardPayload() bool { return true }
func (p *recoveryProgressPlatform) SendPreviewStart(ctx context.Context, target any, content string) (any, error) {
	p.starts++
	p.startIDs = append(p.startIDs, ProgressRequestID(ctx))
	if err := p.UpdateMessage(ctx, target, content); err != nil {
		return nil, err
	}
	return "progress-card", nil
}

func TestCompactProgressWriter_CreationRetryKeepsIdempotencyKey(t *testing.T) {
	p := &recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, err: io.ErrUnexpectedEOF}
	w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
	w.Append("initial progress")
	p.err = nil
	w.Append("newest progress")
	w.Finalize(ProgressCardStateCompleted)
	if len(p.startIDs) != 2 || p.startIDs[0] == "" || p.startIDs[0] != p.startIDs[1] {
		t.Fatalf("creation retry changed identity: %v", p.startIDs)
	}
}
func (p *recoveryProgressPlatform) UpdateMessage(ctx context.Context, target any, content string) error {
	p.attempts++
	if p.err != nil {
		return p.err
	}
	return p.stubPlatformEngine.Send(ctx, target, content)
}

func TestCompactProgressWriter_PublishFailureNeverReplaysRawTools(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"internal_2200", errors.New("patch message code=2200 msg=Internal Error")},
		{"timeout", context.DeadlineExceeded},
		{"disconnect", io.ErrUnexpectedEOF},
		{"unknown", errors.New("unexpected platform failure")},
	} {
		for _, stage := range []string{"start", "update", "finalize"} {
			t.Run(failure.name+"/"+stage, func(t *testing.T) {
				p := &recoveryProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
				w := newCompactProgressWriter(context.Background(), p, "topic", "agent", LangEnglish, nil)
				if stage != "start" {
					w.Append("accepted progress")
				}
				p.err = failure.err
				if stage == "finalize" {
					if !w.Finalize(ProgressCardStateCompleted) {
						t.Fatal("finalization failure requested a raw fallback")
					}
				} else if !w.AppendEvent(ProgressEntryToolUse, "raw-tool-detail", "shell", "raw-tool-detail") {
					t.Fatal("publish failure requested a raw tool message")
				}
				p.err = nil
				if !w.Append("later progress") || !w.Finalize(ProgressCardStateCompleted) {
					t.Fatal("platform recovery did not restore progress rendering")
				}
				sent := p.getSent()
				payload, ok := ParseProgressCardPayload(sent[len(sent)-1])
				if !ok || payload.State != ProgressCardStateCompleted || !strings.Contains(strings.Join(payload.Entries, "\n"), "later progress") {
					t.Fatalf("recovered card missing latest progress: %v", sent)
				}
			})
		}
	}
}
