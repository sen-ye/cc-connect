package core

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReplyFootersIncludeFullAgentSessionID(t *testing.T) {
	const id = "01234567-89ab-4cde-8f01-23456789abcd"
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		t.Run(string(lang), func(t *testing.T) {
			agent := &stubFooterAgent{model: "test-model", workDir: "/workspace/project"}
			e := NewEngine("test", agent, nil, "", lang)
			e.SetReplyFooterEnabled(true)
			session := &controllableAgentSession{
				sessionID: id, model: "test-model", workDir: "/workspace/project",
				contextUsage: &ContextUsage{ContextWindow: 100000, CachedInputTokens: 1000, UsedTokens: 1200},
			}
			render := map[string]func() string{
				"legacy":     func() string { return e.buildReplyFooter(agent, session, "", "") },
				"statusline": func() string { return e.buildClaudeStatusLineFooter(agent, session, "") },
				"rich":       func() string { return e.composeRichStatusFooter(false, time.Now(), agent, session, "") },
			}
			for name, footer := range render {
				t.Run(name, func(t *testing.T) {
					got := footer()
					lines := strings.Split(got, "\n")
					if want := e.i18n.Tf(MsgReplyFooterSessionID, id); lines[len(lines)-1] != want {
						t.Fatalf("last footer line = %q, want %q", lines[len(lines)-1], want)
					}
					if strings.Count(got, id) != 1 || !strings.Contains(got, "test-model") || !strings.Contains(got, "/workspace/project") {
						t.Fatalf("footer must retain model/path and show the full ID once: %q", got)
					}
				})
			}
		})
	}
}

func TestReplyFooterSessionID_FollowsLiveSession(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.SetReplyFooterEnabled(true)
	session := &controllableAgentSession{}
	for _, id := range []string{"", ContinueSession, "  ", "first-agent-session-123456789", "second-agent-session-987654321"} {
		session.sessionID = id
		got := e.composeRichStatusFooter(false, time.Now(), e.agent, session, "")
		if strings.TrimSpace(id) == "" || id == ContinueSession {
			if strings.Contains(got, "Session ID:") {
				t.Fatalf("unknown ID must not produce a placeholder: %q", got)
			}
		} else if !strings.HasSuffix(got, "Session ID: "+id) {
			t.Fatalf("footer does not reflect the current session ID: %q", got)
		}
	}
	if got := e.replyFooterSessionID(nil); got != "" {
		t.Fatalf("nil session ID line = %q", got)
	}
	if got := e.composeRichStatusFooter(true, time.Now(), e.agent, session, ""); got != "" {
		t.Fatalf("streaming footer should keep its existing hidden behavior: %q", got)
	}
}

func TestReplyFooterSessionID_RespectsFooterToggle(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	session := &controllableAgentSession{
		sessionID: "full-agent-session-id", contextUsage: &ContextUsage{ContextWindow: 100000, CachedInputTokens: 1000},
	}
	for _, enabled := range []bool{false, true} {
		e.SetReplyFooterEnabled(enabled)
		e.SetShowContextIndicator(false)
		e.SetShowWorkdirIndicator(false)
		for name, got := range map[string]string{
			"legacy":     e.buildReplyFooter(e.agent, session, "", ""),
			"statusline": e.buildClaudeStatusLineFooter(e.agent, session, ""),
			"rich":       e.composeRichStatusFooter(false, time.Now(), e.agent, session, ""),
		} {
			if !enabled && got != "" {
				t.Errorf("%s: disabled footer = %q", name, got)
			}
			if enabled && !strings.Contains(got, "Session ID: full-agent-session-id") {
				t.Errorf("%s: hiding other indicators hid session ID: %q", name, got)
			}
		}
	}
}

// The platform exposes the rendered card text through the ordinary sender
// recording, so CUJs assert the platform-visible session ID.
type sessionFooterPlatform struct{ stubPlatformEngine }

func (p *sessionFooterPlatform) BuildRichCard(status CardStatus, _ string, _ []ToolStep, body string, _ bool, footer string) string {
	return fmt.Sprintf("card-status:%s\n%s\n%s", status, body, footer)
}
func (p *sessionFooterPlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (any, error) {
	return "card", p.Send(ctx, replyCtx, content)
}
func (p *sessionFooterPlatform) UpdateMessage(ctx context.Context, replyCtx any, content string) error {
	return p.Send(ctx, replyCtx, content)
}

// Some agents only learn their native ID after the first message is sent.
type sessionFooterAgentSession struct {
	*cujAgentSession
	id    string
	ready atomic.Bool
}

func (s *sessionFooterAgentSession) CurrentSessionID() string {
	if !s.ready.Load() {
		return ""
	}
	return s.id
}
func (s *sessionFooterAgentSession) Send(prompt, messageID string, images []ImageAttachment, files []FileAttachment) error {
	s.ready.Store(true)
	return s.cujAgentSession.Send(prompt, messageID, images, files)
}
