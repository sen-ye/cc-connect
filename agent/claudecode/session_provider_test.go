package claudecode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.SessionProviderStarter = (*Agent)(nil)

func newSessionProviderTestAgent(t *testing.T) *Agent {
	t.Helper()
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "native-test-token")
	a, err := New(map[string]any{"cmd": os.Args[0], "work_dir": t.TempDir(), "cc_data_dir": t.TempDir(), "model": "native-model"})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	agent.cliExtraArgs = []string{"-test.run=TestHelperProcess", "--", "claude-stdin-echo"}
	agent.configEnv = []string{"GO_WANT_HELPER_PROCESS=1"}
	return agent
}

func startBoundTestSession(t *testing.T, a *Agent, resumeID, provider string) *claudeSession {
	t.Helper()
	s, err := a.StartSessionWithProvider(context.Background(), resumeID, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.(*claudeSession)
}

func sessionFlag(s *claudeSession, flag string) string {
	i := slices.Index(s.cmd.Args, flag)
	if i >= 0 && i+1 < len(s.cmd.Args) {
		return s.cmd.Args[i+1]
	}
	return ""
}

// Regression for the pre-binding API: selecting a new default used to close
// the rewrite proxy that an already-running session still depended on.
func TestStartSession_ChangingProviderKeepsRunningProxy(t *testing.T) {
	a := newSessionProviderTestAgent(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "original account")
	}))
	defer upstream.Close()
	a.SetProviders([]core.ProviderConfig{
		{Name: "original", BaseURL: upstream.URL, Thinking: "disabled"},
		{Name: "next", BaseURL: "http://next.invalid"},
	})
	a.SetActiveProvider("original")
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	url := envSliceToMap(s.(*claudeSession).cmd.Env)["ANTHROPIC_BASE_URL"]
	a.SetActiveProvider("next")
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("changing provider disconnected the existing session: %v", err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if string(data) != "original account" {
		t.Fatal("existing session changed accounts")
	}
}

func TestSessionProvider_ConcurrentAccountsAndProxyLifetimes(t *testing.T) {
	a := newSessionProviderTestAgent(t)
	upstream := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, label)
		}))
	}
	first, second := upstream("first"), upstream("second")
	defer first.Close()
	defer second.Close()
	a.SetProviders([]core.ProviderConfig{
		{Name: "first", BaseURL: first.URL, APIKey: "key-first", Model: "model-first", Thinking: "disabled"},
		{Name: "second", BaseURL: second.URL, APIKey: "key-second", Model: "model-second", Thinking: "disabled"},
	})
	a.SetActiveProvider("first")
	var sessions [2]*claudeSession
	var errs [2]error
	var wg sync.WaitGroup
	for i, name := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := a.StartSessionWithProvider(context.Background(), "resume-"+name, name)
			errs[i] = err
			if err == nil {
				sessions[i] = s.(*claudeSession)
			}
		}()
	}
	wg.Wait()
	for i, session := range sessions {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		t.Cleanup(func() { _ = session.Close() })
	}
	if a.GetActiveProvider().Name != "first" {
		t.Fatal("starting an explicit provider changed the project default")
	}
	urls := make([]string, 2)
	for i, name := range []string{"first", "second"} {
		env := envSliceToMap(sessions[i].cmd.Env)
		if env["ANTHROPIC_AUTH_TOKEN"] != "key-"+name || sessionFlag(sessions[i], "--model") != "model-"+name || sessionFlag(sessions[i], "--resume") != "resume-"+name {
			t.Fatalf("provider %s lost its credentials, model or resume ID", name)
		}
		urls[i] = env["ANTHROPIC_BASE_URL"]
	}
	if urls[0] == urls[1] {
		t.Fatal("independent provider sessions share a rewrite proxy")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	read := func(url, want string) {
		t.Helper()
		res, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if string(body) != want {
			t.Fatalf("request reached %q, want %q", body, want)
		}
	}
	a.SetActiveProvider("second")
	read(urls[0], "first")
	read(urls[1], "second")
	if err := sessions[1].Close(); err != nil {
		t.Fatal(err)
	}
	read(urls[0], "first")
	if res, err := client.Get(urls[1]); err == nil {
		res.Body.Close()
		t.Fatal("closed session left its proxy listening")
	}
}

