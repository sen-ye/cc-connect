package codex

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type providerRetryHint struct {
	observed time.Time
	until    time.Time
	hard     bool
}

func (p *providerProxy) rememberRetryResponse(response *http.Response) {
	if response.StatusCode != 429 && response.StatusCode != 503 && response.StatusCode != 529 {
		return
	}
	now := time.Now()
	hint := providerRetryHint{observed: now}
	if delay, ok := retryAfterMinimum(response.Header.Get("Retry-After"), now); ok {
		hint.until = now.Add(delay)
	}
	if ms, err := strconv.ParseFloat(response.Header.Get("Retry-After-Ms"), 64); err == nil && ms > 0 && !math.IsInf(ms, 0) {
		delay := time.Duration(min(ms, float64(math.MaxInt64)/float64(time.Millisecond))) * time.Millisecond
		if until := now.Add(delay); until.After(hint.until) {
			hint.until = until
		}
	}
	dimension := response.Header.Get("X-RateLimit-Dimension")
	hint.hard = dimension == "daily_key_model" || dimension == "daily_key_total" || strings.EqualFold(response.Header.Get("X-Should-Retry"), "false")
	p.retryMu.Lock()
	p.retryHint = hint
	p.retryMu.Unlock()
}

func (p *providerProxy) autoRetryHint(now time.Time) (time.Duration, bool) {
	if p == nil {
		return 0, false
	}
	p.retryMu.Lock()
	hint := p.retryHint
	p.retryMu.Unlock()
	if now.Sub(hint.observed) > time.Minute && !hint.until.After(now) {
		return 0, false
	}
	return max(0, hint.until.Sub(now)), hint.hard
}

// Long valid server delays remain long. The recovery budget rejects them;
// treating them as missing would accidentally retry earlier than requested.
func retryAfterMinimum(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value) {
		if value >= float64(math.MaxInt64)/float64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(value * float64(time.Second)), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		return max(0, at.Sub(now)), true
	}
	return 0, false
}
