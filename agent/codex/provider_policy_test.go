package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestProviderOutputLimit_DefaultOnlyAndUnrelatedPayloadsUnchanged(t *testing.T) {
	for _, input := range []string{
		`{"input":[],"model":"gpt-6-astra"}`,
		`{"input":[],"max_output_tokens":null}`,
		`{"input":[],"max_output_tokens":0}`,
	} {
		got, changed := injectProviderOutputLimit([]byte(input), 64000)
		var body map[string]json.RawMessage
		if !changed || json.Unmarshal(got, &body) != nil || string(body["max_output_tokens"]) != "64000" {
			t.Errorf("missing output budget was not supplied: %s", input)
		}
	}
	for _, input := range []string{
		`{"input":[],"max_output_tokens":4096}`, `{"input":[],"max_output_tokens":128000}`,
		`{"input":[],"max_output_tokens":-1}`, `{"input":[],"max_output_tokens":"invalid"}`,
		`{"input":[],"max_output_tokens":false}`, `{"messages":[],"input":[]}`,
		`{"input":"hello"}`, `{"input":null}`, `null`, `[]`, `{"input":`,
	} {
		got, changed := injectProviderOutputLimit([]byte(input), 64000)
		if changed || string(got) != input {
			t.Errorf("changed an explicit budget or non-Responses body: %s", input)
		}
	}
}

func TestProviderOutputLimit_ThenReasoningRepairPreservesOtherFields(t *testing.T) {
	input := strings.Replace(replayInput, `"max_output_tokens": 64,`, `"large_integer": 9007199254740993,`, 1)
	requests := make(chan replayCapturedRequest, 2)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- replayCapture(t, r)
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, replayRefusal)
			return
		}
		_, _ = io.WriteString(w, "OK")
	}))
	defer upstream.Close()
	proxy, local, err := newProviderProxyWithOptions(upstream.URL+"/openai?api-version=test", "", providerProxyOptions{MaxOutputTokens: 64000, ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	request, _ := http.NewRequest(http.MethodPost, local+"/responses?client=unchanged", strings.NewReader(input))
	request.Header.Set("api-key", "test-secret")
	request.Header.Set("X-Session-Id", "same-affinity")
	status, _, response := replayReadResponse(t, replayTestClient(t), request)
	if status != 200 || string(response) != "OK" || calls.Load() != 2 {
		t.Fatalf("request failed: %d %q calls=%d", status, response, calls.Load())
	}
	first, second := <-requests, <-requests
	for _, got := range []replayCapturedRequest{first, second} {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(got.body, &body); err != nil {
			t.Fatal(err)
		}
		if string(body["max_output_tokens"]) != "64000" || string(body["large_integer"]) != "9007199254740993" ||
			got.headers.Get("api-key") != "test-secret" || got.headers.Get("X-Session-Id") != "same-affinity" || got.length != int64(len(got.body)) {
			t.Error("budget/retry changed identity, numeric precision, or content length")
		}
	}
	if first.uri != second.uri || !bytes.Contains(first.body, []byte("replay-cipher-secret")) || bytes.Contains(second.body, []byte("replay-cipher-secret")) {
		t.Error("reasoning repair changed route or ran before explicit refusal")
	}
}

func TestProviderOutputLimit_NeverRewritesCompactionCatalogOrCompressedBody(t *testing.T) {
	requests := make(chan replayCapturedRequest, 6)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- replayCapture(t, r)
		_, _ = io.WriteString(w, "OK")
	}))
	defer upstream.Close()
	proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{MaxOutputTokens: 64000, ModelCatalogURL: upstream.URL + "/catalog"})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	for _, tc := range []struct{ method, path, encoding string }{
		{"POST", "/responses/compact", ""}, {"GET", "/responses", ""},
		{"POST", providerCatalogPath, ""}, {"POST", "/chat/completions", ""}, {"POST", "/responses", "gzip"},
	} {
		const original = "{ \"input\": [] }\n"
		request, _ := http.NewRequest(tc.method, local+tc.path, strings.NewReader(original))
		request.Header.Set("Content-Encoding", tc.encoding)
		replayReadResponse(t, replayTestClient(t), request)
		if got := <-requests; string(got.body) != original {
			t.Errorf("rewrote %s %s encoding=%q", tc.method, tc.path, tc.encoding)
		}
	}
}

