package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestNativeArgs_KeepProviderAndModelSelectionIsolated(t *testing.T) {
	p := config.ProviderConfig{Models: []config.ProviderModelConfig{{Model: "gpt-6-astra"}, {Model: "es1-grape-g61s"}}}
	for _, args := range [][]string{
		{"-m", "es1-grape-g61s", "exec", "hello"},
		{"--model=gpt-6-astra", "resume", "saved-id"},
		{"-mes1-grape-g61s", "exec", "-"},
		{"-c", `model_reasoning_effort="high"`},
		{"--", "--profile", "prompt text"},
	} {
		if err := validateNativeArgs(args, p); err != nil {
			t.Errorf("valid args %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"-m", "other"}, {"-mother"}, {"--model=other"},
		{"-p", "other"}, {"-pother"}, {"--profile=other"}, {"--oss"}, {"--remote=ws://other"},
		{"-c", `model_provider="other"`}, {"-cmodel_provider=other"},
		{"-c", `model_providers.Seed.base_url="http://other"`}, {"-c", `model="other"`}, {"-m"},
	} {
		if err := validateNativeArgs(args, p); err == nil {
			t.Errorf("accepted routing override %v", args)
		}
	}
}

func TestLoadLauncherConfig_MapsRecoveryAndDoesNotExposeSecrets(t *testing.T) {
	file := filepath.Join(t.TempDir(), "provider.toml")
	content := `binary = "/native/codex"
[provider]
name = "Seed"
base_url = "http://localhost/openai?api-version=test"
model = "gpt-6-astra"
models = [{model="gpt-6-astra"}, {model="es1-grape-g61s"}]
[provider.codex]
wire_api = "responses"
reasoning_replay_fallback = true
max_output_tokens = 64000
mirror_developer_instructions = true
rate_limit_backoff_max_seconds = 32
[provider.codex.env_http_headers]
api-key = "SEED_TEST_KEY"
`
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLauncherConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	p := nativeProvider(cfg.Provider)
	if !p.CodexReasoningReplayFallback || p.CodexMaxOutputTokens != 64000 || p.CodexRateLimitBackoffMaxSeconds != 32 || !p.MirrorsCodexDeveloperInstructions() || p.CodexEnvHTTPHeaders["api-key"] != "SEED_TEST_KEY" || p.Name != "Seed" {
		t.Fatal("provider recovery/auth config lost")
	}
	before, _ := os.ReadFile(file)
	if !reflect.DeepEqual(before, []byte(content)) {
		t.Fatal("launcher modified shared config")
	}
	if err := os.WriteFile(file, []byte("api_key = super-secret-without-quotes"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", file}, strings.NewReader(""), &output, &output); code != 2 || strings.Contains(output.String(), "super-secret") {
		t.Fatalf("invalid config leaked a credential or returned success: code=%d", code)
	}
}

func TestNativeProvider_MaxOutputTokensDefaultAndIsolation(t *testing.T) {
	configured := config.ProviderConfig{Name: "configured", Codex: &config.CodexProviderConfig{MaxOutputTokens: 64000}}
	if got := nativeProvider(configured).CodexMaxOutputTokens; got != 64000 {
		t.Fatalf("native output limit = %d, want 64000", got)
	}
	for _, provider := range []config.ProviderConfig{{Name: "legacy"}, {Name: "explicit-zero", Codex: &config.CodexProviderConfig{}}} {
		if got := nativeProvider(provider).CodexMaxOutputTokens; got != 0 {
			t.Fatalf("unconfigured provider %s acquired output limit %d", provider.Name, got)
		}
	}
	configured.Codex.MaxOutputTokens = -1
	if got := nativeProvider(configured).CodexMaxOutputTokens; got != -1 {
		t.Fatal("invalid value was hidden from runtime validation")
	}
}

func TestRestoringSession_PreservesSavedModel(t *testing.T) {
	for _, args := range [][]string{{"resume", "id"}, {"exec", "resume", "--last"}, {"fork", "id"}} {
		if !restoringSession(args) {
			t.Errorf("resume treated as new session: %v", args)
		}
	}
	if restoringSession([]string{"--", "resume"}) || restoringSession([]string{"exec", "hello"}) {
		t.Fatal("prompt treated as restore command")
	}
}

func TestServeModeDoesNotStartNativeCLIOrAcceptLauncherCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.toml")
	source := `binary = "/this-native-binary-must-not-run"
[provider]
name = "test"
base_url = "http://127.0.0.1:1"
model = "gpt-6-astra"
[provider.codex]
mirror_developer_instructions = true
rate_limit_backoff_max_seconds = 32
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, stderr bytes.Buffer
	if code := run(ctx, []string{"--config", path, "--listen", "127.0.0.1:0"}, strings.NewReader(""), &out, &stderr); code != 0 || !strings.Contains(out.String(), `"listening":"http://127.0.0.1:`) {
		t.Fatalf("serve failed: code=%d stderr=%s", code, stderr.String())
	}
	if code := run(ctx, []string{"--config", path, "--listen", "127.0.0.1:0", "--", "exec", "hello"}, strings.NewReader(""), &out, &stderr); code != 2 {
		t.Fatal("serve accepted native arguments")
	}
	if err := os.WriteFile(path, []byte(source+`env_key = "PRIVATE_KEY_SOURCE"`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := run(ctx, []string{"--config", path, "--listen", "127.0.0.1:0"}, strings.NewReader(""), &out, &stderr); code == 0 || out.Len() != 0 || strings.Contains(stderr.String(), "PRIVATE_KEY_SOURCE") {
		t.Fatal("serve accepted launcher credentials or disclosed their source")
	}
}
