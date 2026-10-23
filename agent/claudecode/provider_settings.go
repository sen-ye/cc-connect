package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func providerEnvEntries(env map[string]string) []string {
	entries := make([]string, 0, len(env))
	for key, value := range env {
		entries = append(entries, key+"="+value)
	}
	return entries
}

// Settings remain an external read-only input. Passing their path to Claude
// preserves apiKeyHelper refreshes without copying credentials into config.toml.
func resolveProviderSettings(name, workDir string) (string, string, error) {
	if strings.HasPrefix(name, "~/") || strings.HasPrefix(name, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("claudecode: resolve provider settings home: %w", err)
		}
		name = filepath.Join(home, name[2:])
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(workDir, name)
	}
	path, err := filepath.Abs(name)
	if err != nil {
		return "", "", fmt.Errorf("claudecode: resolve provider settings path: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("claudecode: read provider settings %q: %w", path, err)
	}
	var settings struct {
		Model string `json:"model"`
	}
	cleaned := stripJSONC(data)
	if strings.TrimSpace(string(cleaned)) == "null" {
		return "", "", fmt.Errorf("claudecode: provider settings %q must be a JSON object", path)
	}
	if err := json.Unmarshal(cleaned, &settings); err != nil {
		return "", "", fmt.Errorf("claudecode: parse provider settings %q: %w", path, err)
	}
	return path, settings.Model, nil
}
