package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Exercise the real Codex CLI with a local gateway that rejects encrypted
// reasoning from its previous response. No live credentials or model API.
func TestNativeCodex_SeedReplayFallbackSurvivesResume(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN for the installed CLI")
	}
	catalog, err := os.ReadFile("testdata/service_tier_model.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests, rejected, accepted := 0, 0, 0
	cacheKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		if r.URL.Path != "/openai/responses" || r.URL.Query().Get("api-version") != "2024-03-01-preview" {
			t.Errorf("unexpected route: %s", r.URL)
			http.Error(w, "unexpected route", 400)
			return
		}
		if r.Header.Get("api-key") != "seed-test-only" || r.Header.Get("x-session-id") != "native-affinity" {
			t.Error("native request lost its Seed authentication or affinity")
		}
		var body struct {
			Input          []json.RawMessage `json:"input"`
			PromptCacheKey string            `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		if cacheKey == "" {
			cacheKey = body.PromptCacheKey
		}
		if cacheKey == "" || body.PromptCacheKey != cacheKey {
			t.Error("retry/resume changed prompt cache affinity")
		}
		for _, raw := range body.Input {
			var item struct {
				Type      string `json:"type"`
				Encrypted string `json:"encrypted_content"`
			}
			if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" && item.Encrypted != "" {
				rejected++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_encrypted_content","message":"previous account cannot decrypt this reasoning"}}`))
				return
			}
		}
		if accepted > 0 {
			serialized, _ := json.Marshal(body.Input)
			if !strings.Contains(string(serialized), "native Seed replay OK") {
				t.Error("repair discarded readable assistant history")
			}
		}
		accepted++
		writeNativeReplayResponse(w, accepted)
	}))
	defer server.Close()
	a := &Agent{cmd: bin, backend: "app_server", codexHome: t.TempDir(), workDir: t.TempDir(), model: "gpt-6-astra", mode: "suggest", activeIdx: -1,
		configEnv: []string{"OPENAI_API_KEY=", "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy="}}
	a.SetProviders([]core.ProviderConfig{{Name: "Seed", BaseURL: server.URL + "/openai?api-version=2024-03-01-preview",
		CodexHTTPHeaders:     map[string]string{"api-key": "seed-test-only", "x-session-id": "native-affinity"},
		CodexModelCatalogURL: server.URL + "/models", CodexReasoningReplayFallback: true}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := func(id string) core.AgentSession {
		t.Helper()
		s, err := a.StartSessionWithProvider(ctx, id, "Seed")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	s := start("")
	if !assertProviderReply(t, s, "native Seed replay OK", false) || !assertProviderReply(t, s, "native Seed replay OK", false) {
		return
	}
	id := s.CurrentSessionID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = start(id)
	if !assertProviderReply(t, s, "native Seed replay OK", false) {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 5 || rejected != 2 || accepted != 3 || s.CurrentSessionID() != id {
		t.Fatalf("requests=%d rejected=%d accepted=%d native=%s", requests, rejected, accepted, s.CurrentSessionID())
	}
}

func writeNativeReplayResponse(w http.ResponseWriter, n int) {
	reasoning := map[string]any{"id": fmt.Sprintf("rs_%d", n), "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-from-previous-account"}
	part := map[string]any{"type": "output_text", "text": "native Seed replay OK", "annotations": []any{}}
	message := map[string]any{"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{part}}
	response := map[string]any{"id": fmt.Sprintf("resp_%d", n), "object": "response", "model": "gpt-6-astra", "status": "completed", "output": []any{reasoning, message}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, fields map[string]any) {
		fields["type"] = kind
		data, _ := json.Marshal(fields)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	event("response.created", map[string]any{"response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})
	event("response.output_item.added", map[string]any{"output_index": 0, "item": reasoning})
	event("response.output_item.done", map[string]any{"output_index": 0, "item": reasoning})
	event("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": message["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	event("response.output_text.delta", map[string]any{"item_id": message["id"], "output_index": 1, "content_index": 0, "delta": "native Seed replay OK"})
	event("response.output_item.done", map[string]any{"output_index": 1, "item": message})
	event("response.completed", map[string]any{"response": response})
}
