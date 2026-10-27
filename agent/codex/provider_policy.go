package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Like Maso's CLI proxy, fill an absent output budget on Responses requests.
// Do not replace an explicit budget or touch compaction/catalog/ChatCompletions.
// This only bounds requested output; it is not a TPM quota or a billing limit.
type providerOutputLimitTransport struct {
	base  http.RoundTripper
	limit int
}

func (t *providerOutputLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	encoding := strings.TrimSpace(request.Header.Get("Content-Encoding"))
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") ||
		request.Body == nil || request.ContentLength > maxReplayRequestBytes ||
		(encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return t.base.RoundTrip(request)
	}
	// Only opt-in requests are buffered. Bound memory and preserve an oversized
	// stream by concatenating the consumed prefix with the unread remainder.
	prefix, err := io.ReadAll(io.LimitReader(request.Body, maxReplayRequestBytes+1))
	if err != nil {
		_ = request.Body.Close()
		return nil, err
	}
	outgoing := request.Clone(request.Context())
	outgoing.Body = &replayResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), request.Body), Closer: request.Body}
	if len(prefix) > maxReplayRequestBytes {
		return t.base.RoundTrip(outgoing)
	}
	rewritten, changed := injectProviderOutputLimit(prefix, t.limit)
	if !changed {
		return t.base.RoundTrip(outgoing)
	}
	_ = request.Body.Close()
	outgoing.Body = io.NopCloser(bytes.NewReader(rewritten))
	outgoing.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
	outgoing.ContentLength = int64(len(rewritten))
	outgoing.TransferEncoding = nil
	outgoing.Header.Del("Content-Length")
	return t.base.RoundTrip(outgoing)
}

func injectProviderOutputLimit(raw []byte, limit int) ([]byte, bool) {
	var body map[string]json.RawMessage
	if limit <= 0 || json.Unmarshal(raw, &body) != nil || body == nil {
		return raw, false
	}
	if _, exists := body["messages"]; exists {
		return raw, false
	}
	input := bytes.TrimSpace(body["input"])
	if len(input) == 0 || input[0] != '[' {
		return raw, false
	}
	// Missing/null/zero are unset. Preserve every explicit nonzero value, even
	// an invalid one: validation of user-supplied values belongs to the upstream.
	existing := bytes.TrimSpace(body["max_output_tokens"])
	if len(existing) != 0 && !bytes.Equal(existing, []byte("null")) && !bytes.Equal(existing, []byte("0")) {
		return raw, false
	}
	body["max_output_tokens"] = json.RawMessage(strconv.Itoa(limit))
	rewritten, err := json.Marshal(body)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

var providerLogIDPattern = regexp.MustCompile(`^[0-9]{14}[A-Z0-9]{16,40}$`)

// Inspect only headers, never consume a streaming error body or add another
// retry loop around Codex. Keep rate-limit headers intact for its retry owner.
func logProviderHTTPFailure(response *http.Response, provider, host string) {
	if response.StatusCode < 400 {
		return
	}
	attrs := []any{"provider", provider, "target_host", host, "status", response.StatusCode}
	if response.StatusCode == http.StatusTooManyRequests {
		dimension := strings.TrimSpace(response.Header.Get("X-RateLimit-Dimension"))
		switch dimension {
		case "rpm_key", "tpm_key", "daily_key_model", "daily_key_total":
		default:
			dimension = "unknown"
		}
		attrs = append(attrs, "rate_limit_dimension", dimension)
		if delay, ok := providerRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
			attrs = append(attrs, "retry_after_seconds", delay.Seconds())
		}
	}
	if id := response.Header.Get("X-Tt-Logid"); providerLogIDPattern.MatchString(id) {
		attrs = append(attrs, "request_id", id)
	}
	slog.Warn("codex: provider HTTP error", attrs...)
}

func providerRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 && value <= 86400 {
		return time.Duration(value * float64(time.Second)), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		delay := at.Sub(now)
		if delay >= 0 && delay <= 24*time.Hour {
			return delay, true
		}
	}
	return 0, false
}
