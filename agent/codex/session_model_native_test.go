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

type modelWirePlatform struct {
	mu      sync.Mutex
	replies []string
}

func (*modelWirePlatform) Name() string                    { return "test" }
func (*modelWirePlatform) Start(core.MessageHandler) error { return nil }
func (*modelWirePlatform) Stop() error                     { return nil }
func (p *modelWirePlatform) Reply(_ context.Context, _ any, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replies = append(p.replies, text)
	return nil
}
func (p *modelWirePlatform) Send(ctx context.Context, rc any, text string) error {
	return p.Reply(ctx, rc, text)
}
func (p *modelWirePlatform) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.replies, "\n")
}
func (p *modelWirePlatform) clear() { p.mu.Lock(); defer p.mu.Unlock(); p.replies = nil }

// Exercises public bridge commands against the installed Codex CLI and a local
// Responses endpoint. No production credentials or paid requests are used.
func TestNativeCodex_BridgeModelOverrideAndFastDefault(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN to verify the installed CLI")
	}
	catalog, err := os.ReadFile("testdata/service_tier_model.json")
	if err != nil {
		t.Fatal(err)
	}
	var models map[string][]map[string]any
	if err := json.Unmarshal(catalog, &models); err != nil {
		t.Fatal(err)
	}
	var sol map[string]any
	raw, _ := json.Marshal(models["models"][0])
	_ = json.Unmarshal(raw, &sol)
	sol["slug"], sol["display_name"] = "gpt-6.1-sol", "GPT-6.1 Sol"
	models["models"] = append(models["models"], sol)
	catalog, _ = json.Marshal(models)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") || r.Header.Get("Authorization") != "Bearer local-model-test" {
			t.Error("unexpected route or credential")
			http.Error(w, "unexpected request", 400)
			return
		}
		var body struct {
			Model     string `json:"model"`
			Tier      string `json:"service_tier"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		tier := "standard"
		if body.Tier == "fast" || body.Tier == "priority" {
			tier = "fast"
		}
		n := requests.Add(1)
		text := fmt.Sprintf("wire model=%s tier=%s effort=%s", body.Model, tier, body.Reasoning.Effort)
		t.Log(text)
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		item := map[string]any{"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{part}}
		response := map[string]any{"id": fmt.Sprintf("resp_%d", n), "object": "response", "model": body.Model, "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
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
	home, work := t.TempDir(), t.TempDir()
	config := []byte("service_tier = \"fast\"\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sessions.json")
	makeEngine := func() (*core.Engine, *modelWirePlatform) {
		a := &Agent{cmd: bin, backend: "app_server", codexHome: home, workDir: work, reasoningEffort: "ultra", mode: "suggest", activeIdx: -1,
			configEnv: []string{"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=127.0.0.1,localhost"}}
		mirror := false
		a.SetProviders([]core.ProviderConfig{{Name: "OpenAI", APIKey: "local-model-test", BaseURL: server.URL + "/v1", Model: "gpt-6-astra", CodexModelCatalogURL: server.URL + "/v1/models", CodexMirrorDeveloperInstructions: &mirror,
			Models: []core.ModelOption{{Name: "gpt-6.1-sol"}, {Name: "gpt-6-astra"}}}})
		p := &modelWirePlatform{}
		e := core.NewEngine("test", a, []core.Platform{p}, path, core.LangEnglish)
		t.Cleanup(func() { _ = e.Stop() })
		return e, p
	}
	e, p := makeEngine()
	send := func(key, content string) {
		e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "test", UserID: key, ReplyCtx: key, Content: content})
	}
	chat := func(key, want string) {
		t.Helper()
		p.clear()
		send(key, "Reply with the requested status.")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(p.text(), want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected %s; bridge replies: %s", want, p.text())
	}
	send("test:a", "/provider switch OpenAI")
	send("test:a", "/model switch gpt-6.1-sol")
	chat("test:a", "wire model=gpt-6.1-sol tier=standard")
	chat("test:a", "wire model=gpt-6.1-sol tier=standard")
	send("test:a", "/fast on")
	chat("test:a", "wire model=gpt-6.1-sol tier=fast")
	originalID := core.NewSessionManager(path).GetOrCreateActive("test:a").GetAgentSessionID()
	_ = e.Stop()
	e, p = makeEngine()
	chat("test:a", "wire model=gpt-6.1-sol tier=fast")
	if got := core.NewSessionManager(path).GetOrCreateActive("test:a").GetAgentSessionID(); got != originalID || got == "" {
		t.Fatal("restart lost the native conversation identity")
	}
	send("test:b", "/provider switch OpenAI")
	chat("test:b", "wire model=gpt-6-astra tier=standard")
	_ = e.Stop()
	if requests.Load() != 5 {
		t.Fatalf("unexpected model request count: %d", requests.Load())
	}
	if got, err := os.ReadFile(filepath.Join(home, "config.toml")); err != nil || string(got) != string(config) {
		t.Fatal("bridge changed CLI defaults")
	}
}
