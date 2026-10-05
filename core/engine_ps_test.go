package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type psMessage struct {
	text      string
	messageID string
	images    []ImageAttachment
	files     []FileAttachment
}

type psAttachmentSession struct {
	*cujAgentSession
	calls   chan psMessage
	sendErr error
}

func newPsAttachmentSession() *psAttachmentSession {
	return &psAttachmentSession{
		cujAgentSession: newCUJAgentSession(),
		calls:           make(chan psMessage, 8),
	}
}

func (s *psAttachmentSession) Send(text, messageID string, images []ImageAttachment, files []FileAttachment) error {
	s.calls <- psMessage{text: text, messageID: messageID, images: images, files: files}
	return s.sendErr
}

func TestReceiveMessage_PsPreservesAttachments(t *testing.T) {
	images := []ImageAttachment{
		{MimeType: "image/png", FileName: "chart.png", Data: []byte("chart")},
		{MimeType: "image/jpeg", FileName: "screen.jpg", Data: []byte("screen")},
	}
	files := []FileAttachment{{FileName: "notes.txt", MimeType: "text/plain", Data: []byte("notes")}}
	for _, tc := range []struct {
		name, content, wantText string
		images                  []ImageAttachment
		files                   []FileAttachment
	}{
		{"images and file", "/ps use these attachments", "use these attachments", images, files},
		{"btw alias", "/btw use this chart", "use this chart", images, nil},
		{"uppercase", "/PS use this chart", "use this chart", images, nil},
		{"prefix", "/bt use this chart", "use this chart", images, nil},
		{"rich text line break", "/ps\nuse this chart", "use this chart", images, nil},
		{"CRLF line break", "/btw\r\nuse this chart", "use this chart", images, nil},
		{"configured alias", "supplement use this chart", "use this chart", images, nil},
		{"image only", "/ps", "", images, nil},
		{"file only", "/ps", "", nil, files},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			sess := newPsAttachmentSession()
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			e.AddAlias("supplement", "/ps")
			key := "test:ps-attachments"
			state := &interactiveState{agentSession: sess, platform: p}
			e.interactiveStates[key] = state
			session := e.sessions.GetOrCreateActive(key)
			gen, ok := session.TryLock()
			if !ok {
				t.Fatal("lock session")
			}
			defer session.Unlock(gen)

			e.ReceiveMessage(p, &Message{
				SessionKey: key, MessageID: "ps-image-message", Content: tc.content,
				Images: tc.images, Files: tc.files, ReplyCtx: "ctx",
			})
			select {
			case got := <-sess.calls:
				want := psMessage{text: tc.wantText, messageID: "ps-image-message", images: tc.images, files: tc.files}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("supplement delivered = %#v, want %#v", got, want)
				}
			default:
				t.Fatalf("supplement was not sent to the running task; replies: %v", p.getSent())
			}
			if got := p.getSent(); len(got) != 1 || got[0] != e.i18n.T(MsgPsSent) {
				t.Fatalf("supplement acknowledgment = %v", got)
			}
			if len(state.pendingMessages) != 0 {
				t.Fatal("supplement was also queued for a later turn")
			}
		})
	}
}

func TestSplitCommandArgs_RichTextLineBreaks(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"/ps\nsee this", []string{"/ps", "see", "this"}},
		{"/ps\r\nsee this", []string{"/ps", "see", "this"}},
		{"/ps \"keep\nthese lines\"", []string{"/ps", "keep\nthese lines"}},
		{"/workspace bind '/path with spaces'", []string{"/workspace", "bind", "/path with spaces"}},
	} {
		if got := splitCommandArgs(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCommandArgs(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestReceiveMessage_PsImagesRespectRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		busy     bool
		disabled bool
		sendErr  error
		want     MsgKey
	}{
		{name: "idle", want: MsgPsNoSession},
		{name: "disabled", busy: true, disabled: true, want: MsgCommandDisabled},
		{name: "send failed", busy: true, sendErr: errors.New("backend unavailable"), want: MsgPsSendFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			sess := newPsAttachmentSession()
			sess.sendErr = tc.sendErr
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			key := "test:ps-rejected"
			state := &interactiveState{agentSession: sess, platform: p}
			e.interactiveStates[key] = state
			if tc.busy {
				session := e.sessions.GetOrCreateActive(key)
				gen, _ := session.TryLock()
				defer session.Unlock(gen)
			}
			if tc.disabled {
				e.SetDisabledCommands([]string{"ps"})
			}
			e.ReceiveMessage(p, &Message{
				SessionKey: key, Content: "/ps inspect this", ReplyCtx: "ctx",
				Images: []ImageAttachment{{MimeType: "image/png", Data: []byte("image")}},
			})
			want := e.i18n.T(tc.want)
			if tc.disabled {
				want = e.i18n.Tf(tc.want, "/ps")
			}
			if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], want) {
				t.Fatalf("rejection = %v, want %q", got, want)
			}
			if !tc.busy || tc.disabled {
				if len(sess.calls) != 0 {
					t.Fatal("rejected supplement reached the agent")
				}
			}
			if len(state.pendingMessages) != 0 {
				t.Fatal("rejected supplement was queued for a later turn")
			}
		})
	}
}

func TestReceiveMessage_ImageCaptionStillBypassesOtherCommands(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newPsAttachmentSession()
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	key := "test:image-caption"
	state := &interactiveState{agentSession: sess, platform: p}
	e.interactiveStates[key] = state
	session := e.sessions.GetOrCreateActive(key)
	gen, _ := session.TryLock()
	defer session.Unlock(gen)
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Content: "/help explain the screenshot", ReplyCtx: "ctx",
		Images: []ImageAttachment{{MimeType: "image/png", Data: []byte("image")}},
	})
	if got := p.getSent(); len(got) != 1 || got[0] != e.i18n.T(MsgMessageQueued) {
		t.Fatalf("ordinary image caption should retain normal queue behavior: %v", got)
	}
	if len(state.pendingMessages) != 1 || len(state.pendingMessages[0].images) != 1 || len(sess.calls) != 0 {
		t.Fatal("ordinary image caption was consumed as a command")
	}
}
