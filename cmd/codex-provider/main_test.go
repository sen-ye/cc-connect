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
	if !p.CodexReasoningReplayFallback || p.CodexEnvHTTPHeaders["api-key"] != "SEED_TEST_KEY" || p.Name != "Seed" {
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
