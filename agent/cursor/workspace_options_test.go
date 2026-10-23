package cursor

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestWorkspaceAgentOptions_PreservesCustomLauncherAndModelDiscovery(t *testing.T) {
	t.Setenv("CURSOR_TEST_ROUTE", "wrong-inherited-route")
	command := []string{os.Args[0], "-test.run=TestCursorModelListHelper", "--", "wrapper-profile"}
	agent, err := New(map[string]any{
		"cmd": command, "work_dir": t.TempDir(), "mode": "force", "model": "gemini-4-argon-high",
		"env": map[string]string{"GO_WANT_CURSOR_MODEL_HELPER": "1", "CURSOR_TEST_ROUTE": "configured-route"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := agent.(*Agent)
	a.SetSessionEnv([]string{"TRANSIENT_SESSION=parent-only"})
	snapshotter, ok := agent.(core.WorkspaceAgentOptionSnapshotter)
	if !ok {
		t.Fatal("new workspaces would discard the configured launcher")
	}
	opts := snapshotter.WorkspaceAgentOptions()
	opts["work_dir"] = t.TempDir()
	childAgent, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	child := childAgent.(*Agent)
	if child.cmd != command[0] || !reflect.DeepEqual(child.cliExtraArgs, command[1:]) || child.GetModel() != "gemini-4-argon-high" || child.GetMode() != "force" {
		t.Fatal("workspace clone lost launcher, arguments, model or mode")
	}
	if len(child.sessionEnv) != 0 {
		t.Fatal("workspace inherited another conversation's runtime environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models := child.AvailableModels(ctx)
	if len(models) != 2 || models[0].Name != "gemini-4-argon-high" || models[1].Name != "grok-4.7-high" {
		t.Fatalf("model discovery lost custom launcher arguments or env: %#v", models)
	}
	opts["env"].(map[string]string)["CURSOR_TEST_ROUTE"] = "changed"
	opts["cmd"].([]string)[0] = "changed"
	if a.cmd != command[0] || slices.Contains(a.configEnv, "CURSOR_TEST_ROUTE=changed") {
		t.Fatal("editing a workspace snapshot mutated the parent")
	}
}

func TestAvailableModels_PreservesConfiguredLauncherArgumentsAndEnvironment(t *testing.T) {
	t.Setenv("CURSOR_TEST_ROUTE", "wrong-inherited-route")
	a := &Agent{cmd: os.Args[0], cliExtraArgs: []string{"-test.run=TestCursorModelListHelper", "--", "wrapper-profile"},
		configEnv: []string{"GO_WANT_CURSOR_MODEL_HELPER=1", "CURSOR_TEST_ROUTE=configured-route"}, activeIdx: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if models := a.AvailableModels(ctx); len(models) != 2 || models[1].Name != "grok-4.7-high" {
		t.Fatalf("configured model discovery fell back: %#v", models)
	}
}

func TestCursorModelListHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CURSOR_MODEL_HELPER") != "1" {
		return
	}
	if os.Getenv("CURSOR_TEST_ROUTE") != "configured-route" || !slices.Contains(os.Args, "wrapper-profile") || os.Args[len(os.Args)-1] != "models" {
		os.Exit(2)
	}
	fmt.Println("gemini-4-argon-high - Gemini 4 Argon High")
	fmt.Println("grok-4.7-high - Grok 4.7 High")
	os.Exit(0)
}
