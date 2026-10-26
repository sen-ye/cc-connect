package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt-in wire verification with the installed CLI and a local Responses server.
// No real credentials, production conversations, or paid model calls are used.
func TestNativeCodex_ConversationServiceTierOverridesFastDefault(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN to test the installed CLI")
	}
	catalog, err := os.ReadFile("testdata/service_tier_model.json")
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") || r.Header.Get("Authorization") != "Bearer native-test-only" {
			t.Error("unexpected native route or credential")
			http.Error(w, "unexpected request", 400)
			return
		}
		var body struct {
			ServiceTier string `json:"service_tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		id := requests.Add(1)
		if id == 4 {
			close(held)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		t.Logf("request %d wire service_tier=%q", id, body.ServiceTier)
		tier := body.ServiceTier
		if tier == "priority" {
			tier = "fast"
		}
		if tier == "" || tier == "default" {
			tier = "standard"
		}
		text := "wire tier=" + tier
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		item := map[string]any{"id": fmt.Sprintf("msg_%d", id), "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{part}}
		response := map[string]any{"id": fmt.Sprintf("resp_%d", id), "object": "response", "model": "gpt-6-astra", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, fields map[string]any) {
			fields["type"] = kind
			data, _ := json.Marshal(fields)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		}
		event("response.created", map[string]any{"response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})
		event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		event("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "delta": text})
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		event("response.completed", map[string]any{"response": response})
	}))
	defer server.Close()
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	config := []byte("service_tier = \"fast\"\n")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cmd: bin, backend: "app_server", codexHome: home, workDir: t.TempDir(), model: "gpt-6-astra", mode: "suggest", activeIdx: -1,
		configEnv: []string{"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=127.0.0.1,localhost"}}
	a.SetProviders([]core.ProviderConfig{{Name: "test", APIKey: "native-test-only", BaseURL: server.URL + "/v1", CodexModelCatalogURL: server.URL + "/v1/models"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := func(id string) *appServerSession {
		t.Helper()
		s, err := a.StartSessionWithProvider(ctx, id, "test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s.(*appServerSession)
	}
	first := start("")
	if first.GetServiceTier() != "fast" {
		t.Fatalf("resolved global tier = %q", first.GetServiceTier())
	}
	if err := first.SetServiceTier("default"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("settings update made a model request")
	}
	second := start("")
	if !assertProviderReply(t, first, "wire tier=standard", false) || !assertProviderReply(t, second, "wire tier=fast", false) {
		return
	}
	id := first.CurrentSessionID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	first = start(id)
	// The engine restores the saved override before the first resumed prompt.
	if err := first.SetServiceTier("default"); err != nil {
		t.Fatal(err)
	}
	if !assertProviderReply(t, first, "wire tier=standard", false) {
		return
	}
	changed := make(chan error, 1)
	go func() {
		select {
		case <-held:
			changed <- first.SetServiceTier("fast")
		case <-ctx.Done():
			changed <- ctx.Err()
		}
		releaseOnce.Do(func() { close(release) })
	}()
	// Change speed while a real native turn is waiting for model output.
	// The already-started request stays standard; the next request uses Fast.
	if !assertProviderReply(t, first, "wire tier=standard", false) {
		return
	}
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if !assertProviderReply(t, first, "wire tier=fast", false) {
		return
	}
	if first.CurrentSessionID() != id || second.GetServiceTier() != "fast" || requests.Load() != 5 {
		t.Fatal("tier change replaced the native session, changed another conversation, or retried a model call")
	}
	if actual, err := os.ReadFile(configPath); err != nil || string(actual) != string(config) {
		t.Fatal("conversation setting rewrote the global Fast default")
	}
}
