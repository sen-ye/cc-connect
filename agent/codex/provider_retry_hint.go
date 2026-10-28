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
	if delay := providerRetryMinimum(response.Header, now); delay > 0 {
		hint.until = now.Add(delay)
	}
	dimension := response.Header.Get("X-RateLimit-Dimension")
	hint.hard = dimension == "daily_key_model" || dimension == "daily_key_total" || strings.EqualFold(response.Header.Get("X-Should-Retry"), "false")
	p.retryMu.Lock()
	p.retryHint = hint
	p.retryMu.Unlock()
}

func providerRetryMinimum(headers http.Header, now time.Time) time.Duration {
	delay, _ := retryAfterMinimum(headers.Get("Retry-After"), now)
	if ms, err := strconv.ParseFloat(headers.Get("Retry-After-Ms"), 64); err == nil && ms > 0 && !math.IsInf(ms, 0) && !math.IsNaN(ms) {
		milliseconds := time.Duration(min(ms, float64(math.MaxInt64)/float64(time.Millisecond))) * time.Millisecond
		delay = max(delay, milliseconds)
	}
	return delay
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

// Long valid server delays remain long. Recovery wait limits reject them;
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
