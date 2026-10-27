package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/core"
)

const providerCLIHelperMode = "CC_CONNECT_PROVIDER_CLI_TEST_MODE"

type providerCLIObservation struct {
	BaseURL string   `json:"base_url"`
	Args    []string `json:"args"`
}

// The shell only adds Go's test selector before Codex's flags, then execs the
// real helper process. This avoids mocking process lifetime or HTTP transport.
func providerCLIFixture(t *testing.T, upstream, mode string) (string, core.ProviderConfig, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper trampoline requires a POSIX shell")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "native-codex")
	script := "#!/bin/sh\nexec \"$CC_CONNECT_PROVIDER_CLI_TEST_BINARY\" -test.run=^TestProviderCLI_HelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"config.toml": "model_provider = \"original-provider\"\nmodel = \"original-model\"\n",
		"auth.json":   "{\"OPENAI_API_KEY\":\"original-test-only-key\"}\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != content {
				t.Errorf("launcher changed shared %s: read error=%v", name, err)
			}
		})
	}
	report := filepath.Join(dir, "child-observation.json")
	p := core.ProviderConfig{
		Name: "Seed test", Model: "gpt-6-astra", APIKey: "launcher-test-only-key",
		BaseURL: upstream + "/openai?api-version=2024-03-01-preview",
		CodexHTTPHeaders: map[string]string{
			"api-key": "launcher-test-only-header", "x-session-id": "launcher-affinity",
		},
		CodexReasoningReplayFallback: true,
		Env: map[string]string{
			"CODEX_HOME": dir, providerCLIHelperMode: mode,
			"CC_CONNECT_PROVIDER_CLI_TEST_BINARY": binary,
			"CC_CONNECT_PROVIDER_CLI_TEST_REPORT": report,
		},
	}
	return launcher, p, report
}

func providerCLIReadObservation(t *testing.T, path string) providerCLIObservation {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("child observation missing: %v", err)
	}
	var observation providerCLIObservation
	if err := json.Unmarshal(b, &observation); err != nil {
		t.Fatal(err)
	}
	return observation
}

func providerCLIAssertProxyClosed(t *testing.T, baseURL string) {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("child did not receive a loopback provider: %q", baseURL)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 250*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("provider proxy is still listening after the child exited")
	}
}

func TestProviderCLI_ReplaysEncryptedHistoryAndPreservesStdio(t *testing.T) {
	const body = "{\n \"model\":\"gpt-6-astra\",\"prompt_cache_key\":\"same-thread\",\"input\":[" +
		`{"type":"message","role":"user","content":"continue \\u4e2d"},` +
		`{"type":"reasoning","id":"rs_old","encrypted_content":"old-test-cipher","summary":[{"type":"summary_text","text":"earlier summary"}]},` +
		`{"type":"function_call","call_id":"call_saved","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_saved","output":"saved result"}]}` + "\n"
	const answer = "\x1b[32mnative answer\x1b[0m\r\n\x00end\n"
	requests := make(chan replayCapturedRequest, 3)
	var count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- replayCapture(t, r)
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, replayRefusal)
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	defer upstream.Close()
	binary, provider, report := providerCLIFixture(t, upstream.URL, "request")
	original := cloneProvider(provider)
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"resume", "native-test-session", "prompt with spaces"}
	err := RunProviderCLI(ctx, binary, args, provider, strings.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatalf("launcher failed: %v; stderr=%q", err, stderr.String())
	}
	if stdout.String() != answer || stderr.String() != "native stderr\r\n" {
		t.Fatalf("stdio was not transparent: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !reflect.DeepEqual(provider, original) {
		t.Error("launcher mutated the caller's provider configuration")
	}
	if count.Load() != 2 {
		t.Fatalf("upstream requests=%d, want original plus one retry", count.Load())
	}
	first, second := <-requests, <-requests
	if string(first.body) != body {
		t.Error("first request changed stdin bytes before forwarding")
	}
	for _, request := range []replayCapturedRequest{first, second} {
		if request.uri != "/openai/responses?api-version=2024-03-01-preview&client=launcher" {
			t.Errorf("request route changed: %q", request.uri)
		}
		if request.headers.Get("api-key") != "launcher-test-only-header" ||
			request.headers.Get("Authorization") != "Bearer launcher-test-only-key" ||
			request.headers.Get("x-session-id") != "launcher-affinity" {
			t.Error("process-local authentication or session affinity was lost")
		}
	}
	var repaired map[string]json.RawMessage
	if err := json.Unmarshal(second.body, &repaired); err != nil {
		t.Fatal(err)
	}
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(repaired["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 4 || string(repaired["prompt_cache_key"]) != `"same-thread"` {
		t.Fatal("repair discarded conversation history or changed cache affinity")
	}
	if _, ok := input[1]["encrypted_content"]; ok {
		t.Error("repair retained rejected encrypted reasoning")
	}
	if _, ok := input[1]["id"]; ok {
		t.Error("repair retained the rejected reasoning item id")
	}
	if !bytes.Contains(input[1]["summary"], []byte("earlier summary")) ||
		string(input[2]["call_id"]) != `"call_saved"` || string(input[3]["call_id"]) != `"call_saved"` {
		t.Error("repair lost readable summary or tool call linkage")
	}
	observation := providerCLIReadObservation(t, report)
	if n := len(observation.Args); n < len(args) || !reflect.DeepEqual(observation.Args[n-len(args):], args) {
		t.Error("native resume arguments were not preserved")
	}
	if strings.Contains(strings.Join(observation.Args, " "), "launcher-test-only-") {
		t.Error("launcher placed credentials in child argv")
	}
	providerCLIAssertProxyClosed(t, observation.BaseURL)
}

func TestProviderCLI_PreservesExitCodeAndClosesProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("exit-only child unexpectedly contacted the upstream")
	}))
	defer upstream.Close()
	binary, provider, report := providerCLIFixture(t, upstream.URL, "exit")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunProviderCLI(ctx, binary, nil, provider, strings.NewReader(""), &stdout, &stderr)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 41 {
		t.Fatalf("native exit status lost: %v", err)
	}
	if stdout.String() != "native partial output\n" || stderr.String() != "native failure\n" {
		t.Errorf("failure stdio changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	providerCLIAssertProxyClosed(t, providerCLIReadObservation(t, report).BaseURL)
}

func TestProviderCLI_ContextCancellationStopsChildAndUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(8 * time.Second):
			t.Error("upstream did not observe canceled request")
		}
	}))
	defer upstream.Close()
	binary, provider, report := providerCLIFixture(t, upstream.URL, "request")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunProviderCLI(ctx, binary, nil, provider, strings.NewReader(`{"input":[]}`), &stdout, &stderr)
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("child stopped before making a request: %v; stderr=%q", err, stderr.String())
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("child never started its request")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("canceled child reported successful completion")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("launcher failed to reap its child after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Error("closing the launcher did not cancel the upstream request")
	}
	if stdout.Len() != 0 {
		t.Errorf("launcher wrote proxy chatter to native stdout: %q", stdout.String())
	}
	providerCLIAssertProxyClosed(t, providerCLIReadObservation(t, report).BaseURL)
}

