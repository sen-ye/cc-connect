package cursor

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorACPMigration_SnapshotPreservesOriginalAndRejectsOverwrite(t *testing.T) {
	var err error
	home := filepath.Join(t.TempDir(), "quoted'home")
	source := filepath.Join(home, "print-chat")
	if err = os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(source, "store.db")
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec("CREATE TABLE meta(key TEXT,value TEXT); INSERT INTO meta VALUES('marker','original-history');"); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateCursorSessionToACP(home, source, "test-session", source); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(db)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("migration modified the original chat")
	}
	target := filepath.Join(cursorACPDir(home, "test-session"), "store.db")
	conn, err = sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var marker string
	if err = conn.QueryRow("SELECT value FROM meta WHERE key='marker';").Scan(&marker); err != nil || marker != "original-history" {
		t.Fatalf("history missing from snapshot: %q %v", marker, err)
	}
	if err = migrateCursorSessionToACP(home, source, "test-session", source); err == nil {
		t.Fatal("existing ACP history overwritten")
	}
	if err = migrateCursorSessionToACP(home, source, "../escape", source); err == nil {
		t.Fatal("accepted unsafe session path")
	}
}

func TestCursorBackend_OptionsSurviveWorkspaceClone(t *testing.T) {
	a, err := New(map[string]any{"cmd": os.Args[0], "backend": "acp", "model": "gemini-4-argon-high", "mode": "ask", "env": map[string]string{"ROUTE": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	clone, err := New(a.(*Agent).WorkspaceAgentOptions())
	if err != nil {
		t.Fatal(err)
	}
	if clone.(*Agent).backend != "acp" || clone.(*Agent).model != "gemini-4-argon-high" || len(clone.(*Agent).configEnv) != 1 {
		t.Fatal("lost ACP launch configuration")
	}
	if got := cursorACPModel("gemini-4-argon-high"); got != "gemini-4-argon[reasoning_effort=high]" {
		t.Fatal(got)
	}
	for raw, want := range map[string]string{
		"grok-4.7-high":       "grok-4.7[reasoning_effort=high,fast=false]",
		"grok-4.7-xhigh-fast": "grok-4.7[reasoning_effort=xhigh,fast=true]",
		"grok-4.7[fast=true]": "grok-4.7[fast=true]",
	} {
		if got := cursorACPModel(raw); got != want {
			t.Errorf("%s = %s, want %s", raw, got, want)
		}
	}
	if _, err := New(map[string]any{"cmd": os.Args[0], "backend": "typo"}); err == nil {
		t.Fatal("unknown backend silently accepted")
	}
}
