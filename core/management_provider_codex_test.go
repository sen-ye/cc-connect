package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectProviderAdd_PreservesCodexRecoveryAndEnvironmentReferences(t *testing.T) {
	t.Setenv("SEED_PROXY_TEST_KEY", "test-secret-not-to-serialize")
	const body = `{"name":"Seed","model":"gpt-6-astra","base_url":"https://router.example/responses/openai?api-version=v1","codex":{"wire_api":"responses","env_key":"SEED_PROXY_TEST_KEY","env_http_headers":{"api-key":"SEED_PROXY_TEST_KEY"},"http_headers":{"X-Test":"metadata"},"proxy_url":"http://proxy.example:8118","model_catalog_url":"https://router.example/models?api-version=v1","reasoning_replay_fallback":true}}`
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
				saved.CodexProxyURL != "http://proxy.example:8118" || saved.CodexModelCatalogURL != "https://router.example/models?api-version=v1" {
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
			} else if w.Code != http.StatusOK || agent.count() != 2 || !agent.ListProviders()[1].CodexReasoningReplayFallback {
				t.Fatal("saved Codex settings were not installed")
			}
		})
	}
}

func TestProjectProviderAdd_RecoveryDefaultsOff(t *testing.T) {
	for _, payload := range []string{`{"name":"plain"}`, `{"name":"plain","codex":{"wire_api":"responses"}}`, `{"name":"plain","codex":{"reasoning_replay_fallback":false}}`} {
		agent := &stubPersistProvider{}
		e := NewEngine("test", agent, nil, "", LangEnglish)
		t.Cleanup(func() { _ = e.Stop() })
		w := httptest.NewRecorder()
		(&ManagementServer{}).handleProjectProviders(w, httptest.NewRequest(http.MethodPost, "/providers", strings.NewReader(payload)), e, "")
		if w.Code != http.StatusOK || agent.count() != 1 || agent.ListProviders()[0].CodexReasoningReplayFallback {
			t.Fatalf("unexpected recovery opt-in for legacy payload: HTTP %d", w.Code)
		}
	}
}
