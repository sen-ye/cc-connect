package cursor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	_ "modernc.org/sqlite"
)

// Copy a consistent SQLite snapshot into ACP storage. The original print-mode
// database is retained. Publication is atomic; incomplete copies never resume.
func migrateCursorSessionToACP(configDir, workDir, id, source string) error {
	if !validCursorSessionID(id) {
		return fmt.Errorf("cursor: invalid session identifier")
	}
	target := cursorACPDir(configDir, id)
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("cursor: ACP migration target already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("cursor: create ACP storage: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".cc-connect-migrate-")
	if err != nil {
		return fmt.Errorf("cursor: prepare ACP migration: %w", err)
	}
	defer os.RemoveAll(tmp)
	db := filepath.Join(tmp, "store.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sql := "VACUUM INTO '" + strings.ReplaceAll(db, "'", "''") + "';"
	if err := snapshotCursorDB(ctx, filepath.Join(source, "store.db"), db, sql); err != nil {
		return fmt.Errorf("cursor: snapshot print session for ACP: %w", err)
	}
	if err := os.Chmod(db, 0600); err != nil {
		return err
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{"schemaVersion": 1, "cwd": workDir, "title": readSessionMeta(db).Name})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), metadata, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("cursor: publish ACP session snapshot: %w", err)
	}
	return nil
}

func snapshotCursorDB(ctx context.Context, source, target, statement string) error {
	openReadOnly := func(path string) (*sql.DB, error) {
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
		return sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)")
	}
	in, err := openReadOnly(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := in.ExecContext(ctx, statement); err != nil {
		return err
	}
	out, err := openReadOnly(target)
	if err != nil {
		return err
	}
	defer out.Close()
	var check string
	if err := out.QueryRowContext(ctx, "PRAGMA quick_check;").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("snapshot failed integrity check")
	}
	return nil
}

func listAllCursorSessions(workDir string, extraEnv []string) ([]core.AgentSessionInfo, error) {
	printSessions, err := listCursorSessions(workDir)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configDir := cursorConfigDir(home, extraEnv)
	entries, err := os.ReadDir(filepath.Dir(cursorACPDir(configDir, "placeholder")))
	if os.IsNotExist(err) {
		return printSessions, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cursor: list ACP sessions: %w", err)
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]core.AgentSessionInfo)
	for _, session := range printSessions {
		byID[session.ID] = session
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := cursorACPDir(configDir, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var metadata struct{ Cwd, Title string }
		if json.Unmarshal(data, &metadata) != nil || filepath.Clean(metadata.Cwd) != filepath.Clean(abs) {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, "store.db"))
		if err != nil {
			continue
		}
		byID[entry.Name()] = core.AgentSessionInfo{ID: entry.Name(), Summary: cursorSessionSummary(metadata.Title, "", entry.Name()), ModifiedAt: info.ModTime()}
	}
	result := make([]core.AgentSessionInfo, 0, len(byID))
	for _, session := range byID {
		result = append(result, session)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ModifiedAt.After(result[j].ModifiedAt) })
	return result, nil
}
