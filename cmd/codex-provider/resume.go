package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/config"
	"github.com/google/uuid"
)

// A native UUID can otherwise override the provider implied by a launcher.
// Require an explicit ID and verify ownership before starting a model process.
// The unrestricted native resume picker remains available in ordinary Codex.
func validateResumeProvider(args []string, provider config.ProviderConfig) error {
	index := restoreCommandIndex(args)
	if index < 0 {
		return nil
	}
	var id string
	for i := index + 1; i < len(args); i++ {
		if nativeOptionTakesValue(args[i]) {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		if parsed, err := uuid.Parse(args[i]); err == nil && len(args[i]) == 36 {
			id = parsed.String()
		}
		break
	}
	if id == "" {
		return fmt.Errorf("codex-provider: resume/fork requires an explicit session UUID belonging to this provider")
	}
	home := os.Getenv("CODEX_HOME")
	if value := provider.Env["CODEX_HOME"]; value != "" {
		home = value
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("codex-provider: cannot locate Codex history")
		}
		home = filepath.Join(userHome, ".codex")
	}
	for _, folder := range []string{"sessions", "archived_sessions"} {
		var filename string
		err := filepath.WalkDir(filepath.Join(home, folder), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), "-"+id+".jsonl") {
				filename = path
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("codex-provider: cannot verify saved session provider")
		}
		if filename != "" {
			return verifySessionHeader(filename, id, provider.Name)
		}
	}
	return fmt.Errorf("codex-provider: saved session was not found; provider ownership cannot be verified")
}

func verifySessionHeader(filename, id, provider string) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("codex-provider: cannot read saved session metadata")
	}
	defer f.Close()
	var header struct {
		Type    string `json:"type"`
		Payload struct {
			ID       string `json:"id"`
			Provider string `json:"model_provider"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&header); err != nil ||
		header.Type != "session_meta" || header.Payload.ID != id || header.Payload.Provider != provider {
		return fmt.Errorf("codex-provider: saved session does not belong to this provider; use its original launcher")
	}
	return nil
}