func TestProviderPolicy_429RetainsRetryHeadersAndNeverAddsRetries(t *testing.T) {
	logs := &replayLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(previous)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-RateLimit-Dimension", "rpm_key")
		w.Header().Set("X-RateLimit-Reset", "1793095440")
		w.Header().Set("X-Should-Retry", "false")
		w.Header().Set("X-Tt-Logid", "20261027180359FCDF5D0BB5E22F3A6EC6")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":"rate-limit-body-secret"}`)
	}))
	defer upstream.Close()
	proxy, local, err := newProviderProxyWithOptions(upstream.URL+"?secret=url-secret", "", providerProxyOptions{MaxOutputTokens: 64000, ReasoningReplayFallback: true, ProviderName: "Seed"})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(`{"input":[]}`))
	request.Header.Set("api-key", "header-secret")
	status, headers, body := replayReadResponse(t, replayTestClient(t), request)
	if status != 429 || string(body) != `{"error":"rate-limit-body-secret"}` || calls.Load() != 1 {
		t.Fatal("HTTP 429 was rewritten, retried, or consumed")
	}
	for key, want := range map[string]string{"Retry-After": "17", "X-RateLimit-Dimension": "rpm_key", "X-RateLimit-Reset": "1793095440", "X-Should-Retry": "false"} {
		if headers.Get(key) != want {
			t.Errorf("lost %s", key)
		}
	}
	output := logs.String()
	if !strings.Contains(output, "rate_limit_dimension=rpm_key") || !strings.Contains(output, "retry_after_seconds=17") || !strings.Contains(output, "provider=Seed") {
		t.Fatal("missing rate-limit diagnostic")
	}
	for _, secret := range []string{"url-secret", "header-secret", "rate-limit-body-secret"} {
		if strings.Contains(output, secret) {
			t.Error("diagnostic exposed a request or response secret")
		}
	}
}

func TestProviderPolicy_IndependentProviderBudgetsAndNegativeValidation(t *testing.T) {
	if _, _, err := newProviderProxyWithOptions("http://localhost", "", providerProxyOptions{MaxOutputTokens: -1}); err == nil {
		t.Fatal("accepted negative output budget")
	}
	provider := core.ProviderConfig{Name: "bad", BaseURL: "http://localhost", CodexMaxOutputTokens: -1}
	if _, err := prepareProviderRuntime(&provider, nil); err == nil {
		t.Fatal("runtime accepted negative output budget")
	}
	seen := make(chan int, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Limit int `json:"max_output_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen <- body.Limit
		_, _ = io.WriteString(w, "OK")
	}))
	defer upstream.Close()
	for _, limit := range []int{64000, 4096} {
		provider := core.ProviderConfig{Name: "independent", BaseURL: upstream.URL, CodexMaxOutputTokens: limit}
		original := provider
		cfg, err := prepareProviderRuntime(&provider, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cfg.proxy.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/responses", strings.NewReader(`{"input":[]}`))
		replayReadResponse(t, replayTestClient(t), request)
		cancel()
		if got := <-seen; got != original.CodexMaxOutputTokens || original.CodexMaxOutputTokens != limit {
			t.Error("provider output budget leaked across proxy instances")
		}
	}
}

func TestProviderRetryAfter_ParsesMetadataWithoutTrustingArbitraryHeaders(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Duration{"1": time.Second, "1.5": 1500 * time.Millisecond, now.Add(time.Minute).Format(http.TimeFormat): time.Minute} {
		if got, ok := providerRetryAfter(raw, now); !ok || got != want {
			t.Errorf("retry-after %q = %s, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"secret", "NaN", "Inf", "-1", "86401", now.Add(-time.Second).Format(http.TimeFormat)} {
		if _, ok := providerRetryAfter(raw, now); ok {
			t.Errorf("accepted invalid retry header %q", raw)
		}
	}
}

func TestProviderReplay_ContinuationIdentityOnlyWithEncryptedReasoning(t *testing.T) {
	const refusal = `{"error":{"message":"Conflicting authenticated continuation identities"}}`
	for _, tc := range []struct {
		input string
		calls int
	}{
		{`{"input":[{"type":"reasoning","encrypted_content":"cipher","summary":[]}]}`, 2},
		{`{"input":[{"type":"reasoning","content":["plain"],"summary":[]}]}`, 1},
		{`{"input":[{"type":"compaction","encrypted_content":"cipher"}]}`, 1},
		{`{"input":[]}`, 1},
	} {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, refusal)
				return
			}
			var got map[string]any
			_ = json.NewDecoder(r.Body).Decode(&got)
			if !reflect.DeepEqual(got["input"], []any{}) {
				t.Error("conflicting ciphertext was not removed")
			}
			_, _ = io.WriteString(w, "OK")
		}))
		proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(tc.input))
		replayReadResponse(t, replayTestClient(t), request)
		proxy.Close()
		upstream.Close()
		if int(calls.Load()) != tc.calls {
			t.Errorf("wrong retry count for %s: %d", tc.input, calls.Load())
		}
	}
}
