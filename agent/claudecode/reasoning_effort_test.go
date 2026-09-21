package claudecode

import (
	"context"
	"os"
	"slices"
	"testing"
)

func TestAvailableReasoningEfforts_IncludesXHigh(t *testing.T) {
	a := &Agent{}
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if got := a.AvailableReasoningEfforts(); !slices.Equal(got, want) {
		t.Fatalf("AvailableReasoningEfforts() = %v, want %v", got, want)
	}
}

// Regression: xhigh was absent from the menu and silently normalized to an
// empty value, preventing both /reasoning and config from reaching the CLI.
func TestReasoningEffort_XHighReachesCLI(t *testing.T) {
	for _, source := range []string{"config", "command"} {
		t.Run(source, func(t *testing.T) {
			opts := map[string]any{
				"cmd":         os.Args[0],
				"work_dir":    t.TempDir(),
				"cc_data_dir": t.TempDir(),
			}
			if source == "config" {
				opts["reasoning_effort"] = " xHigh "
			}
			agent, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			a := agent.(*Agent)
			if source == "command" {
				a.SetReasoningEffort(" XHIGH ")
			}
			if got := a.GetReasoningEffort(); got != "xhigh" {
				t.Fatalf("GetReasoningEffort() = %q, want xhigh", got)
			}
			if got := a.WorkspaceAgentOptions()["reasoning_effort"]; got != "xhigh" {
				t.Fatalf("workspace reasoning_effort = %v, want xhigh", got)
			}

			// Use the existing Claude process stub; never call a real model.
			a.cliExtraArgs = []string{"-test.run=TestHelperProcess", "--", "claude-stdin-echo"}
			a.configEnv = []string{"GO_WANT_HELPER_PROCESS=1"}
			session, err := a.StartSession(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			args := session.(*claudeSession).cmd.Args
			i := slices.Index(args, "--effort")
			if i < 0 || i+1 >= len(args) || args[i+1] != "xhigh" {
				t.Fatalf("Claude CLI args = %v, want --effort xhigh", args)
			}
		})
	}
}
