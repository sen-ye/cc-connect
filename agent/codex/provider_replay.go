package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

const (
	maxReplayRequestBytes = 32 << 20
	maxReplayErrorBytes   = 1 << 20
)

// reasoningReplayTransport repairs one explicitly rejected Responses request.
// Successful streams and failures after an HTTP 200 are never replayed: the
// model may already have produced output or requested a side effect.
type reasoningReplayTransport struct{ base http.RoundTripper }

func (t *reasoningReplayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	encoding := strings.TrimSpace(request.Header.Get("Content-Encoding"))
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") ||
		request.Body == nil || request.ContentLength > maxReplayRequestBytes ||
		(encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return t.base.RoundTrip(request)
	}
	// Capture while sending rather than delaying or rejecting the first request.
	// An oversized or incompletely consumed body is forwarded, without repair.
	captured := &replayRequestBody{ReadCloser: request.Body}
	outgoing := request.Clone(request.Context())
	outgoing.Body = captured
	response, err := t.base.RoundTrip(outgoing)
	if err != nil || response.StatusCode != http.StatusBadRequest {
		captured.discard()
		return response, err
	}
	body, complete := captured.snapshot(request.ContentLength)
	captured.discard()
	if !complete {
		return response, nil
	}
	originalBody := response.Body
	prefix, readErr := io.ReadAll(io.LimitReader(originalBody, maxReplayErrorBytes+1))
	response.Body = &replayResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), originalBody), Closer: originalBody}
	if readErr != nil || len(prefix) > maxReplayErrorBytes || !replayRefusalMatches(prefix, body) {
		return response, nil
	}
	repaired, changed := stripEncryptedReasoning(body)
	if changed == 0 {
		return response, nil
	}
	_ = originalBody.Close()
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	retry := outgoing.Clone(outgoing.Context())
	retry.Body = io.NopCloser(bytes.NewReader(repaired))
	retry.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(repaired)), nil }
	retry.ContentLength = int64(len(repaired))
	retry.TransferEncoding = nil
	retry.Header.Del("Content-Length")
	slog.Warn("codex: retrying rejected encrypted reasoning as summaries", "target_host", request.URL.Hostname(), "reasoning_items", changed)
	// Call the underlying transport directly: the second response is final,
	// regardless of status, and its SSE body remains streamed without buffering.
	return t.base.RoundTrip(retry)
}

// Maso's Python Responses client also recognizes this continuation identity
// error, but only when the request really contains encrypted reasoning.
func replayRefusalMatches(errorBody, requestBody []byte) bool {
	if bytes.Contains(errorBody, []byte("invalid_encrypted_content")) {
		return true
	}
	if !bytes.Contains(errorBody, []byte("Conflicting authenticated continuation identities")) {
		return false
	}
	var request struct {
		Input []struct {
			Type      string          `json:"type"`
			Encrypted json.RawMessage `json:"encrypted_content"`
		} `json:"input"`
	}
	if json.Unmarshal(requestBody, &request) != nil {
		return false
	}
	for _, item := range request.Input {
		if item.Type == "reasoning" && replayValuePresent(item.Encrypted) {
			return true
		}
	}
	return false
}

type replayResponseBody struct {
	io.Reader
	io.Closer
}

type replayRequestBody struct {
	io.ReadCloser
	mu      sync.Mutex
	buf     []byte
	eof     bool
	skipped bool
}

func (b *replayRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.skipped {
		if len(b.buf)+n > maxReplayRequestBytes {
			b.buf = nil
			b.skipped = true
		} else {
			b.buf = append(b.buf, p[:n]...)
		}
	}
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *replayRequestBody) snapshot(length int64) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipped || (!b.eof && (length < 0 || int64(len(b.buf)) != length)) {
		return nil, false
	}
	return append([]byte(nil), b.buf...), true
}

func (b *replayRequestBody) discard() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = nil
	b.skipped = true
}

// Keep readable history and tool call IDs. Match Maso's narrow compatibility
// rule: only reasoning items lose id/content/encrypted_content; an empty-summary
// item is removed. Compaction items and previous_response_id are not repaired.
// RawMessage preserves arbitrary numeric values and non-reasoning item fields.
func stripEncryptedReasoning(raw []byte) ([]byte, int) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return raw, 0
	}
	if _, chatCompletion := body["messages"]; chatCompletion {
		return raw, 0
	}
	input := bytes.TrimSpace(body["input"])
	if len(input) == 0 || input[0] != '[' {
		return raw, 0
	}
	var items []json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return raw, 0
	}
	kept := make([]json.RawMessage, 0, len(items))
	changed := 0
	for _, rawItem := range items {
		var item map[string]json.RawMessage
		var kind string
		if json.Unmarshal(rawItem, &item) != nil || json.Unmarshal(item["type"], &kind) != nil || kind != "reasoning" ||
			(!replayValuePresent(item["encrypted_content"]) && !replayValuePresent(item["content"])) {
			kept = append(kept, rawItem)
			continue
		}
		changed++
		if replayValuePresent(item["summary"]) {
			summary, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"reasoning"`), "summary": item["summary"]})
			if err != nil {
				return raw, 0
			}
			kept = append(kept, summary)
		}
	}
	if changed == 0 {
		return raw, 0
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return raw, 0
	}
	body["input"] = encoded
	encoded, err = json.Marshal(body)
	if err != nil {
		return raw, 0
	}
	return encoded, changed
}

func replayValuePresent(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return v != ""
	case []any:
		return len(v) != 0
	case map[string]any:
		return len(v) != 0
	case bool:
		return v
	case float64:
		return v != 0
	default:
		return false
	}
}
