package cursor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/internal/acpclient"
)

func startCursorACP(ctx context.Context, cmd string, args []string, workDir, model, mode, sessionID string, env []string) (core.AgentSession, error) {
	args = append(append([]string(nil), args...), "acp")
	if mode == "force" {
		args = append(args, "--force")
		mode = "agent"
	}
	if mode == "default" || mode == "" {
		mode = "agent"
	}
	return acpclient.Start(ctx, acpclient.RuntimeOptions{
		Command: cmd, Args: args, Env: env, WorkDir: workDir, SessionID: sessionID,
		AuthMethod: "cursor_login", Mode: mode, Model: cursorACPModel(model),
		Interruptible: true, StrictResume: true, ParameterizedModels: true,
		ErrorMapper: func(err error) error {
			failure := cursorFailure(map[string]any{"message": err.Error()})
			failure.Detail = safeCursorDetail(failure.Detail, env)
			failure.Model = model
			return failure
		},
	})
}

// Older Cursor model aliases remain valid in existing cc-connect configs.
// ACP exposes the same choices as a base model plus config-option values.
func cursorACPModel(model string) string {
	if strings.HasPrefix(model, "gemini-4-argon-") {
		effort := strings.TrimPrefix(model, "gemini-4-argon-")
		if effort == "low" || effort == "medium" || effort == "high" {
			return "gemini-4-argon[reasoning_effort=" + effort + "]"
		}
	}
	if strings.HasPrefix(model, "grok-4.7-") {
		effort := strings.TrimPrefix(model, "grok-4.7-")
		fast := "false"
		if strings.HasSuffix(effort, "-fast") {
			effort = strings.TrimSuffix(effort, "-fast")
			fast = "true"
		}
		if effort == "low" || effort == "medium" || effort == "high" || effort == "xhigh" {
			return "grok-4.7[reasoning_effort=" + effort + ",fast=" + fast + "]"
		}
	}
	return model
}

func validCursorSessionID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00") && filepath.Base(id) == id
}

func cursorACPDir(configDir, id string) string {
	return filepath.Join(configDir, "acp-sessions", id)
}

func cursorConfigDir(home string, extraEnv []string) string {
	values := map[string]string{"CURSOR_CONFIG_DIR": os.Getenv("CURSOR_CONFIG_DIR"), "XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME")}
	for _, entry := range extraEnv {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	if dir := strings.TrimSpace(values["CURSOR_CONFIG_DIR"]); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(values["XDG_CONFIG_HOME"]); dir != "" {
		return filepath.Join(dir, "cursor")
	}
	return filepath.Join(home, ".cursor")
}

func resolveCursorBackend(backend, workDir, sessionID string, extraEnv []string) (bool, error) {
	useACP := backend == "acp"
	if sessionID == "" || sessionID == core.ContinueSession {
		return useACP, nil
	}
	if !validCursorSessionID(sessionID) {
		return false, fmt.Errorf("cursor: invalid session identifier")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("cursor: locate session storage: %w", err)
	}
	configDir := cursorConfigDir(home, extraEnv)
	if info, err := os.Stat(filepath.Join(cursorACPDir(configDir, sessionID), "store.db")); err == nil && info.Mode().IsRegular() {
		// An ACP conversation remains on ACP if the default later changes.
		return true, nil
	}
	if !useACP {
		return false, nil
	}
	source := filepath.Join(configDir, "chats", workspaceHash(workDir), sessionID)
	if _, err := os.Stat(filepath.Join(source, "store.db")); err != nil {
		source, _ = findCursorSessionDir(home, workDir, sessionID)
	}
	if source != "" {
		if err := migrateCursorSessionToACP(configDir, workDir, sessionID, source); err != nil {
			return false, err
		}
	}
	return true, nil // strict ACP resume reports missing stores without losing the ID
}
