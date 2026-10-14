package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/core"
)

func TestProviderRuntimeConfig_SecretsAndAffinity(t *testing.T) {
	provider := core.ProviderConfig{
		Name: "key.a 测试", APIKey: "fake-secret-a", BaseURL: "http://localhost/responses",
		CodexHTTPHeaders:    map[string]string{"api-key": "fake-header-secret", "X-Title": "test"},
		CodexEnvHTTPHeaders: map[string]string{"x-session-id": "TEST_AFFINITY"},
	}
	args, env := providerRuntimeConfig(provider)
	if strings.Contains(strings.Join(args, " "), "fake-") {
		t.Fatal("secret exposed in argv")
	}
	var config struct {
		Providers map[string]struct {
			EnvKey       string            `toml:"env_key"`
			Headers      map[string]string `toml:"env_http_headers"`
			RequiresAuth bool              `toml:"requires_openai_auth"`
		} `toml:"model_providers"`
	}
	if _, err := toml.Decode(args[1], &config); err != nil {
		t.Fatal(err)
	}
	p := config.Providers[provider.Name]
	values := envToMap(env)
	if values[p.EnvKey] != provider.APIKey || p.RequiresAuth {
		t.Fatal("wrong process-local authentication")
	}
	if values[p.Headers["api-key"]] != "fake-header-secret" {
		t.Fatal("literal header was not passed privately")
	}
	if p.Headers["x-session-id"] != "TEST_AFFINITY" {
		t.Fatal("lost session affinity header mapping")
	}
}

func TestStartSessionWithProvider_UnknownProviderDoesNotFallback(t *testing.T) {
	a := &Agent{activeIdx: -1}
	a.SetProviders([]core.ProviderConfig{{Name: "key-a", APIKey: "fake-a"}})
	a.SetActiveProvider("key-a")
	if _, err := a.StartSessionWithProvider(context.Background(), "original", "missing"); err == nil {
		t.Fatal("missing binding silently fell back to default")
	}
}

