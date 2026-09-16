package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestPatchCardMessage_RetriesInternalError2200(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "test-token"})
			return
		}
		if patches.Add(1) == 1 {
			writeJSON(t, w, map[string]any{"code": 2200, "msg": "Internal Error"})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer srv.Close()
	p := &Platform{platformName: "feishu", domain: srv.URL, useInteractiveCard: true,
		client: lark.NewClient("recovery-app", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
	}
	if err := p.patchCardMessage(context.Background(), "message", `{}`); err != nil {
		t.Fatalf("temporary 2200 should recover without disabling progress: %v", err)
	}
	if patches.Load() != 2 {
		t.Fatalf("patch attempts=%d, want 2", patches.Load())
	}
}

func TestMessageDeliveryErrorsAreClassified(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	for _, tc := range []struct {
		code int
		kind core.MessageErrorKind
	}{
		{2200, core.MessageErrorTransient}, {230020, core.MessageErrorTransient},
		{99991400, core.MessageErrorTransient}, {230028, core.MessageErrorContentRejected},
		{230011, core.MessageErrorTargetUnavailable}, {230027, core.MessageErrorPermanent},
		{230099, core.MessageErrorPermanent}, {123456, core.MessageErrorUnknown},
	} {
		for _, err := range []error{p.messageAPIError("patch", tc.code, "test"), classifyFeishuCardAPIError("update", tc.code, "test")} {
			var classified core.MessageErrorClassifier
			if !errors.As(err, &classified) || classified.MessageErrorKind() != tc.kind {
				t.Fatalf("code=%d classified incorrectly: %v", tc.code, err)
			}
			if got := isTransientError(err); got != (tc.kind == core.MessageErrorTransient) {
				t.Fatalf("code=%d retryable=%v", tc.code, got)
			}
		}
	}
	for _, tc := range []struct {
		status int
		kind   core.MessageErrorKind
	}{
		{0, core.MessageErrorTransient}, {408, core.MessageErrorTransient},
		{429, core.MessageErrorTransient}, {503, core.MessageErrorTransient},
		{404, core.MessageErrorTargetUnavailable}, {410, core.MessageErrorTargetUnavailable},
		{400, core.MessageErrorPermanent}, {403, core.MessageErrorPermanent},
	} {
		err := &messageHTTPError{operation: "update card", status: tc.status}
		if err.MessageErrorKind() != tc.kind {
			t.Fatalf("HTTP %d kind=%s", tc.status, err.MessageErrorKind())
		}
	}
}

func TestMessageRetriesPreserveIdempotencyKey(t *testing.T) {
	for _, mode := range []string{"reply", "create", "preview_reply", "preview_create"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			ids := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "test-token"})
					return
				}
				var body struct {
					UUID string `json:"uuid"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				ids <- body.UUID
				if requests.Add(1) == 1 {
					writeJSON(t, w, map[string]any{"code": 2200, "msg": "Internal Error"})
					return
				}
				writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "created-message"}})
			}))
			defer srv.Close()
			p := &Platform{platformName: "feishu", domain: srv.URL, useInteractiveCard: true,
				client: lark.NewClient("idempotent-"+mode, "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
			}
			ctx := context.Background()
			var err error
			switch mode {
			case "reply":
				err = p.replyMessage(ctx, replyContext{messageID: "root"}, "text", `{"text":"test"}`)
			case "create":
				err = p.createMessage(ctx, "chat", "text", `{"text":"test"}`, "send")
			case "preview_reply":
				_, err = p.SendPreviewStart(ctx, replyContext{chatID: "chat", messageID: "root"}, "progress")
			case "preview_create":
				_, err = p.SendPreviewStart(ctx, replyContext{chatID: "chat"}, "progress")
			}
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 {
				t.Fatalf("requests=%d, want 2", requests.Load())
			}
			first, second := <-ids, <-ids
			if first == "" || first != second {
				t.Fatalf("retry changed idempotency key: %q, %q", first, second)
			}
		})
	}
}
