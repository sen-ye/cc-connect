package core

import (
	"path/filepath"
	"testing"
)

// Regression: saveLocked's manual Session copy omitted ActiveProvider, even
// though the field was tagged for JSON and restored by the engine on resume.
func TestSessionManager_ActiveProviderSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	s := sm.GetOrCreateActive("test:topic")
	s.SetActiveProvider("key-a")
	s.SetAgentSessionID("native-original", "test")
	sm.Save()
	reloaded := NewSessionManager(path).GetOrCreateActive("test:topic")
	if got := reloaded.GetActiveProvider(); got != "key-a" {
		t.Fatalf("provider after restart = %q, want key-a", got)
	}
}
