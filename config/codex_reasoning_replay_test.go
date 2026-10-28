package config

import (
	"encoding/json"
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
max_output_tokens = 64000
mirror_developer_instructions = true
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
	if provider.Codex == nil || !provider.Codex.ReasoningReplayFallback || provider.Codex.MaxOutputTokens != 64000 || (provider.Codex.MirrorDeveloperInstructions == nil || !*provider.Codex.MirrorDeveloperInstructions) || provider.APIKey != "" {
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
	if legacy.ReasoningReplayFallback || legacy.MaxOutputTokens != 0 {
		t.Fatal("legacy provider unexpectedly enabled lossy reasoning recovery")
	}
	encoded.Reset()
	if err := toml.NewEncoder(&encoded).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "reasoning_replay_fallback") || strings.Contains(encoded.String(), "max_output_tokens") {
		t.Fatal("disabled default should remain omitted")
	}
}

func TestCodexMaxOutputTokens_ConfigJSONRoundTrip(t *testing.T) {
	for _, limit := range []int{0, 64000, -1} {
		settings := CodexProviderConfig{MaxOutputTokens: limit}
		encoded, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "max_output_tokens") != (limit != 0) {
			t.Fatalf("JSON output injection field for %d: %s", limit, encoded)
		}
		var decoded CodexProviderConfig
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.MaxOutputTokens != limit {
			t.Fatalf("JSON lost max_output_tokens=%d", limit)
		}
	}
}

func TestCloneAgentConfig_PreservesCodexRoutingAndRecovery(t *testing.T) {
	settings := &CodexProviderConfig{
		WireAPI: "responses", EnvKey: "PROVIDER_ENV_KEY",
		ProxyURL: "http://proxy.example:8118", ModelCatalogURL: "https://router.example/models?api-version=v1",
		HTTPHeaders: map[string]string{"X-Test": "original"}, EnvHTTPHeaders: map[string]string{"api-key": "PROVIDER_ENV_KEY"},
		ReasoningReplayFallback:     true,
		MaxOutputTokens:             64000,
		MirrorDeveloperInstructions: mirrorInstructionsValue(true),
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
	got.MaxOutputTokens = 32000
	*got.MirrorDeveloperInstructions = false
	if settings.HTTPHeaders["X-Test"] != "original" || settings.EnvHTTPHeaders["api-key"] != "PROVIDER_ENV_KEY" || !settings.ReasoningReplayFallback || settings.MaxOutputTokens != 64000 || !*settings.MirrorDeveloperInstructions {
		t.Fatal("cloned project mutated the source provider configuration")
	}
}

func mirrorInstructionsValue(value bool) *bool { return &value }

func TestCodexInstructions_OfficialOptOutSurvivesTOML(t *testing.T) {
	for _, setting := range []*bool{nil, mirrorInstructionsValue(false), mirrorInstructionsValue(true)} {
		var encoded strings.Builder
		if err := toml.NewEncoder(&encoded).Encode(CodexProviderConfig{MirrorDeveloperInstructions: setting}); err != nil {
			t.Fatal(err)
		}
		var decoded CodexProviderConfig
		if _, err := toml.Decode(encoded.String(), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.MirrorDeveloperInstructions, setting) {
			t.Fatal("TOML lost the distinction between a default and an explicit instructions exception")
		}
	}
}
