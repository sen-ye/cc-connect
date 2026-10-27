package codex

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const replayRefusal = `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}`

const replayInput = `{
  "model": "gpt-6-astra",
  "prompt_cache_key": "replay-cache-identity",
  "previous_response_id": "resp_unchanged",
  "max_output_tokens": 64,
  "input": [
    {"type":"message","id":"msg_keep","role":"user","content":"continue"},
    {"type":"reasoning","id":"rs_drop","status":"completed","encrypted_content":"replay-cipher-secret","content":[{"type":"reasoning_text","text":"opaque-content"}],"summary":[{"type":"summary_text","text":"keep this summary"}]},
    {"type":"reasoning","id":"rs_empty","encrypted_content":"replay-empty-secret","summary":[]},
    {"type":"reasoning","id":"rs_plain","summary":[{"type":"summary_text","text":"already plain"}]},
    {"type":"function_call","id":"fc_keep","call_id":"call_keep","name":"read","arguments":"{}"},
    {"type":"function_call_output","id":"fo_keep","call_id":"call_keep","output":"tool output"},
    {"type":"custom_tool_call","id":"custom_keep","call_id":"custom_call","name":"exec","input":"literal code"},
    {"type":"custom_tool_call_output","call_id":"custom_call","output":"custom result"},
    {"type":"compaction","id":"cmp_keep","encrypted_content":"compaction-kept"}
  ]
}`

type replayCapturedRequest struct {
	method, uri string
	headers     http.Header
	body        []byte
	length      int64
}

func replayCapture(t *testing.T, r *http.Request) replayCapturedRequest {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upstream request: %v", err)
	}
	return replayCapturedRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), body, r.ContentLength}
}

func replayTestClient(t *testing.T) *http.Client {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func replayReadResponse(t *testing.T, client *http.Client, request *http.Request) (int, http.Header, []byte) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, body
}

type replayLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *replayLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *replayLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestProviderReplay_FirstRequestIntactAndRetryPreservesIdentity(t *testing.T) {
	logs := &replayLogBuffer{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	requests := make(chan replayCapturedRequest, 3)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- replayCapture(t, r)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, replayRefusal)
			return
		}
		w.Header().Set("X-Request-Id", "retry-response-id")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(
		upstream.URL+"/azure/openai?api-version=2024-03-01-preview&route_secret=replay-url-secret", "",
		providerProxyOptions{ReasoningReplayFallback: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	request, err := http.NewRequest(http.MethodPost, local+"/responses?client=test%2Bidentity", strings.NewReader(replayInput))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Api-Key", "replay-header-secret")
	request.Header.Set("Authorization", "Bearer replay-bearer-secret")
	request.Header.Set("X-Session-Id", "replay-session-identity")
	request.Header.Set("Extra", `{"session_id":"replay-session-identity","keep":"other-extra-field"}`)
	request.Header.Set("X-Tt-Logid", "replay-log-identity")
	status, headers, body := replayReadResponse(t, replayTestClient(t), request)
	if status != http.StatusOK || string(body) != `{"ok":true}` || headers.Get("X-Request-Id") != "retry-response-id" {
		t.Fatalf("retry response not forwarded: status=%d body=%q", status, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	first, retry := <-requests, <-requests
	if !bytes.Equal(first.body, []byte(replayInput)) {
		t.Fatal("first request was rewritten before the provider rejected it")
	}
	if first.uri != retry.uri || !strings.Contains(first.uri, "/azure/openai/responses?") ||
		!strings.Contains(first.uri, "api-version=2024-03-01-preview") || !strings.Contains(first.uri, "client=test%2Bidentity") {
		t.Fatal("retry changed the endpoint or its fixed query parameters")
	}
	for _, key := range []string{"Api-Key", "Authorization", "X-Session-Id", "Extra", "X-Tt-Logid", "Content-Type"} {
		if first.headers.Get(key) != request.Header.Get(key) || retry.headers.Get(key) != first.headers.Get(key) {
			t.Errorf("retry lost fixed header %s", key)
		}
	}
	if first.method != http.MethodPost || retry.method != first.method || retry.length != int64(len(retry.body)) {
		t.Fatal("retry changed the method or did not recalculate Content-Length")
	}
	var actual, expected map[string]any
	if err := json.Unmarshal(retry.body, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{
  "model":"gpt-6-astra", "prompt_cache_key":"replay-cache-identity",
  "previous_response_id":"resp_unchanged", "max_output_tokens":64,
  "input":[
    {"type":"message","id":"msg_keep","role":"user","content":"continue"},
    {"type":"reasoning","summary":[{"type":"summary_text","text":"keep this summary"}]},
    {"type":"reasoning","id":"rs_plain","summary":[{"type":"summary_text","text":"already plain"}]},
    {"type":"function_call","id":"fc_keep","call_id":"call_keep","name":"read","arguments":"{}"},
    {"type":"function_call_output","id":"fo_keep","call_id":"call_keep","output":"tool output"},
    {"type":"custom_tool_call","id":"custom_keep","call_id":"custom_call","name":"exec","input":"literal code"},
    {"type":"custom_tool_call_output","call_id":"custom_call","output":"custom result"},
    {"type":"compaction","id":"cmp_keep","encrypted_content":"compaction-kept"}
  ]
}`), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("fallback changed ordinary items, continuation/cache identity, or reasoning-summary semantics")
	}
	for _, secret := range []string{"replay-header-secret", "replay-bearer-secret", "replay-url-secret", "replay-cipher-secret", "opaque-content"} {
		if strings.Contains(logs.String(), secret) || bytes.Contains(body, []byte(secret)) {
			t.Error("compatibility layer leaked a synthetic credential or encrypted request payload")
		}
	}
}

func TestProviderReplay_DirectModeIgnoresInheritedProxy(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	// Loopback is automatically exempted by Go's environment proxy function;
	// also assert it is disabled so real internal hostnames get the same route.
	if proxy.transport.Proxy != nil {
		t.Fatal("internal Router traffic can still inherit a process-wide proxy")
	}
	request, _ := http.NewRequest(http.MethodGet, local+"/health", nil)
	status, _, body := replayReadResponse(t, replayTestClient(t), request)
	if status != 200 || string(body) != "direct" {
		t.Fatal("local compatibility proxy did not use the direct route")
	}
}

func TestProviderReplay_OnlyKnownUnstartedResponsesAreRetried(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte(replayInput)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, encoding, requestBody, responseBody string
		status, wantCalls                                       int
		disabled                                                bool
	}{
		{name: "known_error", status: 400, wantCalls: 2},
		{name: "identity_encoding", encoding: "identity", status: 400, wantCalls: 2},
		{name: "disabled", status: 400, wantCalls: 1, disabled: true},
		{name: "already_started_sse", status: 200, responseBody: "event: response.failed\ndata: " + replayRefusal + "\n\n", wantCalls: 1},
		{name: "unrelated_400", status: 400, responseBody: `{"error":{"code":"invalid_request","message":"other failure"}}`, wantCalls: 1},
		{name: "conflicting_identities_not_supported", status: 400, responseBody: `{"error":{"message":"Conflicting authenticated continuation identities"}}`, wantCalls: 1},
		{name: "rate_limit", status: 429, wantCalls: 1},
		{name: "server_failure", status: 503, wantCalls: 1},
		{name: "get", method: http.MethodGet, status: 400, wantCalls: 1},
		{name: "other_endpoint", path: "/chat/completions", status: 400, wantCalls: 1},
		{name: "input_string", requestBody: `{"input":"invalid_encrypted_content"}`, status: 400, wantCalls: 1},
		{name: "invalid_json", requestBody: `{invalid_encrypted_content`, status: 400, wantCalls: 1},
		{name: "messages_shape", requestBody: `{"input":[{"type":"reasoning","encrypted_content":"old"}],"messages":[]}`, status: 400, wantCalls: 1},
		{name: "no_encrypted_reasoning", requestBody: `{"input":[{"type":"reasoning","summary":[]}]}`, status: 400, wantCalls: 1},
		{name: "compaction_only", requestBody: `{"input":[{"type":"compaction","encrypted_content":"old"}]}`, status: 400, wantCalls: 1},
		{name: "compressed", encoding: "gzip", requestBody: compressed.String(), status: 400, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, path, requestBody, errorBody := tc.method, tc.path, tc.requestBody, tc.responseBody
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = "/responses"
			}
			if requestBody == "" {
				requestBody = replayInput
			}
			if errorBody == "" {
				errorBody = replayRefusal
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured := replayCapture(t, r)
				if calls.Add(1) == 1 {
					if string(captured.body) != requestBody {
						t.Error("first request changed")
					}
					if tc.status == 200 {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, errorBody)
					return
				}
				_, _ = io.WriteString(w, "retried")
			}))
			t.Cleanup(upstream.Close)
			proxyURL := ""
			if tc.disabled {
				proxyURL = upstream.URL
			}
			proxy, local, err := newProviderProxyWithOptions(upstream.URL+"/v1", proxyURL, providerProxyOptions{ReasoningReplayFallback: !tc.disabled})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(proxy.Close)
			request, err := http.NewRequest(method, local+path, strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			if tc.encoding != "" {
				request.Header.Set("Content-Encoding", tc.encoding)
			}
			status, _, body := replayReadResponse(t, replayTestClient(t), request)
			if calls.Load() != int32(tc.wantCalls) {
				t.Fatalf("calls=%d, want %d", calls.Load(), tc.wantCalls)
			}
			if tc.wantCalls == 1 && (status != tc.status || string(body) != errorBody) {
				t.Fatal("non-retried response was altered or truncated")
			}
			if tc.wantCalls == 2 && (status != 200 || string(body) != "retried") {
				t.Fatal("retry response was not forwarded")
			}
		})
	}
}

func TestProviderReplay_SecondFailureIsReturnedWithoutRecursion(t *testing.T) {
	for _, secondStatus := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(secondStatus), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				n := calls.Add(1)
				status := http.StatusBadRequest
				if n > 1 {
					status = secondStatus
				}
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"invalid_encrypted_content","attempt":%d}}`, n)
			}))
			t.Cleanup(upstream.Close)
			proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(proxy.Close)
			request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(replayInput))
			status, headers, body := replayReadResponse(t, replayTestClient(t), request)
			if calls.Load() != 2 || status != secondStatus || string(body) != `{"error":{"code":"invalid_encrypted_content","attempt":2}}` || headers.Get("Retry-After") != "7" {
				t.Fatalf("wrong second-failure propagation: calls=%d status=%d", calls.Load(), status)
			}
		})
	}
}

