package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type autoRetryPolicy struct {
	maxAttempts int
	maxWait     time.Duration
	baseDelay   time.Duration
	maxDelay    time.Duration
	jitter      bool
}

func parseAutoRetryPolicy(opts map[string]any) (autoRetryPolicy, error) {
	p := autoRetryPolicy{maxWait: 10 * time.Minute, baseDelay: 2 * time.Second, maxDelay: 2 * time.Minute, jitter: true}
	for _, setting := range []struct {
		name string
		max  int
	}{
		{"auto_retry_max_attempts", 20}, {"auto_retry_max_wait_seconds", 3600},
	} {
		value, exists := opts[setting.name]
		if !exists {
			continue
		}
		n, err := strconv.Atoi(fmt.Sprint(value))
		if err != nil || n < 0 || n > setting.max || (setting.name == "auto_retry_max_wait_seconds" && n == 0) {
			return p, fmt.Errorf("codex: invalid %s", setting.name)
		}
		if setting.name == "auto_retry_max_attempts" {
			p.maxAttempts = n
		} else {
			p.maxWait = time.Duration(n) * time.Second
		}
	}
	return p, nil
}

func (p autoRetryPolicy) addOptions(opts map[string]any) {
	if p.maxAttempts > 0 {
		opts["auto_retry_max_attempts"] = p.maxAttempts
		opts["auto_retry_max_wait_seconds"] = int(p.maxWait / time.Second)
	}
}

// Protected by appServerSession.stateMu. A generation invalidates waiting
// retries when a user supplies new input or stops the session.
type autoRetryState struct {
	policy     autoRetryPolicy
	generation uint64
	attempts   int
	waited     time.Duration
	waiting    bool
	cancel     context.CancelFunc
	retired    []string
}

