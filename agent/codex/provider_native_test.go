package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// Opt-in: run the installed native Codex against a local Responses fixture.
// It checks actual Authorization headers, native multi-turn/resume behavior,
// and shared-home isolation without using real credentials or a model API.
func TestNativeCodex_SessionProviderIsolation(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN to run the local native CLI test")
	}
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(logger) })
	for _, backend := range []string{"exec", "app_server"} {
		for _, proxied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_fixed_proxy_%v", backend, proxied), func(t *testing.T) { runNativeCodexProviderTest(t, bin, backend, proxied) })
		}
	}
}

func runNativeCodexProviderTest(t *testing.T, bin, backend string, proxied bool) {
	var sequence atomic.Int64
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isCatalog := strings.HasSuffix(r.URL.Path, "/models")
		if !strings.HasSuffix(r.URL.Path, "/responses") && !isCatalog {
			http.NotFound(w, r)
			return
		}
		name := ""
		switch r.Header.Get("Authorization") {
		case "Bearer native-test-a":
			name = "api.a"
		case "Bearer native-test-b":
			name = "OpenAI"
		default:
			t.Error("native request used an unexpected credential")
			http.Error(w, "wrong credential", http.StatusUnauthorized)
			return
		}
		if isCatalog {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
			return
		}
		if r.Header.Get("X-Session-Id") != "affinity-"+name {
			t.Error("native request lost its session affinity")
		}
		mu.Lock()
		counts[name]++
		mu.Unlock()
		id := sequence.Add(1)
		text := "native account=" + name
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		item := map[string]any{"id": fmt.Sprintf("msg_%d", id), "type": "message", "status": "completed", "role": "assistant", "phase": "final_answer", "content": []any{part}}
		response := map[string]any{"id": fmt.Sprintf("resp_%d", id), "object": "response", "created_at": time.Now().Unix(), "model": "gpt-6-astra", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "input_tokens_details": map[string]int{"cached_tokens": 0}, "output_tokens_details": map[string]int{"reasoning_tokens": 0}}}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, fields map[string]any) {
			fields["type"] = kind
			data, _ := json.Marshal(fields)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		}
		event("response.created", map[string]any{"response": map[string]any{"id": response["id"], "model": "gpt-6-astra", "status": "in_progress", "output": []any{}}})
		event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		event("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		event("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "delta": text})
		event("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "text": text})
		event("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": part})
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		event("response.completed", map[string]any{"response": response})
	}))
	defer server.Close()
	home, work := t.TempDir(), t.TempDir()
	original := map[string]string{"auth.json": "{\"OPENAI_API_KEY\":\"legacy-test-only\"}", "config.toml": "model_provider = \"legacy\"\n[model_providers.legacy]\nname = \"legacy\"\nbase_url = \"http://127.0.0.1:1\"\n"}
	for name, data := range original {
		if err := os.WriteFile(filepath.Join(home, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := &Agent{cmd: bin, backend: backend, codexHome: home, workDir: work, model: "gpt-6-astra", mode: "suggest", activeIdx: -1,
		configEnv: []string{"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "all_proxy=", "ALL_PROXY=", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"}}
	providers := []core.ProviderConfig{{Name: "api.a", APIKey: "native-test-a"}, {Name: "OpenAI", APIKey: "native-test-b"}}
	for i := range providers {
		providers[i].BaseURL = server.URL + "/v1"
		providers[i].CodexModelCatalogURL = server.URL + "/v1/models"
		providers[i].Env = map[string]string{"TEST_AFFINITY": "affinity-" + providers[i].Name}
		providers[i].CodexEnvHTTPHeaders = map[string]string{"x-session-id": "TEST_AFFINITY"}
		if proxied {
			key := providers[i].APIKey
			route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !r.URL.IsAbs() || (r.URL.Host != "model.invalid" && r.URL.Host != "catalog.invalid") {
					t.Error("request did not use the configured network proxy")
				}
				if r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("API key used the wrong fixed proxy")
				}
				server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(route.Close)
			providers[i].BaseURL = "http://model.invalid/v1"
			providers[i].CodexModelCatalogURL = "http://catalog.invalid/v1/models"
			providers[i].CodexProxyURL = route.URL
		}
	}
	a.SetProviders(providers)
	// Existing deployments already have a migrated native state DB. Bootstrap
	// the fresh fixture home once before exercising concurrent processes.
	opts := a.WorkspaceAgentOptionSnapshot()
	opts["work_dir"] = work
	opts["backend"] = "app_server"
	bootstrap, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.(*Agent).SetProviders(providers)
	warm, err := bootstrap.(*Agent).StartSessionWithProvider(context.Background(), "", providers[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s, err := a.StartSessionWithProvider(ctx, "", name)
			if err != nil {
				t.Error(err)
				return
			}
			if !assertProviderReply(t, s, "native account="+name, false) {
				s.Close()
				return
			}
			if backend == "exec" {
				// Exercise persisted resume with a new CLI process. Completion
				// can precede the native CLI's background shutdown work.
				id := s.CurrentSessionID()
				s.Close()
				s, err = a.StartSessionWithProvider(ctx, id, name)
				if err != nil {
					t.Error(err)
					return
				}
			}
			if !assertProviderReply(t, s, "native account="+name, false) {
				s.Close()
				return
			}
			id := s.CurrentSessionID()
			s.Close()
			s, err = a.StartSessionWithProvider(ctx, id, name)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			assertProviderReply(t, s, "native account="+name, false)
		}(p.Name)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, p := range providers {
		if counts[p.Name] != 3 {
			t.Errorf("%s received %d requests, want 3", p.Name, counts[p.Name])
		}
	}
	for name, want := range original {
		got, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("native flow modified shared %s", name)
		}
	}
}
