package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestResumeProvider_RefusesToMigrateAnotherProvidersHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	seedID, aidpID := "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002"
	for id, provider := range map[string]string{seedID: "Seed", aidpID: "aidp"} {
		dir := filepath.Join(home, "sessions", "2026", "10", "27")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"model_provider":%q}}`, id, provider)
		if err := os.WriteFile(filepath.Join(dir, "rollout-test-"+id+".jsonl"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	provider := config.ProviderConfig{Name: "Seed"}
	for _, args := range [][]string{{"resume", seedID}, {"exec", "resume", "-m", "es1-grape-g61s", seedID}, {"fork", seedID}} {
		if err := validateResumeProvider(args, provider); err != nil {
			t.Errorf("own session refused: %v", err)
		}
	}
	for _, args := range [][]string{{"resume", aidpID}, {"resume", "--last"}, {"resume"}, {"resume", "00000000-0000-7000-8000-000000000099"}} {
		if err := validateResumeProvider(args, provider); err == nil {
			t.Errorf("unverified history accepted: %v", args)
		}
	}
	if restoringSession([]string{"-C", "resume", "prompt"}) || restoringSession([]string{"exec", "prompt mentioning", "resume"}) {
		t.Error("option value or prompt misidentified as a resume command")
	}
}
