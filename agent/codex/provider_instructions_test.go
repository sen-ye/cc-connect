package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestProviderInstructions_CurrentModelPromptWithoutChangingHistory(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "es1-grape-g61s"} {
		t.Run(model, func(t *testing.T) {
			prompt := "Instructions selected by the client for " + model
			body := `{"model":` + tomlString(model) + `,"input":[
{"type":"additional_tools","role":"developer","id":"at_keep","tools":[{"type":"function","name":"test"}]},
{"type":"message","role":"developer","content":[{"type":"input_text","text":` + tomlString(prompt+"\n") + `}]},
{"type":"message","role":"developer","content":"later permissions must not become the base prompt"},
{"type":"reasoning","id":"rs_keep","encrypted_content":"cipher-unchanged","summary":[]},
{"type":"function_call","id":"fc_keep","call_id":"call_keep","name":"test","arguments":"{}"},
{"type":"function_call_output","call_id":"call_keep","output":"result unchanged"}],
"client_metadata":{"large_integer":9007199254740993},"prompt_cache_key":"stable","tools":[],"parallel_tool_calls":false}`
			out, changed := mirrorDeveloperInstructions([]byte(body))
			if !changed {
				t.Fatal("missing instructions were not supplied")
			}
			var after, before map[string]json.RawMessage
			_ = json.Unmarshal(out, &after)
			_ = json.Unmarshal([]byte(body), &before)
			var got string
			_ = json.Unmarshal(after["instructions"], &got)
			if got != prompt {
				t.Fatalf("instructions = %q, want client-selected prompt %q", got, prompt)
			}
			delete(after, "instructions")
			for key, expected := range before {
				var a, b bytes.Buffer
				_ = json.Compact(&a, expected)
				_ = json.Compact(&b, after[key])
				if !bytes.Equal(a.Bytes(), b.Bytes()) {
					t.Fatalf("changed request field %s", key)
				}
			}
			if len(before) != len(after) {
				t.Fatal("added unrelated request fields")
			}
			if twice, changed := mirrorDeveloperInstructions(out); changed || !bytes.Equal(twice, out) {
				t.Fatal("repeated mirroring changed an already supplied prompt")
			}
		})
	}
}

func TestProviderInstructions_ExplicitValuesAndUnusableSourcesPreserved(t *testing.T) {
	for _, existing := range []string{`"caller override"`, `123`, `{}`, `[]`, `false`} {
		body := `{"instructions":` + existing + `,"input":[{"role":"developer","content":"model instructions"}]}`
		if got, changed := mirrorDeveloperInstructions([]byte(body)); changed || string(got) != body {
			t.Fatalf("overrode caller instructions: %s", existing)
		}
	}
	for _, body := range []string{
		`{"input":[{"role":"user","content":"no base prompt"}]}`,
		`{"input":[{"role":"developer","content":" "},{"role":"developer","content":"later"}]}`,
		`{"input":[{"role":"developer","content":[{"type":"input_image","image_url":"keep"}]},{"role":"developer","content":"later"}]}`,
		`{"input":[{"role":"developer","content":[{"type":"input_text"}]}]}`,
		`{"input":"text input"}`, `{"messages":[],"input":[{"role":"developer","content":"chat"}]}`,
		`{"input":`, `null`, `[]`,
	} {
		if got, changed := mirrorDeveloperInstructions([]byte(body)); changed || string(got) != body {
			t.Fatalf("changed unusable source: %s", body)
		}
	}
	for _, existing := range []string{`null`, `""`, `" \n "`} {
		body := `{"instructions":` + existing + `,"input":[{"role":"developer","content":"model prompt"}]}`
		if _, changed := mirrorDeveloperInstructions([]byte(body)); !changed {
			t.Fatalf("did not fill empty instructions %s", existing)
		}
	}
}

