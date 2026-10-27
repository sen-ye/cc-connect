package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func TestCodexReasoningReplayFallback_ProjectAndGlobalRoundTrip(t *testing.T) {
	t.Setenv("SEED_PROXY_TEST_KEY", "test-secret-not-to-serialize")
	want := config.ProviderConfig{
		Name: "Seed", Model: "gpt-6-astra", BaseURL: "https://router.example/responses/openai?api-version=v1",
		Codex: &config.CodexProviderConfig{
			WireAPI: "responses", EnvKey: "SEED_PROXY_TEST_KEY",
			HTTPHeaders: map[string]string{"X-Test": "metadata"}, EnvHTTPHeaders: map[string]string{"api-key": "SEED_PROXY_TEST_KEY"},
			ProxyURL: "http://proxy.example:8118", ModelCatalogURL: "https://router.example/models?api-version=v1",
			ReasoningReplayFallback: true,
			MaxOutputTokens:         64000,
		},
	}
	runtime := configProviderToCore(want)
	if !runtime.CodexReasoningReplayFallback || runtime.CodexMaxOutputTokens != 64000 {
		t.Fatal("agent wiring lost the recovery opt-in")
	}
	if got := coreProviderToConfig(runtime); !reflect.DeepEqual(got, want) {
		t.Fatalf("project provider-add round-trip = %+v; want %+v", got, want)
	}
	global := configProviderToGlobal(want)
	encoded, err := json.Marshal(global)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"reasoning_replay_fallback":true`) || !strings.Contains(string(encoded), `"max_output_tokens":64000`) || strings.Contains(string(encoded), "test-secret-not-to-serialize") {
		t.Fatal("management JSON lost the opt-in or resolved a credential")
	}
	var decoded core.GlobalProviderInfo
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := globalProviderToConfig(decoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("global management round-trip = %+v; want %+v", got, want)
	}
}

func TestCodexReasoningReplayFallback_OptInOnlyProviderPersists(t *testing.T) {
	got := coreProviderToConfig(core.ProviderConfig{Name: "Seed", CodexReasoningReplayFallback: true})
	if got.Codex == nil || !got.Codex.ReasoningReplayFallback {
		t.Fatal("provider-add dropped a standalone recovery opt-in")
	}
	legacy := coreProviderToConfig(core.ProviderConfig{Name: "aidp"})
	if legacy.Codex != nil || configProviderToCore(legacy).CodexReasoningReplayFallback {
		t.Fatal("provider without an opt-in changed behavior")
	}
	global := globalProviderToConfig(core.GlobalProviderInfo{Name: "legacy", Codex: &core.GlobalCodexConfig{WireAPI: "responses"}})
	if global.Codex.ReasoningReplayFallback {
		t.Fatal("legacy management JSON enabled recovery")
	}
}

func TestCodexMaxOutputTokens_StandaloneAndDefaultMappings(t *testing.T) {
	for _, limit := range []int{64000, -1} {
		// Preserve invalid values as well: runtime validation must see them,
		// rather than silently changing them to disabled during persistence.
		runtime := core.ProviderConfig{Name: "configured", CodexMaxOutputTokens: limit}
		stored := coreProviderToConfig(runtime)
		if stored.Codex == nil || stored.Codex.MaxOutputTokens != limit || configProviderToCore(stored).CodexMaxOutputTokens != limit {
			t.Fatalf("project mapping lost standalone max_output_tokens=%d", limit)
		}
		global := configProviderToGlobal(stored)
		if global.Codex == nil || global.Codex.MaxOutputTokens != limit || globalProviderToConfig(global).Codex.MaxOutputTokens != limit {
			t.Fatalf("global mapping lost standalone max_output_tokens=%d", limit)
		}
	}
	legacy := coreProviderToConfig(core.ProviderConfig{Name: "legacy"})
	if legacy.Codex != nil || configProviderToCore(legacy).CodexMaxOutputTokens != 0 {
		t.Fatal("legacy provider acquired an output limit")
	}
	encoded, err := json.Marshal(configProviderToGlobal(config.ProviderConfig{Name: "legacy", Codex: &config.CodexProviderConfig{WireAPI: "responses"}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "max_output_tokens") {
		t.Fatal("disabled output injection should remain omitted from management JSON")
	}
}