func TestStartSessionWithProvider_DefaultPreservesCLIRouting(t *testing.T) {
	a := &Agent{backend: "exec", activeIdx: -1, configEnv: []string{"AIDP_API_KEY=current-test-key"}}
	a.SetProviders([]core.ProviderConfig{{Name: "other", APIKey: "other-test-key", BaseURL: "http://other.invalid/v1"}})
	if a.GetActiveProvider() != nil {
		t.Fatal("adding an API changed the original default")
	}
	s, err := a.StartSessionWithProvider(context.Background(), "original-native", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cs := s.(*codexSession)
	if cs.modelProvider != "" || len(cs.providerArgs) != 0 {
		t.Fatal("overrode the CLI's existing provider")
	}
	if got := envToMap(cs.extraEnv); got["AIDP_API_KEY"] != "current-test-key" || got[providerAPIKeyEnv] != "" {
		t.Fatal("default credential environment changed")
	}
}

func TestWorkspaceAgentOptions_PreserveProviderEnvironment(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cmd: bin, cliExtraArgs: []string{"-test.run=^TestCodexProviderProcessHelper$", "--"}, backend: "app_server", codexHome: t.TempDir(), activeIdx: -1,
		configEnv:  []string{"CC_CONNECT_PROVIDER_TEST_CHILD=1", "PROFILE_KEY=fake-workspace-key", "AFFINITY=workspace-affinity"},
		sessionEnv: []string{"CC_SESSION_KEY=must-not-copy"}}
	opts := a.WorkspaceAgentOptionSnapshot()
	opts["work_dir"] = t.TempDir()
	created, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	clone := created.(*Agent)
	clone.SetProviders([]core.ProviderConfig{{Name: "workspace", CodexEnvKey: "PROFILE_KEY", CodexEnvHTTPHeaders: map[string]string{"x-session-id": "AFFINITY"}}})
	s, err := clone.StartSessionWithProvider(context.Background(), "", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertProviderReply(t, s, testProviderFingerprint("fake-workspace-key", "workspace", "workspace-affinity"))
	if len(clone.sessionEnv) != 0 {
		t.Fatal("workspace inherited another conversation's runtime environment")
	}
}

func TestStartSessionWithProvider_ConcurrentProcessesAndResume(t *testing.T) {
	for _, backend := range []string{"exec", "app_server"} {
		t.Run(backend, func(t *testing.T) {
			home, work := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{\"OPENAI_API_KEY\":\"legacy\"}"), 0600); err != nil {
				t.Fatal(err)
			}
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			a := &Agent{activeIdx: -1, backend: backend, cmd: bin, codexHome: home, workDir: work,
				cliExtraArgs: []string{"-test.run=^TestCodexProviderProcessHelper$", "--"},
				configEnv:    []string{"CC_CONNECT_PROVIDER_TEST_CHILD=1"},
			}
			providers := []core.ProviderConfig{
				{Name: "key-a", APIKey: "fake-key-a", BaseURL: "http://localhost/a", Model: "gpt-6-astra", CodexEnvHTTPHeaders: map[string]string{"x-session-id": "AFFINITY"}, Env: map[string]string{"AFFINITY": "session-a"}},
				{Name: "key-b", APIKey: "fake-key-b", BaseURL: "http://localhost/b", Model: "gpt-6-astra", CodexEnvHTTPHeaders: map[string]string{"x-session-id": "AFFINITY"}, Env: map[string]string{"AFFINITY": "session-b"}},
			}
			a.SetProviders(providers)
			a.SetActiveProvider("key-b")
			var wg sync.WaitGroup
			for _, provider := range providers {
				wg.Add(1)
				go func(provider core.ProviderConfig) {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					s, err := a.StartSessionWithProvider(ctx, "", provider.Name)
					if err != nil {
						t.Error(err)
						return
					}
					want := testProviderFingerprint(provider.APIKey, provider.Name, provider.Env["AFFINITY"])
					if !assertProviderReply(t, s, want) {
						s.Close()
						return
					}
					thread := s.CurrentSessionID()
					if !assertProviderReply(t, s, want) {
						s.Close()
						return
					}
					s.Close()
					resumed, err := a.StartSessionWithProvider(ctx, thread, provider.Name)
					if err != nil {
						t.Error(err)
						return
					}
					defer resumed.Close()
					assertProviderReply(t, resumed, want)
					if resumed.CurrentSessionID() != thread {
						t.Error("resume changed native session ID")
					}
				}(provider)
			}
			wg.Wait()
			if a.GetActiveProvider().Name != "key-b" {
				t.Fatal("session selection mutated default")
			}
			data, err := os.ReadFile(filepath.Join(home, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "{\"OPENAI_API_KEY\":\"legacy\"}" {
				t.Fatal("overwrote shared credentials")
			}
			if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
				t.Fatal("created shared provider configuration")
			}
		})
	}
}

func assertProviderReply(t *testing.T, s core.AgentSession, want string, waitForExit ...bool) bool {
	t.Helper()
	if err := s.Send("report account", "test-message", nil, nil); err != nil {
		t.Error(err)
		return false
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var text strings.Builder
	for {
		select {
		case event, ok := <-s.Events():
			if !ok {
				t.Error("closed before response")
				return false
			}
			if event.Type == core.EventError {
				t.Errorf("agent error: %s", event.Content)
				return false
			}
			if event.Type == core.EventText {
				text.WriteString(event.Content)
			}
			if event.Type == core.EventResult {
				text.WriteString(event.Content)
				if !strings.Contains(text.String(), want) {
					t.Errorf("response %q, want %q", text.String(), want)
					return false
				}
				// exec emits its result immediately before process reaping.
				if cs, ok := s.(*codexSession); ok && (len(waitForExit) == 0 || waitForExit[0]) {
					deadline := time.Now().Add(time.Second)
					for cs.turnInFlight.Load() && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if cs.turnInFlight.Load() {
						t.Error("exec process did not exit after completing its turn")
						return false
					}
				}
				return true
			}
		case <-timer.C:
			t.Error("timed out waiting for account response")
			return false
		}
	}
}

func testProviderFingerprint(key, name, affinity string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("account=%x provider=%s affinity=%s", sum[:6], name, affinity)
}

// A real subprocess boundary verifies what each CLI receives, including the
// provider chosen in app-server start/resume requests. No external API is used.
func TestCodexProviderProcessHelper(t *testing.T) {
	if os.Getenv("CC_CONNECT_PROVIDER_TEST_CHILD") != "1" {
		return
	}
	var cfg struct {
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			EnvKey  string            `toml:"env_key"`
			Headers map[string]string `toml:"env_http_headers"`
		} `toml:"model_providers"`
	}
	var settings []string
	appServer := false
	for i, arg := range os.Args {
		if arg == "app-server" {
			appServer = true
		}
		if arg == "-c" && i+1 < len(os.Args) {
			settings = append(settings, os.Args[i+1])
		}
	}
	if _, err := toml.Decode(strings.Join(settings, "\n"), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	p := cfg.Providers[cfg.Provider]
	answer := testProviderFingerprint(os.Getenv(p.EnvKey), cfg.Provider, os.Getenv(p.Headers["x-session-id"]))
	thread := "native-" + cfg.Provider
	out := json.NewEncoder(os.Stdout)
	if !appServer {
		out.Encode(map[string]any{"type": "thread.started", "thread_id": thread})
		out.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": answer}})
		out.Encode(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		os.Exit(0)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(3)
		}
		if len(request.ID) == 0 {
			continue
		}
		result := map[string]any{}
		switch request.Method {
		case "thread/start", "thread/resume":
			if request.Params["modelProvider"] != cfg.Provider {
				out.Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "provider binding missing from thread request"}})
				continue
			}
			if id, ok := request.Params["threadId"].(string); ok {
				thread = id
			}
			result = map[string]any{"thread": map[string]any{"id": thread}, "model": "gpt-6-astra", "reasoningEffort": "xhigh"}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "test-turn"}}
		}
		out.Encode(map[string]any{"id": request.ID, "result": result})
		if request.Method == "turn/start" {
			out.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": thread, "turn": map[string]any{"id": "test-turn", "status": "inProgress"}}})
			out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": thread, "turnId": "test-turn", "item": map[string]any{"type": "agentMessage", "text": answer}}})
			out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": thread, "turn": map[string]any{"id": "test-turn", "status": "completed"}}})
		}
	}
	os.Exit(0)
}
