package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Regression: starting a provider must not overwrite credentials/config used
// by another live Codex process (including legacy CLI-authenticated sessions).
func TestStartSession_PreservesSharedCredentials(t *testing.T) {
	home := t.TempDir()
	original := map[string]string{
		"config.toml": "model_provider = \"legacy\"\n[model_providers.legacy]\nname = \"legacy\"\nbase_url = \"http://localhost/legacy\"\n",
		"auth.json":   "{\"OPENAI_API_KEY\":\"original-test-key\"}\n",
	}
	for name, contents := range original {
		if err := os.WriteFile(filepath.Join(home, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := &Agent{codexHome: home, backend: "exec", activeIdx: -1}
	a.SetProviders([]core.ProviderConfig{{Name: "alternate", APIKey: "alternate-test-key", BaseURL: "http://localhost/alternate"}})
	a.SetActiveProvider("alternate")
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for name, want := range original {
		got, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("starting a provider changed shared %s", name)
		}
	}
}
