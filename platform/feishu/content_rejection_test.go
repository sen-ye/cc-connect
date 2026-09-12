package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestMessageContentRejectionIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "test-token"})
			return
		}
		writeJSON(t, w, map[string]any{"code": 230028, "msg": "The messages do NOT pass the audit"})
	}))
	defer srv.Close()
	p := &Platform{platformName: "feishu", domain: srv.URL, useInteractiveCard: true,
		client: lark.NewClient("test-app", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"reply":  func() error { return p.replyMessage(ctx, replyContext{messageID: "root"}, "text", `{"text":"test"}`) },
		"create": func() error { return p.createMessage(ctx, "chat", "text", `{"text":"test"}`, "send") },
		"preview reply": func() error {
			_, err := p.SendPreviewStart(ctx, replyContext{chatID: "chat", messageID: "root"}, "progress")
			return err
		},
		"preview create": func() error { _, err := p.SendPreviewStart(ctx, replyContext{chatID: "chat"}, "progress"); return err },
		"patch":          func() error { return p.patchCardMessage(ctx, "message", `{}`) },
		"card entity": func() error {
			return classifyFeishuCardAPIError("update card entity", 230028, "The messages do NOT pass the audit")
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			var rejection interface{ ContentRejected() bool }
			if !errors.As(err, &rejection) || !rejection.ContentRejected() {
				t.Fatalf("content rejection must survive error wrapping: %v", err)
			}
		})
	}
}
