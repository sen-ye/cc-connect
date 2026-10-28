package codex

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Shared by callers of one provider listener. Only a digest of the credential
// identifies a quota bucket; no credential, history or affinity is retained.
type providerCooldown struct {
	mu       sync.Mutex
	states   map[[32]byte]*providerCooldownState
	base     time.Duration
	maximum  time.Duration
	provider string
	now      func() time.Time
}

type providerCooldownState struct {
	until      time.Time
	nextDelay  time.Duration
	probe      bool
	waiters    int
	generation uint64
	changed    chan struct{} // closed/replaced only while holding providerCooldown.mu
}

type providerCooldownLease struct {
	gate       *providerCooldown
	key        [32]byte
	state      *providerCooldownState
	generation uint64
	probe      bool
}

func newProviderCooldown(maximum time.Duration, provider string) *providerCooldown {
	return &providerCooldown{states: make(map[[32]byte]*providerCooldownState), base: 2 * time.Second,
		maximum: maximum, provider: provider, now: time.Now}
}

func providerQuotaKey(headers http.Header) ([32]byte, bool) {
	key := strings.TrimSpace(headers.Get("api-key"))
	if key == "" {
		parts := strings.Fields(headers.Get("Authorization"))
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			key = parts[1]
		}
	}
	return sha256.Sum256([]byte(key)), key != ""
}

func (g *providerCooldown) acquire(ctx context.Context, key [32]byte) (providerCooldownLease, error) {
	g.mu.Lock()
	s := g.states[key]
	if s == nil {
		g.mu.Unlock()
		return providerCooldownLease{gate: g, key: key}, nil
	}
	s.waiters++
	for {
		if err := ctx.Err(); err != nil {
			s.waiters--
			g.mu.Unlock()
			return providerCooldownLease{}, err
		}
		remaining := s.until.Sub(g.now())
		if !s.probe && remaining <= 0 {
			s.waiters--
			s.probe = true
			lease := providerCooldownLease{gate: g, key: key, state: s, generation: s.generation, probe: true}
			g.mu.Unlock()
			return lease, nil
		}
		changed := s.changed
		var timer *time.Timer
		var timeout <-chan time.Time
		if !s.probe && remaining > 0 {
			timer = time.NewTimer(remaining)
			timeout = timer.C
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timeout:
		}
		if timer != nil {
			timer.Stop()
		}
		g.mu.Lock()
	}
}

func (g *providerCooldown) signal(s *providerCooldownState) {
	close(s.changed)
	s.changed = make(chan struct{})
}

func retryableQuotaResponse(response *http.Response) bool {
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		return false
	}
	dimension := response.Header.Get("X-RateLimit-Dimension")
	return dimension != "daily_key_model" && dimension != "daily_key_total" &&
		!strings.EqualFold(response.Header.Get("X-Should-Retry"), "false")
}

func (lease providerCooldownLease) finish(response *http.Response, requestErr error, canceled bool) {
	g := lease.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.states[lease.key]
	now := g.now()
	if retryableQuotaResponse(response) {
		if s == nil {
			// Bound state held by untrusted callers. Expired idle buckets can be
			// reclaimed; overflow still receives the original upstream response.
			if len(g.states) >= 1024 {
				for key, old := range g.states {
					if !old.probe && old.waiters == 0 && now.Sub(old.until) > time.Minute {
						delete(g.states, key)
					}
				}
				if len(g.states) >= 1024 {
					return
				}
			}
			s = &providerCooldownState{nextDelay: min(g.base, g.maximum), changed: make(chan struct{})}
			g.states[lease.key] = s
		}
		delay := s.nextDelay
		s.nextDelay = min(s.nextDelay*2, g.maximum)
		// The exponential component is capped; a longer server minimum wins.
		delay = max(delay, providerRetryMinimum(response.Header, now))
		if until := now.Add(delay); until.After(s.until) {
			s.until = until
		}
		s.generation++
		if lease.probe {
			s.probe = false
		}
		g.signal(s)
		slog.Warn("codex: provider shared cooldown", "provider", g.provider, "delay", s.until.Sub(now), "waiting_requests", s.waiters)
		return
	}
	if s == nil || !lease.probe || s != lease.state {
		return // A response from before the 429 must not clear its cooldown.
	}
	s.probe = false
	if lease.generation == s.generation {
		if requestErr == nil && response != nil && response.StatusCode < 500 {
			s.nextDelay = min(g.base, g.maximum)
			s.until = now
			if s.waiters == 0 {
				delete(g.states, lease.key)
			}
		} else if !canceled {
			delay := s.nextDelay
			if response != nil {
				delay = max(delay, providerRetryMinimum(response.Header, now))
			}
			s.until = now.Add(delay)
			s.nextDelay = min(s.nextDelay*2, g.maximum)
		}
	}
	// During recovery, queued requests pass one at a time until the queue
	// drains, rather than all waking and hitting the same key together.
	g.signal(s)
}

func (g *providerCooldown) snapshot() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	waiters, probes := 0, 0
	var remaining time.Duration
	for _, s := range g.states {
		waiters += s.waiters
		if s.probe {
			probes++
		}
		remaining = max(remaining, s.until.Sub(g.now()))
	}
	return map[string]any{"keys": len(g.states), "waiting_requests": waiters, "probes": probes,
		"remaining_seconds": max(0, remaining.Seconds()), "backoff_max_seconds": g.maximum.Seconds()}
}

type providerCooldownTransport struct {
	base http.RoundTripper
	gate *providerCooldown
}

func (t *providerCooldownTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	key, authenticated := providerQuotaKey(request.Header)
	if !authenticated || request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
		return t.base.RoundTrip(request)
	}
	if request.Body != nil && request.GetBody == nil {
		// Oversized or encoded bodies bypass buffering. Keep their original
		// streaming/cancellation behavior rather than blocking an unread body.
		return t.base.RoundTrip(request)
	}
	lease, err := t.gate.acquire(request.Context(), key)
	if err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	response, err := t.base.RoundTrip(request)
	lease.finish(response, err, request.Context().Err() != nil)
	return response, err // Never replay a request or read/re-encode its response body.
}