func (r *autoRetryState) invalidate() {
	r.generation++
	r.waiting = false
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

func (r *autoRetryState) retire(id string) {
	if id == "" {
		return
	}
	r.retired = append(r.retired, id)
	if len(r.retired) > 32 {
		r.retired = append([]string(nil), r.retired[len(r.retired)-32:]...)
	}
}

func (r *autoRetryState) isRetired(id string) bool {
	for _, previous := range r.retired {
		if id != "" && id == previous {
			return true
		}
	}
	return false
}

type codexTurnFailure struct {
	Message string          `json:"message"`
	Info    json.RawMessage `json:"codexErrorInfo"`
}

func (f *codexTurnFailure) Error() string { return f.Message }

var transientStatusPattern = regexp.MustCompile(`(?i)(?:last status:|http(?: status)?[: ]|status(?: code)?[:= ])\s*(\d{3})\b`)

func retryableTurnFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"unauthorized", "invalid_api_key", "invalid api key", "permission denied", "bad_request", "bad request", "invalid_request", "context_window_exceeded", "context_length_exceeded", "context window exceeded", "usage_limit", "usage limit", "insufficient_quota", "daily quota", "credit balance", "spend limit", "cancelled", "canceled", "interrupted"} {
		if strings.Contains(message, marker) {
			return false
		}
	}
	var failure *codexTurnFailure
	if errors.As(err, &failure) && len(failure.Info) > 0 {
		var code string
		if json.Unmarshal(failure.Info, &code) == nil && code != "other" {
			switch code {
			case "rateLimitExceeded", "serverOverloaded", "internalServerError", "flexUnavailable":
				return true
			default:
				return false
			}
		}
		var variants map[string]struct {
			Status *int `json:"httpStatusCode"`
		}
		if json.Unmarshal(failure.Info, &variants) == nil {
			if len(variants) > 1 {
				return false
			}
			for code, detail := range variants {
				switch code {
				case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts":
					if detail.Status != nil {
						return retryableHTTPStatus(*detail.Status) || (code == "responseStreamDisconnected" && *detail.Status == 200)
					}
					return true
				default:
					return false
				}
			}
		}
	}
	if match := transientStatusPattern.FindStringSubmatch(message); len(match) > 1 {
		status, _ := strconv.Atoi(match[1])
		return retryableHTTPStatus(status)
	}
	for _, marker := range []string{"rate limit exceeded", "too many requests", "stream disconnected", "stream closed before response.completed", "connection reset by peer", "connection timed out", "error decoding response body", "server overloaded", "temporarily unavailable"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func retryableHTTPStatus(status int) bool {
	return status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504 || status == 529
}

const autoRetryContinuation = "Continue the previous unfinished user task from the progress already saved in this same conversation. The last turn ended because of a temporary model-service failure. Review the recorded tool results and current state, preserve completed work, and do not repeat completed actions or recreate the original request. If an action's outcome is uncertain, verify its state before trying it again. Do not switch model providers."

// Only authoritative failed-turn notifications reach this method. HTTP and
// in-stream retries still belong to Codex, which has already stopped this turn.
func (s *appServerSession) scheduleAutoRetry(err error) bool {
	if !retryableTurnFailure(err) || s.hasPendingApprovals() {
		return false
	}
	hint, hardLimit := s.providerProxy.autoRetryHint(time.Now())
	s.stateMu.Lock()
	r := &s.autoRetry
	if hardLimit || !s.alive.Load() || s.currentTurn == "" || r.waiting || r.policy.maxAttempts == 0 || r.attempts >= r.policy.maxAttempts || s.CurrentSessionID() == "" {
		s.stateMu.Unlock()
		return false
	}
	delay := r.policy.baseDelay
	for i := 0; i < r.attempts && delay < r.policy.maxDelay; i++ {
		delay = min(delay*2, r.policy.maxDelay)
	}
	delay = max(delay, hint)
	if r.policy.jitter && delay > 0 {
		delay += time.Duration(rand.Int64N(int64(delay/10) + 1))
	}
	if delay > r.policy.maxWait-r.waited {
		s.stateMu.Unlock()
		return false // Never shorten a server's minimum delay to fit our budget.
	}
	if r.generation == 0 {
		r.generation = 1
	}
	r.attempts++
	r.waited += delay
	r.waiting = true
	r.retire(s.currentTurn)
	s.turnRevision++
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	r.cancel = cancel
	generation, attempt, limit := r.generation, r.attempts, r.policy.maxAttempts
	threadID := s.CurrentSessionID()
	s.pendingMsgs = s.pendingMsgs[:0]
	s.wg.Add(1) // Close takes stateMu before starting its Wait.
	s.stateMu.Unlock()
	// Discard the failed draft in the UI, retaining completed tool steps and
	// the native history. The continuation may produce a different final answer.
	s.emit(core.Event{Type: core.EventTurnSuperseded, SessionID: threadID, Synthetic: true})
	s.emit(core.Event{Type: core.EventRetry, SessionID: threadID, Retry: &core.AgentRetry{Attempt: attempt, MaxAttempts: limit, DelaySeconds: int((delay + time.Second - 1) / time.Second)}})
	slog.Warn("codex: scheduling same-session recovery", "session_id", threadID, "provider", s.modelProvider, "attempt", attempt, "max_attempts", limit, "delay", delay)
	go s.runAutoRetry(ctx, generation, attempt, threadID, delay)
	return true
}

func (s *appServerSession) runAutoRetry(ctx context.Context, generation uint64, attempt int, threadID string, delay time.Duration) {
	defer s.wg.Done()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return
	}
	if s.CurrentSessionID() != threadID {
		s.stopAutoRetry(generation, attempt, fmt.Errorf("codex: session ID changed during recovery; refusing to continue"))
		return
	}
	if err := s.sendTurn(autoRetryContinuation, "", nil, nil, generation); err != nil {
		// An RPC failure is ambiguous: do not resend a possibly accepted turn.
		s.stopAutoRetry(generation, attempt, fmt.Errorf("codex: automatic continuation could not start: %w", err))
	}
}

func (s *appServerSession) stopAutoRetry(generation uint64, attempt int, err error) {
	s.stateMu.Lock()
	current := s.autoRetry.generation == generation && s.autoRetry.attempts == attempt && s.autoRetry.waiting && s.alive.Load()
	if current {
		s.autoRetry.invalidate()
	}
	s.stateMu.Unlock()
	if current {
		s.failTurn(err)
	}
}

func (s *appServerSession) recoveryFailure(err error) error {
	s.stateMu.Lock()
	attempts, enabled := s.autoRetry.attempts, s.autoRetry.policy.maxAttempts > 0
	s.stateMu.Unlock()
	if !enabled {
		return err
	}
	kind := core.FailureProvider
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unauthorized") || strings.Contains(message, "invalid_api_key") || strings.Contains(message, "permission denied") {
		kind = core.FailureAuth
	} else if strings.Contains(message, "429") || strings.Contains(message, "rate limit") {
		kind = core.FailureRateLimit
	}
	failure := &core.AgentFailure{Kind: kind, Detail: err.Error(), Model: s.GetModel(), RetryCountKnown: true, RetryCount: attempts}
	if delay, _ := s.providerProxy.autoRetryHint(time.Now()); delay > 0 {
		failure.RetryAfter = fmt.Sprintf("%d s", (delay+time.Second-1)/time.Second)
	}
	return failure
}
