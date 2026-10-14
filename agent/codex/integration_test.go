package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Optional live integration test. Authentication is scoped to the child
// process; even this test must leave existing Codex credential files intact.
func TestIntegration_CodexProviderFlow(t *testing.T) {
	apiKey := os.Getenv("SHENGSUANYUN_API_KEY")
	if apiKey == "" {
		t.Skip("SHENGSUANYUN_API_KEY not set")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex CLI not in PATH")
	}
	home, work := t.TempDir(), t.TempDir()
	provider := core.ProviderConfig{Name: "shengsuanyun-codex", APIKey: apiKey, BaseURL: "https://router.shengsuanyun.com/api/v1", Model: "openai/gpt-5.3-codex"}
	a := &Agent{backend: "exec", codexHome: home, workDir: work, mode: "suggest", activeIdx: -1}
	a.SetProviders([]core.ProviderConfig{provider})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := a.StartSessionWithProvider(ctx, "", provider.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("reply with exactly integration-ok and nothing else", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	var response strings.Builder
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case event, ok := <-s.Events():
			if !ok {
				t.Fatal("session closed before completion")
			}
			if event.Type == core.EventError {
				t.Fatalf("provider failed: %s", strings.ReplaceAll(event.Content, apiKey, "[REDACTED]"))
			}
			if event.Type == core.EventText || event.Type == core.EventResult {
				response.WriteString(event.Content)
			}
			if event.Type == core.EventResult {
				if !strings.Contains(response.String(), "integration-ok") {
					t.Fatal("missing expected response")
				}
				if _, err := os.Stat(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
					t.Fatal("unexpected auth.json write")
				}
				return
			}
		}
	}
}
