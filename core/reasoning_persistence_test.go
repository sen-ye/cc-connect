package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The process boundary reports the effort it actually received, so journeys
// can verify runtime settings through the platform rather than private state.
type reasoningPersistenceAgent struct {
	cujAgent
	name, workDir, effort string
}

func (a *reasoningPersistenceAgent) Name() string       { return a.name }
func (a *reasoningPersistenceAgent) GetWorkDir() string { return a.workDir }
func (a *reasoningPersistenceAgent) AvailableReasoningEfforts() []string {
	return []string{"low", "medium", "high", "xhigh", "max"}
}
func (a *reasoningPersistenceAgent) GetReasoningEffort() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.effort
}
func (a *reasoningPersistenceAgent) SetReasoningEffort(effort string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.effort = effort
}
func (a *reasoningPersistenceAgent) WorkspaceAgentOptions() map[string]any {
	return map[string]any{"reasoning_effort": a.GetReasoningEffort()}
}
func (a *reasoningPersistenceAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	session, err := a.cujAgent.StartSession(ctx, id)
	if err == nil {
		s := session.(*cujAgentSession)
		s.mu.Lock()
		s.reply = "runtime effort: " + a.GetReasoningEffort()
		s.mu.Unlock()
	}
	return session, err
}

func TestReasoningCard_PersistsWorkspaceSelection(t *testing.T) {
	root := t.TempDir()
	workspace := normalizeWorkspacePath(t.TempDir())
	statePath := filepath.Join(root, "project.json")
	global := &reasoningPersistenceAgent{name: "reasoning-card", effort: "high"}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", global, []Platform{p}, filepath.Join(root, "sessions.json"), LangEnglish)
	e.SetProjectStateStore(NewProjectStateStore(statePath))
	e.SetMultiWorkspace(root, filepath.Join(root, "bindings.json"))
	e.workspaceBindings.Bind("project:test", "room", "room", workspace)
	ws := e.workspacePool.GetOrCreate(workspace)
	ws.agent = &reasoningPersistenceAgent{name: global.name, workDir: workspace, effort: "high"}
	ws.sessions = NewSessionManager("")

	card := e.handleCardNav("act:/reasoning max", "test:room:user")
	if got := ws.agent.(ReasoningEffortSwitcher).GetReasoningEffort(); got != "max" {
		t.Fatalf("workspace effort = %q, want max", got)
	}
	if global.effort != "high" {
		t.Fatalf("workspace card changed global effort to %q", global.effort)
	}
	if card == nil || !strings.Contains(card.RenderText(), "Current reasoning effort: max") {
		t.Fatal("expected refreshed card to show the workspace's max effort")
	}
	data, err := os.ReadFile(statePath)
	if err != nil || !strings.Contains(string(data), `"max"`) {
		t.Fatalf("saved state = %s, error = %v; want persisted max", data, err)
	}
}
