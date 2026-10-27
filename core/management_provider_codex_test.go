package core

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectProviderAdd_PreservesCodexRecoveryAndEnvironmentReferences(t *testing.T) {
	t.Setenv("SEED_PROXY_TEST_KEY", "test-secret-not-to-serialize")
	const body = `{"name":"Seed","model":"gpt-6-astra","base_url":"https://router.example/responses/openai?api-version=v1","codex":{"wire_api":"responses","env_key":"SEED_PROXY_TEST_KEY","env_http_headers":{"api-key":"SEED_PROXY_TEST_KEY"},"http_headers":{"X-Test":"metadata"},"proxy_url":"http://proxy.example:8118","model_catalog_url":"https://router.example/models?api-version=v1","reasoning_replay_fallback":true,"max_output_tokens":64000}}`
	for _, failSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "save_failed"}[failSave], func(t *testing.T) {
			agent := &stubPersistProvider{providers: []ProviderConfig{{Name: "aidp"}}, active: "aidp"}
			e := NewEngine("test", agent, nil, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			var saved ProviderConfig
			e.providerAddSaveFunc = func(provider ProviderConfig) error {
				saved = provider
				if failSave {
					return errors.New("configuration write failed")
				}
				return nil
			}
			w := httptest.NewRecorder()
			(&ManagementServer{}).handleProjectProviders(w, httptest.NewRequest(http.MethodPost, "/providers", strings.NewReader(body)), e, "")
			if !saved.CodexReasoningReplayFallback || saved.CodexWireAPI != "responses" || saved.CodexEnvKey != "SEED_PROXY_TEST_KEY" ||
				saved.CodexEnvHTTPHeaders["api-key"] != "SEED_PROXY_TEST_KEY" || saved.CodexHTTPHeaders["X-Test"] != "metadata" ||
				saved.CodexProxyURL != "http://proxy.example:8118" || saved.CodexModelCatalogURL != "https://router.example/models?api-version=v1" || saved.CodexMaxOutputTokens != 64000 {
				t.Fatal("provider persistence callback lost Codex settings")
			}
			if saved.APIKey != "" || strings.Contains(w.Body.String(), "test-secret-not-to-serialize") {
				t.Fatal("management path materialized an environment credential")
			}
			if agent.activeName() != "aidp" {
				t.Fatal("adding Seed changed the existing default provider")
			}
			if failSave {
				if w.Code != http.StatusInternalServerError || agent.count() != 1 {
					t.Fatal("failed persistence installed the provider")
				}
			} else if w.Code != http.StatusOK || agent.count() != 2 || !agent.ListProviders()[1].CodexReasoningReplayFallback || agent.ListProviders()[1].CodexMaxOutputTokens != 64000 {
				t.Fatal("saved Codex settings were not installed")
			}
		})
	}
}

func TestProjectProviderAdd_RecoveryDefaultsOff(t *testing.T) {
	for _, payload := range []string{`{"name":"plain"}`, `{"name":"plain","codex":{"wire_api":"responses"}}`, `{"name":"plain","codex":{"reasoning_replay_fallback":false,"max_output_tokens":0}}`} {
		agent := &stubPersistProvider{}
		e := NewEngine("test", agent, nil, "", LangEnglish)
		t.Cleanup(func() { _ = e.Stop() })
		w := httptest.NewRecorder()
		(&ManagementServer{}).handleProjectProviders(w, httptest.NewRequest(http.MethodPost, "/providers", strings.NewReader(payload)), e, "")
		if w.Code != http.StatusOK || agent.count() != 1 || agent.ListProviders()[0].CodexReasoningReplayFallback || agent.ListProviders()[0].CodexMaxOutputTokens != 0 {
			t.Fatalf("unexpected recovery opt-in for legacy payload: HTTP %d", w.Code)
		}
	}
}

func TestGlobalProviderCodexMaxOutputTokens_ManagementRoundTrip(t *testing.T) {
	var saved GlobalProviderInfo
	m := &ManagementServer{}
	m.SetAddGlobalProvider(func(provider GlobalProviderInfo) error {
		saved = provider
		return nil
	})
	m.SetUpdateGlobalProvider(func(name string, provider GlobalProviderInfo) error {
		if name != "configured" {
			t.Fatalf("updated wrong provider: %s", name)
		}
		saved = provider
		return nil
	})
	m.SetListGlobalProviders(func() ([]GlobalProviderInfo, error) { return []GlobalProviderInfo{saved}, nil })
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/v1/providers", `{"name":"configured","codex":{"max_output_tokens":64000}}`, 64000},
		{http.MethodPatch, "/api/v1/providers/configured", `{"name":"configured","codex":{"max_output_tokens":32000}}`, 32000},
		{http.MethodPut, "/api/v1/providers/configured", `{"name":"configured","codex":{"max_output_tokens":0}}`, 0},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.method == http.MethodPost {
			m.handleGlobalProviders(w, r)
		} else {
			m.handleGlobalProviderRoutes(w, r)
		}
		if w.Code != http.StatusOK || saved.Codex == nil || saved.Codex.MaxOutputTokens != tc.want {
			t.Fatalf("%s lost output limit: HTTP %d, codex=%+v", tc.method, w.Code, saved.Codex)
		}
		w = httptest.NewRecorder()
		m.handleGlobalProviders(w, httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil))
		var listed struct {
			Data struct {
				Providers []GlobalProviderInfo `json:"providers"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Data.Providers) != 1 || listed.Data.Providers[0].Codex == nil || listed.Data.Providers[0].Codex.MaxOutputTokens != tc.want {
			t.Fatalf("GET lost output limit after %s: %s", tc.method, w.Body.String())
		}
		if tc.want == 0 && strings.Contains(w.Body.String(), "max_output_tokens") {
			t.Fatal("disabled output injection was not omitted")
		}
	}
}