func TestProviderReplay_RetrySSEStreamsAndCancellationReachesUpstream(t *testing.T) {
	var calls atomic.Int32
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, replayRefusal)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: retry-stream-first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, local+"/responses", strings.NewReader(replayInput))
	response, err := replayTestClient(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: retry-stream-first\n" || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("second response buffered or corrupted: line=%q error=%v", line, err)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("retry continued upstream after client cancellation")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
}

func TestProviderReplay_CatalogNeverEntersFallback(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/catalog/responses" || r.URL.Query().Get("version") != "catalog" {
			t.Error("catalog route was lost")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, replayRefusal)
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(upstream.URL+"/v1", "", providerProxyOptions{
		ReasoningReplayFallback: true, ModelCatalogURL: upstream.URL + "/catalog/responses?version=catalog",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	// Even a POST to the local catalog endpoint is catalog traffic, not a model
	// request, despite the configured remote path ending in /responses.
	request, _ := http.NewRequest(http.MethodPost, local+providerCatalogPath, strings.NewReader(replayInput))
	status, _, body := replayReadResponse(t, replayTestClient(t), request)
	if calls.Load() != 1 || status != 400 || string(body) != replayRefusal {
		t.Fatal("catalog response triggered a replay or was changed")
	}
}

func TestProviderReplay_OversizedRequestIsForwardedIntactWithoutReplay(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"old"}],"padding":"` + strings.Repeat("x", int(maxReplayRequestBytes)+1) + `"}`)
	wantHash := sha256.Sum256(body)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hash := sha256.New()
		size, err := io.Copy(hash, r.Body)
		if err != nil || size != int64(len(body)) || !bytes.Equal(hash.Sum(nil), wantHash[:]) {
			t.Error("oversized request was truncated or rewritten")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, replayRefusal)
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	request, _ := http.NewRequest(http.MethodPost, local+"/responses", bytes.NewReader(body))
	status, _, response := replayReadResponse(t, replayTestClient(t), request)
	if calls.Load() != 1 || status != 400 || string(response) != replayRefusal {
		t.Fatal("oversized request entered replay instead of intact pass-through")
	}
}

func TestProviderReplay_OversizedErrorIsForwardedIntactWithoutReplay(t *testing.T) {
	errorBody := []byte(`{"error":{"code":"invalid_encrypted_content"},"padding":"` + strings.Repeat("e", int(maxReplayErrorBytes)+1) + `"}`)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(errorBody)
	}))
	t.Cleanup(upstream.Close)
	proxy, local, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(replayInput))
	status, _, response := replayReadResponse(t, replayTestClient(t), request)
	if calls.Load() != 1 || status != 400 || !bytes.Equal(response, errorBody) {
		t.Fatal("oversized upstream error was truncated, changed, or retried")
	}
}

func TestProviderReplay_LocalTransportErrorDoesNotLeakSecrets(t *testing.T) {
	logs := &replayLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	upstream := httptest.NewServer(http.NotFoundHandler())
	url := strings.Replace(upstream.URL, "http://", "http://replay-user:replay-password@", 1) + "/v1?api_key=replay-query-secret"
	upstream.Close()
	proxy, local, err := newProviderProxyWithOptions(url, "", providerProxyOptions{ReasoningReplayFallback: true})
	if err != nil {
		t.Fatal("valid upstream URL rejected")
	}
	t.Cleanup(proxy.Close)
	request, _ := http.NewRequest(http.MethodPost, local+"/responses", strings.NewReader(replayInput))
	request.Header.Set("Api-Key", "replay-header-secret")
	status, _, body := replayReadResponse(t, replayTestClient(t), request)
	if status != http.StatusBadGateway {
		t.Fatalf("transport failure status=%d", status)
	}
	for _, secret := range []string{"replay-password", "replay-query-secret", "replay-header-secret", "replay-cipher-secret"} {
		if strings.Contains(logs.String(), secret) || bytes.Contains(body, []byte(secret)) {
			t.Error("local transport error leaked a synthetic credential or encrypted request payload")
		}
	}
}