func TestProviderInstructions_ProxyPreservesSSEErrorsAndRoutingHeaders(t *testing.T) {
	const stream = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	requests := make(chan replayCapturedRequest, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- replayCapture(t, r)
		w.Header().Set("X-Codex-Turn-State", "opaque-test-state")
		w.Header().Set("X-Modelhub-Account-Id", "account-test")
		if r.URL.Query().Get("fail") == "1" {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()
	p, local, err := newProviderProxyWithOptions(upstream.URL+"/openai", "", providerProxyOptions{MirrorDeveloperInstructions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	body := `{"input":[{"type":"message","role":"developer","content":"current model prompt"}]}`
	for _, fail := range []string{"0", "1"} {
		request, _ := http.NewRequest(http.MethodPost, local+"/responses?fail="+fail, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-credential")
		request.Header.Set("Extra", `{"session_id":"stable-parent"}`)
		request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"stable-turn"}`)
		status, headers, output := replayReadResponse(t, replayTestClient(t), request)
		if headers.Get("X-Codex-Turn-State") != "opaque-test-state" || headers.Get("X-Modelhub-Account-Id") != "account-test" {
			t.Fatal("dropped upstream state headers")
		}
		if fail == "0" && (status != 200 || string(output) != stream) {
			t.Fatal("re-encoded or truncated successful SSE")
		}
		if fail == "1" && (status != 429 || headers.Get("Retry-After") != "2" || string(output) != `{"error":{"code":"rate_limit_exceeded"}}`) {
			t.Fatal("wrapped a provider error or lost retry guidance")
		}
		got := <-requests
		if got.headers.Get("Authorization") != "Bearer test-credential" || got.headers.Get("Extra") != `{"session_id":"stable-parent"}` || got.headers.Get("X-Codex-Turn-Metadata") != `{"turn_id":"stable-turn"}` || got.length != int64(len(got.body)) {
			t.Fatal("changed auth, affinity, metadata or body framing")
		}
		if !bytes.Contains(got.body, []byte(`"instructions":"current model prompt"`)) {
			t.Fatal("proxy did not inject instructions")
		}
	}
	for _, path := range []string{"/responses/compact", "/models"} {
		request, _ := http.NewRequest(http.MethodPost, local+path, strings.NewReader(body))
		_, _, _ = replayReadResponse(t, replayTestClient(t), request)
		if got := <-requests; string(got.body) != body {
			t.Fatalf("rewrote %s", path)
		}
	}
}

func TestProviderInstructions_StandaloneListenerIsPrivateAndCancellable(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", ":8080", "[::]:8080", "example.com:8080", "127.0.0.1"} {
		if validateProviderListenAddress(address) == nil {
			t.Fatalf("accepted public or invalid listener %q", address)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() {
		done <- ServeProviderProxy(ctx, "127.0.0.1:0", core.ProviderConfig{Name: "test", BaseURL: "http://127.0.0.1:1", CodexMirrorDeveloperInstructions: mirrorInstructionsValue(true)}, func(url string) error { ready <- url; return nil })
	}()
	var address string
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("listener not ready")
	}
	request, _ := http.NewRequest(http.MethodGet, address+"/_cc_connect_health", nil)
	status, _, body := replayReadResponse(t, replayTestClient(t), request)
	var health struct {
		OK       bool   `json:"ok"`
		Service  string `json:"service"`
		Provider string `json:"provider"`
	}
	if json.Unmarshal(body, &health) != nil || status != 200 || !health.OK || health.Service != "codex-provider" || health.Provider != "test" {
		t.Fatalf("health = %d %s", status, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not stop")
	}
}

func TestProviderInstructions_ProviderScopeDoesNotAffectOtherProfiles(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()
	body := `{"input":[{"role":"developer","content":"model prompt"}]}`
	for _, setting := range []*bool{nil, mirrorInstructionsValue(true), mirrorInstructionsValue(false)} {
		enabled := setting == nil || *setting
		provider := core.ProviderConfig{Name: "test", BaseURL: upstream.URL, CodexMirrorDeveloperInstructions: setting}
		if !enabled {
			// The official deployment still needs its fixed network proxy. Its
			// explicit exception must suppress instructions on that path too.
			provider.CodexProxyURL = upstream.URL
		}
		runtime, err := prepareProviderRuntime(&provider, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.proxy.Close()
		request, _ := http.NewRequest(http.MethodPost, runtime.baseURL+"/responses", strings.NewReader(body))
		_, _, output := replayReadResponse(t, replayTestClient(t), request)
		var got map[string]any
		_ = json.Unmarshal(output, &got)
		if (got["instructions"] == "model prompt") != enabled {
			t.Fatalf("provider opt-in ignored: %v", enabled)
		}
		if !reflect.DeepEqual(got["input"], []any{map[string]any{"role": "developer", "content": "model prompt"}}) {
			t.Fatal("provider changed original history")
		}
	}
}

func TestProviderInstructions_ConcurrentCallersKeepTheirIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string `json:"instructions"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil ||
			r.Header.Get("Authorization") != "Bearer "+body.Instructions ||
			r.Header.Get("Extra") != body.Instructions ||
			r.Header.Get("X-Session-Id") != body.Instructions {
			t.Error("relay mixed credentials, affinity or prompts between callers")
		}
		w.Header().Set("X-Codex-Turn-State", body.Instructions)
		_, _ = io.WriteString(w, body.Instructions)
	}))
	defer upstream.Close()
	p, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{
		MirrorDeveloperInstructions: true, ListenAddress: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client := replayTestClient(t)
	var wg sync.WaitGroup
	for _, identity := range []string{"astra-caller", "sol-caller", "seed-caller", "different-key"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"input":[{"role":"developer","content":` + tomlString(identity) + `}]}`
			request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+identity)
			request.Header.Set("Extra", identity)
			request.Header.Set("X-Session-Id", identity)
			status, headers, output := replayReadResponse(t, client, request)
			if status != 200 || string(output) != identity || headers.Get("X-Codex-Turn-State") != identity {
				t.Error("relay mixed responses between callers")
			}
		}()
	}
	wg.Wait()
}

func mirrorInstructionsValue(value bool) *bool { return &value }
