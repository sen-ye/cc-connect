package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Installed CLI, fake model endpoint, isolated history. Exercise prompt
// injection on multiple turns and after restarting/resuming the native process.
func TestNativeCodex_InstructionsFollowClientAcrossResume(t *testing.T) {
	bin := os.Getenv("CC_CODEX_NATIVE_TEST_BIN")
	if bin == "" {
		t.Skip("set CC_CODEX_NATIVE_TEST_BIN to exercise the installed CLI")
	}
	catalog, err := os.ReadFile("testdata/service_tier_model.json")
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(catalogPath, catalog, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		var body struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Instructions != "You are a test assistant." {
			t.Error("did not forward the prompt selected by the native model catalog")
		}
		if r.Header.Get("api-key") != "same-test-key" || r.Header.Get("x-session-id") != "same-affinity" {
			t.Error("instructions changed provider identity")
		}
		writeNativeReplayResponse(w, int(calls.Add(1)))
	}))
	defer upstream.Close()
	a := &Agent{cmd: bin, cliExtraArgs: []string{"-c", "model_catalog_json=" + tomlString(catalogPath)}, backend: "app_server", codexHome: t.TempDir(), workDir: t.TempDir(), model: "gpt-6-astra", mode: "suggest", activeIdx: -1,
		configEnv: []string{"OPENAI_API_KEY=", "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy="}}
	a.SetProviders([]core.ProviderConfig{{Name: "fixture", BaseURL: upstream.URL, CodexHTTPHeaders: map[string]string{"api-key": "same-test-key", "x-session-id": "same-affinity"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := a.StartSessionWithProvider(ctx, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !assertProviderReply(t, s, "native Seed replay OK", false) || !assertProviderReply(t, s, "native Seed replay OK", false) {
		return
	}
	id := s.CurrentSessionID()
	_ = s.Close()
	resumed, err := a.StartSessionWithProvider(ctx, id, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !assertProviderReply(t, resumed, "native Seed replay OK", false) {
		return
	}
	if resumed.CurrentSessionID() != id || calls.Load() != 3 {
		t.Fatal("restart lost the native thread or repeated requests")
	}
}
