package cursor

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var (
	cursorANSI       = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	cursorSecret     = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]+|bearer\s+[a-z0-9._~+/-]+=*)`)
	cursorCredential = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|access[_-]?token|auth[_-]?token|authorization|app[_-]?secret|password)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	cursorURLAuth    = regexp.MustCompile(`(https?://)[^\s/@]+:[^\s/@]+@`)
)

func safeCursorDetail(text string, env []string) string {
	text = cursorANSI.ReplaceAllString(text, "")
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if ok && len(value) >= 8 && (strings.Contains(key, "KEY") || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD")) {
			text = core.RedactToken(text, value)
		}
	}
	text = cursorSecret.ReplaceAllString(text, "<redacted>")
	text = cursorCredential.ReplaceAllString(text, "${1}<redacted>")
	text = cursorURLAuth.ReplaceAllString(text, "${1}<redacted>@")
	return truncateStr(strings.TrimSpace(text), 1200)
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func failureKind(code, detail, status string) core.AgentFailureKind {
	text := strings.ToLower(code + " " + detail)
	if strings.Contains(text, "without saving progress") || strings.Contains(text, "resume attempts made no progress") {
		return core.FailureNoProgress
	}
	// The provider's HTTP status is more specific than a generic transport
	// code or a phrase embedded in the error's description.
	switch status {
	case "429":
		return core.FailureRateLimit
	case "401", "403":
		return core.FailureAuth
	case "":
	default:
		return core.FailureProvider
	}
	code = strings.ToLower(code)
	switch {
	case strings.Contains(code, "rate_limited"):
		return core.FailureRateLimit
	case strings.Contains(code, "unauthenticated"), strings.Contains(code, "unauthorized"):
		return core.FailureAuth
	case strings.Contains(code, "provider_error"):
		return core.FailureProvider
	case strings.Contains(text, "rate_limited"), strings.Contains(text, "rate limit"), strings.Contains(text, "http 429"):
		return core.FailureRateLimit
	case strings.Contains(text, "unauthenticated"), strings.Contains(text, "unauthorized"), strings.Contains(text, "invalid api key"):
		return core.FailureAuth
	case strings.Contains(text, "provider_error"), strings.Contains(text, "provider error"):
		return core.FailureProvider
	case strings.Contains(text, "failed to reach"), strings.Contains(text, "econnrefused"), strings.Contains(text, "enotfound"), strings.Contains(text, "failed to connect"):
		return core.FailureConnection
	case strings.Contains(text, "stall_detector"), strings.Contains(text, "connection stalled"), strings.Contains(text, "connection interrupted"), strings.Contains(text, "deadline_exceeded"):
		return core.FailureStalled
	}
	// ResourceExhausted alone is not HTTP 429; Canceled alone is not a user stop.
	return ""
}

// cursorFailure reads only known error envelopes, never user/assistant/tool text.
func cursorFailure(raw map[string]any) *core.AgentFailure {
	f := &core.AgentFailure{}
	code := ""
	nodes := []map[string]any{raw, object(raw["error"]), object(raw["displayInfo"])}
	if inner := object(raw["error"]); inner != nil {
		nodes = append(nodes, object(inner["displayInfo"]))
	}
	for _, node := range append([]map[string]any(nil), nodes...) {
		details := object(node["errorDetails"])
		nodes = append(nodes, details, object(details["details"]), object(node["details"]))
	}
	for _, node := range nodes {
		for _, key := range []string{"result", "message", "detail"} {
			if value := stringField(node, key); value != "" {
				f.Detail = value
			}
		}
		for _, key := range []string{"error_code", "inferenceRequestErrorType", "code", "error"} {
			if value := stringField(node, key); value != "" {
				code += " " + value
				if key == "error" && f.Detail == "" {
					f.Detail = value
				}
			}
		}
		for _, key := range []string{"request_id", "requestId"} {
			if value := stringField(node, key); value != "" {
				f.RequestID = value
			}
		}
		if value, ok := node["isRetryable"].(bool); ok {
			f.Retryable = &value
		}
		for _, values := range []map[string]any{node, object(node["additionalInfo"])} {
			for _, key := range []string{"providerStatusCode", "status_code", "http_status"} {
				if n := number(values[key]); n >= 100 && n <= 599 {
					f.ProviderStatus = strconv.Itoa(n)
				}
			}
			for _, key := range []string{"retryAfter", "retry_after", "Retry-After"} {
				if value := values[key]; value != nil {
					f.RetryAfter = fmt.Sprint(value)
				}
			}
		}
	}
	if f.Detail == "" {
		if items, ok := raw["errors"].([]any); ok {
			var messages []string
			for _, item := range items {
				if text, ok := item.(string); ok {
					messages = append(messages, text)
				}
			}
			f.Detail = strings.Join(messages, "; ")
		}
	}
	f.Kind = failureKind(code, f.Detail, f.ProviderStatus)
	return f
}

func mergeCursorFailure(primary, extra *core.AgentFailure) *core.AgentFailure {
	if primary == nil {
		primary = &core.AgentFailure{}
	}
	result := *primary
	if extra == nil {
		return &result
	}
	if result.Kind == "" {
		result.Kind = extra.Kind
	}
	if result.Detail == "" {
		result.Detail = extra.Detail
	}
	if result.ProviderStatus == "" {
		result.ProviderStatus = extra.ProviderStatus
	}
	if result.RequestID == "" {
		result.RequestID = extra.RequestID
	}
	if result.Retryable == nil {
		result.Retryable = extra.Retryable
	}
	if result.RetryAfter == "" {
		result.RetryAfter = extra.RetryAfter
	}
	result.RetryCountKnown = result.RetryCountKnown || extra.RetryCountKnown
	result.RetryCount = max(result.RetryCount, extra.RetryCount)
	return &result
}

func failedCursorResult(raw map[string]any) bool {
	failed, _ := raw["is_error"].(bool)
	subtype := strings.ToLower(stringField(raw, "subtype"))
	return failed || strings.HasPrefix(subtype, "error") || subtype == "failed" || subtype == "failure"
}
