package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestCodexReasoningReplayFallback_ConfigDefaultsAndRoundTrip(t *testing.T) {
	t.Setenv("SEED_PROXY_TEST_KEY", "test-secret-not-to-serialize")
	const source = `[[providers]]
name = "Seed"
base_url = "https://router.example/responses/openai?api-version=2024-03-01-preview"
[providers.codex]
wire_api = "responses"
reasoning_replay_fallback = true
[providers.codex.env_http_headers]
api-key = "SEED_PROXY_TEST_KEY"

[[projects]]
name = "chat"
[projects.agent]
type = "codex"
provider_refs = ["Seed"]
`
	var cfg Config
	if _, err := toml.Decode(source, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ResolveProviderRefs()
	provider := cfg.Projects[0].Agent.Providers[0]
	if provider.Codex == nil || !provider.Codex.ReasoningReplayFallback || provider.APIKey != "" {
		t.Fatal("provider reference lost its opt-in or materialized a credential")
	}
	var encoded strings.Builder
	if err := toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "test-secret-not-to-serialize") {
		t.Fatal("encoding copied an environment credential into configuration")
	}
	var reloaded Config
	if _, err := toml.Decode(encoded.String(), &reloaded); err != nil {
		t.Fatal(err)
	}
	got := reloaded.Projects[0].Agent.Providers[0]
	if !reflect.DeepEqual(got.Codex, provider.Codex) || got.BaseURL != provider.BaseURL {
		t.Fatal("TOML round-trip lost provider routing or recovery settings")
	}

	var legacy CodexProviderConfig
	if _, err := toml.Decode(`wire_api = "responses"`, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ReasoningReplayFallback {
		t.Fatal("legacy provider unexpectedly enabled lossy reasoning recovery")
	}
	encoded.Reset()
	if err := toml.NewEncoder(&encoded).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "reasoning_replay_fallback") {
		t.Fatal("disabled default should remain omitted")
	}
}

func TestCloneAgentConfig_PreservesCodexRoutingAndRecovery(t *testing.T) {
	settings := &CodexProviderConfig{
		WireAPI: "responses", EnvKey: "PROVIDER_ENV_KEY",
		ProxyURL: "http://proxy.example:8118", ModelCatalogURL: "https://router.example/models?api-version=v1",
		HTTPHeaders: map[string]string{"X-Test": "original"}, EnvHTTPHeaders: map[string]string{"api-key": "PROVIDER_ENV_KEY"},
		ReasoningReplayFallback: true,
	}
	source := AgentConfig{Type: "codex", Providers: []ProviderConfig{{Name: "Seed", Codex: settings}}}
	cloned := cloneAgentConfig(source)
	got := cloned.Providers[0].Codex
	if !reflect.DeepEqual(got, settings) {
		t.Fatalf("cloned Codex settings = %+v; want %+v", got, settings)
	}
	got.HTTPHeaders["X-Test"] = "changed"
	got.EnvHTTPHeaders["api-key"] = "OTHER_ENV_KEY"
	got.ReasoningReplayFallback = false
	if settings.HTTPHeaders["X-Test"] != "original" || settings.EnvHTTPHeaders["api-key"] != "PROVIDER_ENV_KEY" || !settings.ReasoningReplayFallback {
		t.Fatal("cloned project mutated the source provider configuration")
	}
}
