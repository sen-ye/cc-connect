package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quotaResponse(status int, retry string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{retry}}}
}

func TestProviderCooldown_Exponential32AndSuccessReset(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newProviderCooldown(32*time.Second, "test")
	g.now = func() time.Time { return now }
	key, _ := providerQuotaKey(http.Header{"Api-Key": []string{"test-key"}})
	for _, seconds := range []int{2, 4, 8, 16, 32, 32} {
		lease, err := g.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		lease.finish(quotaResponse(429, ""), nil, false)
		if got := g.states[key].until.Sub(now); got != time.Duration(seconds)*time.Second {
			t.Fatalf("shared cooldown = %s, want %ds", got, seconds)
		}
		now = g.states[key].until
	}
	lease, _ := g.acquire(context.Background(), key)
	lease.finish(quotaResponse(200, ""), nil, false)
	if len(g.states) != 0 {
		t.Fatal("successful probe did not reset cooldown")
	}
	lease, _ = g.acquire(context.Background(), key)
	lease.finish(quotaResponse(429, ""), nil, false)
	if g.states[key].until.Sub(now) != 2*time.Second {
		t.Fatal("new failure did not restart at 2s")
	}
}

func TestProviderCooldown_ServerMinimumAndOldResponses(t *testing.T) {
	for _, headers := range []http.Header{
		{"Retry-After": []string{"55"}},
		{"Retry-After-Ms": []string{"55000"}},
		{"Retry-After": []string{time.Unix(1055, 0).UTC().Format(http.TimeFormat)}},
	} {
		g := newProviderCooldown(32*time.Second, "test")
		now := time.Unix(1000, 0)
		g.now = func() time.Time { return now }
		key, _ := providerQuotaKey(http.Header{"Api-Key": []string{"test-key"}})
		first, _ := g.acquire(context.Background(), key)
		old, _ := g.acquire(context.Background(), key)
		first.finish(&http.Response{StatusCode: 429, Header: headers}, nil, false)
		old.finish(quotaResponse(200, ""), nil, false)
		if g.states[key].until.Sub(now) != 55*time.Second {
			t.Fatal("shortened server minimum or cleared cooldown using an old response")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := g.acquire(ctx, key); err != context.Canceled {
			t.Fatal("wait ignored cancellation")
		}
		if g.states[key].waiters != 0 {
			t.Fatal("canceled waiter was retained")
		}
	}
}

func TestProviderCooldown_DailyQuotaAndNoRetryAreNotRetried(t *testing.T) {
	for _, headers := range []http.Header{
		{"X-Ratelimit-Dimension": []string{"daily_key_model"}},
		{"X-Ratelimit-Dimension": []string{"daily_key_total"}},
		{"X-Should-Retry": []string{"false"}},
	} {
		g := newProviderCooldown(32*time.Second, "test")
		key, _ := providerQuotaKey(http.Header{"Api-Key": []string{"test-key"}})
		lease, _ := g.acquire(context.Background(), key)
		lease.finish(&http.Response{StatusCode: 429, Header: headers}, nil, false)
		if len(g.states) != 0 {
			t.Fatal("scheduled cooldown for non-retryable quota")
		}
	}
}

func waitCooldownWaiters(t *testing.T, g *providerCooldown, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if g.snapshot()["waiting_requests"] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting requests = %v, want %d", g.snapshot(), want)
}

func TestProviderCooldown_CancelQueuedRequestDoesNotForward(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "55")
		w.WriteHeader(429)
	}))
	defer upstream.Close()
	p, address, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{RateLimitBackoffMaxSeconds: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client := replayTestClient(t)
	first, _ := http.NewRequest(http.MethodPost, address+"/responses", strings.NewReader(`{"input":[]}`))
	first.Header.Set("api-key", "shared-key")
	replayReadResponse(t, client, first)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued, _ := http.NewRequestWithContext(ctx, http.MethodPost, address+"/responses", strings.NewReader(`{"input":[]}`))
	queued.Header.Set("api-key", "shared-key")
	done := make(chan error, 1)
	go func() {
		r, err := client.Do(queued)
		if r != nil {
			r.Body.Close()
		}
		done <- err
	}()
	waitCooldownWaiters(t, p.cooldown, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
	waitCooldownWaiters(t, p.cooldown, 0)
	if calls.Load() != 1 {
		t.Fatal("canceled queued request reached the upstream")
	}
}

func TestProviderCooldown_SharedKeyWaitsSeriallyWhileOtherKeyFlows(t *testing.T) {
	const stream = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	entered := make(chan string, 10)
	release := make(chan struct{}, 3)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		caller := r.Header.Get("X-Test-Caller")
		if r.Header.Get("Extra") != caller {
			t.Error("affinity changed")
		}
		entered <- caller
		if caller == "first" {
			w.Header().Set("Retry-After", "55")
			w.Header().Set("X-RateLimit-Dimension", "rpm_key")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, "original limit body")
			return
		}
		if strings.HasPrefix(caller, "queued") {
			<-release
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()
	defer close(release)
	p, address, err := newProviderProxyWithOptions(upstream.URL, "", providerProxyOptions{RateLimitBackoffMaxSeconds: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client := replayTestClient(t)
	request := func(caller, key string, bearer bool) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, address+"/responses", strings.NewReader(`{"input":[]}`))
		if bearer {
			r.Header.Set("Authorization", "Bearer "+key)
		} else {
			r.Header.Set("api-key", key)
		}
		r.Header.Set("X-Test-Caller", caller)
		r.Header.Set("Extra", caller)
		return r
	}
	status, headers, body := replayReadResponse(t, client, request("first", "shared-key", false))
	if status != 429 || headers.Get("Retry-After") != "55" || string(body) != "original limit body" || calls.Load() != 1 {
		t.Fatal("first failed request was changed or automatically replayed")
	}
	<-entered
	done := make(chan error, 2)
	for i, caller := range []string{"queued-a", "queued-b"} {
		go func(caller string, bearer bool) {
			response, err := client.Do(request(caller, "shared-key", bearer))
			if err == nil {
				data, e := io.ReadAll(response.Body)
				response.Body.Close()
				err = e
				if response.StatusCode != 200 || string(data) != stream {
					t.Error("SSE response changed")
				}
			}
			done <- err
		}(caller, i == 1)
	}
	waitCooldownWaiters(t, p.cooldown, 2)
	status, _, _ = replayReadResponse(t, client, request("independent", "other-key", false))
	if status != 200 || <-entered != "independent" {
		t.Fatal("cooldown blocked a different key")
	}
	if calls.Load() != 2 {
		t.Fatal("same-key request escaped the shared cooldown")
	}
	key, _ := providerQuotaKey(http.Header{"Api-Key": []string{"shared-key"}})
	p.cooldown.mu.Lock()
	p.cooldown.states[key].until = time.Now()
	p.cooldown.signal(p.cooldown.states[key])
	p.cooldown.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no probe after cooldown")
	}
	select {
	case <-entered:
		t.Fatal("multiple probes hit the upstream together")
	case <-time.After(20 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queued request did not advance after accepted probe")
	}
	release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("proxy replayed a request")
	}
}
