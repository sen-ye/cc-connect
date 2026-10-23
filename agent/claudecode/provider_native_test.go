package claudecode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt-in live check. Normal tests never use operator credentials or model APIs.
// The settings file is read-only; transcripts live in a private temporary home.
func TestNativeClaude_SettingsProviderMultiTurnAndResume(t *testing.T) {
	bin, settings := os.Getenv("CC_CLAUDE_NATIVE_TEST_BIN"), os.Getenv("CC_CLAUDE_PROVIDER_SETTINGS")
	if bin == "" || settings == "" {
		t.Skip("set CC_CLAUDE_NATIVE_TEST_BIN and CC_CLAUDE_PROVIDER_SETTINGS for live validation")
	}
	original, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(map[string]any{
		"cmd": bin, "work_dir": t.TempDir(), "cc_data_dir": t.TempDir(),
		"env": map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	agent.cliExtraArgs = append(agent.cliExtraArgs, "--tools", "")
	agent.SetProviders([]core.ProviderConfig{{Name: "settings-test", SettingsFile: settings}})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	s, err := agent.StartSessionWithProvider(ctx, "", "settings-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	send := func(session core.AgentSession, prompt, want string) {
		t.Helper()
		if err := session.Send(prompt, "native-provider-check", nil, nil); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case event, ok := <-session.Events():
				if !ok {
					t.Fatal("native session exited before answering")
				}
				if event.Type == core.EventToolUse {
					t.Fatal("live check unexpectedly invoked a tool")
				}
				if event.Type == core.EventError {
					t.Fatal(event.Error)
				}
				if event.Type == core.EventResult && event.Done && !event.Background {
					if strings.TrimSpace(event.Content) != want {
						t.Fatal("native reply did not preserve the expected conversation marker")
					}
					return
				}
			case <-ctx.Done():
				t.Fatal("native provider check timed out")
			}
		}
	}
	marker := fmt.Sprintf("provider-%d", time.Now().UnixNano())
	send(s, "Remember the marker "+marker+". Reply exactly READY. Do not use tools.", "READY")
	send(s, "Reply only with the original marker. Do not use tools.", marker)
	id := s.CurrentSessionID()
	if id == "" {
		t.Fatal("native session has no resumable ID")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := agent.StartSessionWithProvider(ctx, id, "settings-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	send(resumed, "Reply only with the original marker. Do not use tools.", marker)
	if resumed.CurrentSessionID() != id || agent.GetActiveProvider() != nil {
		t.Fatal("resume changed the native ID or the project default")
	}
	after, err := os.ReadFile(settings)
	if err != nil || string(after) != string(original) {
		t.Fatal("native check changed the original settings file")
	}
}
