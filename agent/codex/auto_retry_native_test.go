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
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Real native CLI, local fake provider. Disable inner retries so the failed
// turn must reach the bridge's outer continuation instead of being hidden by
// the CLI. No real credentials, external model calls, or production history.
func TestNativeCodex_AutoRetryAfter429KeepsProviderAndHistory(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN to exercise the installed CLI")
	}
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint("stop=", stop), func(t *testing.T) {
			runNativeAutoRetry(t, bin, stop)
		})
	}
}

func runNativeAutoRetry(t *testing.T, bin string, stop bool) {
	catalog, err := os.ReadFile("testdata/service_tier_model.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	cacheKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		if r.Header.Get("api-key") != "same-seed-key" || r.Header.Get("x-session-id") != "same-affinity" {
			t.Error("automatic recovery changed the selected credentials")
		}
		var body struct {
			Input     json.RawMessage `json:"input"`
			CacheKey  string          `json:"prompt_cache_key"`
			MaxOutput int             `json:"max_output_tokens"`
			Model     string          `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			cacheKey = body.CacheKey
		}
		if cacheKey == "" || body.CacheKey != cacheKey || body.Model != "gpt-6-astra" || body.MaxOutput != 64000 {
			t.Error("recovery changed cache identity, model, or output budget")
		}
		if calls == 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"temporary rate limit"}}`))
			return
		}
		if calls == 3 {
			input := string(body.Input)
			if strings.Count(input, "ORIGINAL_RECOVERY_TASK_ONCE") != 1 || strings.Count(input, autoRetryContinuation) != 1 || !strings.Contains(input, "native Seed replay OK") {
				t.Error("native recovery duplicated the task or discarded saved progress")
			}
		}
		writeNativeReplayResponse(w, calls)
	}))
	defer server.Close()
	home, work := t.TempDir(), t.TempDir()
	wrapper := filepath.Join(t.TempDir(), "native-codex")
	script := "#!/bin/sh\nexec \"$CC_NATIVE_TEST_BINARY\" \"$@\" -c 'model_providers.Seed.request_max_retries=0' -c 'model_providers.Seed.stream_max_retries=0'\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	delay := 20 * time.Millisecond
	if stop {
		delay = 200 * time.Millisecond
	}
	a := &Agent{cmd: wrapper, backend: "app_server", codexHome: home, workDir: work, model: "gpt-6-astra", mode: "suggest", activeIdx: -1,
		autoRetry: autoRetryPolicy{maxAttempts: 2, maxWait: time.Second, baseDelay: delay, maxDelay: time.Second},
		configEnv: []string{"CC_NATIVE_TEST_BINARY=" + bin, "OPENAI_API_KEY=", "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy="}}
	a.SetProviders([]core.ProviderConfig{
		{Name: "Seed", BaseURL: server.URL + "/openai", CodexModelCatalogURL: server.URL + "/models", CodexMaxOutputTokens: 64000, CodexReasoningReplayFallback: true,
			CodexHTTPHeaders: map[string]string{"api-key": "same-seed-key", "x-session-id": "same-affinity"}},
		{Name: "Other", APIKey: "must-not-be-used", BaseURL: "http://127.0.0.1:1"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := a.StartSessionWithProvider(ctx, "", "Seed")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !assertProviderReply(t, s, "native Seed replay OK", false) {
		return
	}
	id := s.CurrentSessionID()
	if err := s.Send("ORIGINAL_RECOVERY_TASK_ONCE", "second-user-message", nil, nil); err != nil {
		t.Fatal(err)
	}
	retries := 0
	for {
		select {
		case event, ok := <-s.Events():
			if !ok {
				t.Fatal("native event stream closed before recovery completed")
			}
			switch event.Type {
			case core.EventRetry:
				retries++
				a.SetActiveProvider("Other") // Must never redirect this conversation.
				if stop {
					_ = s.Close()
					time.Sleep(250 * time.Millisecond)
					mu.Lock()
					defer mu.Unlock()
					if calls != 2 {
						t.Fatalf("stop allowed another model request: calls=%d", calls)
					}
					return
				}
			case core.EventError:
				t.Fatalf("transient error escaped automatic recovery: %v", event.Error)
			case core.EventResult:
				if retries != 1 || s.CurrentSessionID() != id {
					t.Fatalf("recovery lost identity or attempt count: retries=%d", retries)
				}
				mu.Lock()
				defer mu.Unlock()
				if calls != 3 {
					t.Fatalf("unexpected model replay count: %d", calls)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("native automatic recovery timed out")
		}
	}
}