func TestSessionProvider_SettingsOverlayAndNativeResumeStayIsolated(t *testing.T) {
	a := newSessionProviderTestAgent(t)
	settings := filepath.Join(a.workDir, "provider settings.json")
	content := []byte("{\n// provider-specific overlay\n\"model\":\"overlay-model\",\"apiKeyHelper\":\"test-helper\"}\n")
	if err := os.WriteFile(settings, content, 0600); err != nil {
		t.Fatal(err)
	}
	a.SetProviders([]core.ProviderConfig{{Name: "overlay", SettingsFile: "provider settings.json"}})
	a.SetActiveProvider("overlay")
	overlay := startBoundTestSession(t, a, "overlay-native-id", "overlay")
	if sessionFlag(overlay, "--settings") != settings || sessionFlag(overlay, "--model") != "overlay-model" {
		t.Fatal("settings path/model did not reach the provider's process")
	}
	native := startBoundTestSession(t, a, "legacy-native-id", "")
	if sessionFlag(native, "--settings") != "" || sessionFlag(native, "--model") != "native-model" || sessionFlag(native, "--resume") != "legacy-native-id" {
		t.Fatal("legacy native resume picked up the new default provider")
	}
	if a.GetActiveProvider().Name != "overlay" {
		t.Fatal("native resume cleared the project default")
	}
	a.SetProviders([]core.ProviderConfig{{Name: "overlay", SettingsFile: settings, Model: "explicit-model"}})
	explicit := startBoundTestSession(t, a, "", "overlay")
	if sessionFlag(explicit, "--model") != "explicit-model" {
		t.Fatal("settings model overrode explicit provider model")
	}
	got, err := os.ReadFile(settings)
	if err != nil || string(got) != string(content) {
		t.Fatal("starting providers changed the settings file")
	}
}

func TestSessionProvider_UnavailableConfigurationCannotFallback(t *testing.T) {
	a := newSessionProviderTestAgent(t)
	invalid := filepath.Join(a.workDir, "invalid.json")
	if err := os.WriteFile(invalid, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	nullSettings := filepath.Join(a.workDir, "null.json")
	if err := os.WriteFile(nullSettings, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	a.SetProviders([]core.ProviderConfig{
		{Name: "missing-file", SettingsFile: "not-here.json"},
		{Name: "invalid-file", SettingsFile: invalid},
		{Name: "null-file", SettingsFile: nullSettings},
	})
	for _, name := range []string{"removed-provider", "missing-file", "invalid-file", "null-file"} {
		s, err := a.StartSessionWithProvider(context.Background(), "keep-native-id", name)
		if err == nil || s != nil {
			if s != nil {
				s.Close()
			}
			t.Fatalf("provider %s silently fell back to native configuration", name)
		}
	}
}

func TestSessionProvider_ExplicitRouteDoesNotUseDefaultRouter(t *testing.T) {
	a := newSessionProviderTestAgent(t)
	a.routerURL, a.routerAPIKey = "http://default-router.invalid", "router-token"
	a.SetProviders([]core.ProviderConfig{{Name: "custom", BaseURL: "http://custom.invalid", APIKey: "custom-token"}})
	s := startBoundTestSession(t, a, "", "custom")
	env := envSliceToMap(s.cmd.Env)
	if env["ANTHROPIC_BASE_URL"] != "http://custom.invalid" || env["ANTHROPIC_AUTH_TOKEN"] != "custom-token" || strings.Contains(env["ANTHROPIC_API_KEY"], "router") {
		t.Fatal("project router changed the explicit provider's route/authentication")
	}
}