func TestProviderCLI_HelperProcess(t *testing.T) {
	mode := os.Getenv(providerCLIHelperMode)
	if mode == "" {
		return
	}
	fail := func(message string) {
		fmt.Fprintln(os.Stderr, message)
		os.Exit(96)
	}
	var settings []string
	var nativeArgs []string
	for i, arg := range os.Args {
		if arg == "--" {
			nativeArgs = os.Args[i+1:]
			break
		}
	}
	for i := 0; i < len(nativeArgs); i++ {
		if nativeArgs[i] == "-c" && i+1 < len(nativeArgs) {
			i++
			settings = append(settings, nativeArgs[i])
		}
	}
	var cfg struct {
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			BaseURL string            `toml:"base_url"`
			EnvKey  string            `toml:"env_key"`
			Headers map[string]string `toml:"env_http_headers"`
		} `toml:"model_providers"`
	}
	if _, err := toml.Decode(strings.Join(settings, "\n"), &cfg); err != nil {
		fail("invalid process-local provider TOML")
	}
	provider, ok := cfg.Providers[cfg.Provider]
	if !ok || provider.BaseURL == "" || os.Getenv("OPENAI_BASE_URL") != provider.BaseURL {
		fail("missing process-local provider routing")
	}
	report, _ := json.Marshal(providerCLIObservation{BaseURL: provider.BaseURL, Args: nativeArgs})
	if err := os.WriteFile(os.Getenv("CC_CONNECT_PROVIDER_CLI_TEST_REPORT"), report, 0600); err != nil {
		fail("cannot record child observation")
	}
	if mode == "exit" {
		_, _ = io.WriteString(os.Stdout, "native partial output\n")
		_, _ = io.WriteString(os.Stderr, "native failure\n")
		os.Exit(41)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail("cannot read native stdin")
	}
	request, err := http.NewRequest(http.MethodPost, provider.BaseURL+"/responses?client=launcher", bytes.NewReader(input))
	if err != nil {
		fail("cannot form native request")
	}
	request.Header.Set("Content-Type", "application/json")
	if provider.EnvKey != "" {
		request.Header.Set("Authorization", "Bearer "+os.Getenv(provider.EnvKey))
	}
	for key, envKey := range provider.Headers {
		request.Header.Set(key, os.Getenv(envKey))
	}
	client := &http.Client{Transport: &http.Transport{}, Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		fail("native request failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		fail("native request returned an error status")
	}
	if _, err := io.Copy(os.Stdout, response.Body); err != nil {
		response.Body.Close()
		fail("cannot relay native stdout")
	}
	response.Body.Close()
	client.CloseIdleConnections()
	_, _ = io.WriteString(os.Stderr, "native stderr\r\n")
	os.Exit(0)
}
