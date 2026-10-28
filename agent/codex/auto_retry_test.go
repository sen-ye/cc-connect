package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type recoveryRPC struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	sentAt time.Time
}

type recoveryWriter struct {
	requests chan recoveryRPC
	done     chan struct{}
	once     sync.Once
}

func (w *recoveryWriter) Write(raw []byte) (int, error) {
	var request recoveryRPC
	if err := json.Unmarshal(raw, &request); err != nil {
		return 0, err
	}
	request.sentAt = time.Now()
	select {
	case w.requests <- request:
		return len(raw), nil
	case <-w.done:
		return 0, io.EOF
	}
}
func (w *recoveryWriter) Close() error { w.once.Do(func() { close(w.done) }); return nil }

func newRecoveryFixture(t *testing.T) (*appServerSession, *recoveryWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &recoveryWriter{requests: make(chan recoveryRPC, 16), done: make(chan struct{})}
	s := &appServerSession{ctx: ctx, cancel: cancel, stdin: w, events: make(chan core.Event, 64),
		pending: make(map[int64]chan rpcResponseEnvelope), model: "gpt-6-astra", effort: "xhigh", modelProvider: "Seed", mode: "suggest",
		autoRetry: autoRetryState{policy: autoRetryPolicy{maxAttempts: 2, maxWait: time.Second, baseDelay: 15 * time.Millisecond, maxDelay: 100 * time.Millisecond}}}
	s.threadID.Store("saved-seed-thread")
	s.alive.Store(true)
	t.Cleanup(func() { _ = s.Close() })
	return s, w
}

func takeRecoveryRPC(t *testing.T, w *recoveryWriter) recoveryRPC {
	t.Helper()
	select {
	case r := <-w.requests:
		return r
	case <-time.After(time.Second):
		t.Fatal("no recovery RPC")
		return recoveryRPC{}
	}
}

func recoveryNotification(s *appServerSession, method, id, status string, err error) {
	turn := map[string]any{"id": id, "status": status}
	if err != nil {
		turn["error"] = map[string]any{"message": err.Error(), "codexErrorInfo": map[string]any{"responseTooManyFailedAttempts": map[string]int{"httpStatusCode": 429}}}
	}
	data, _ := json.Marshal(map[string]any{"threadId": s.CurrentSessionID(), "turn": turn})
	s.handleNotification(method, data)
}

func acceptRecoveryRPC(s *appServerSession, r recoveryRPC, id string) {
	recoveryNotification(s, "turn/started", id, "inProgress", nil)
	raw, _ := json.Marshal(map[string]any{"turn": map[string]string{"id": id}})
	s.handleResponse(rpcResponseEnvelope{ID: r.ID, Result: raw})
}

func startRecoveryUserTurn(t *testing.T, s *appServerSession, w *recoveryWriter, prompt, id string) recoveryRPC {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Send(prompt, "user-message", nil, nil) }()
	r := takeRecoveryRPC(t, w)
	acceptRecoveryRPC(s, r, id)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return r
}

func takeRecoveryEvent(t *testing.T, s *appServerSession, want core.EventType) core.Event {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case e := <-s.Events():
			if e.Type == want {
				return e
			}
			if e.Type == core.EventError || e.Type == core.EventResult {
				t.Fatalf("premature terminal event: %s %v", e.Type, e.Error)
			}
		case <-deadline.C:
			t.Fatalf("missing %s event", want)
		}
	}
}

func assertNoRecoveryRPC(t *testing.T, w *recoveryWriter, duration time.Duration) {
	t.Helper()
	select {
	case r := <-w.requests:
		t.Fatalf("unexpected extra RPC: %s", r.Method)
	case <-time.After(duration):
	}
}

func TestAutoRetry_ContinuesSameThreadOnceAfterNativeRetriesEnd(t *testing.T) {
	s, w := newRecoveryFixture(t)
	startRecoveryUserTurn(t, s, w, "ORIGINAL_TASK_ONCE", "original-turn")
	s.handleNotification("error", []byte(`{"threadId":"saved-seed-thread","turnId":"original-turn","willRetry":true,"error":{"message":"429","codexErrorInfo":"rateLimitExceeded"}}`))
	assertNoRecoveryRPC(t, w, 20*time.Millisecond)
	s.handleNotification("thread/status/changed", []byte(`{"threadId":"saved-seed-thread","status":{"type":"idle"}}`))
	s.handleNotification("error", []byte(`{"threadId":"saved-seed-thread","turnId":"original-turn","willRetry":false,"error":{"message":"429","codexErrorInfo":"rateLimitExceeded"}}`))
	recoveryNotification(s, "turn/completed", "original-turn", "failed", errors.New("exceeded retry limit, last status: 429 Too Many Requests"))
	e := takeRecoveryEvent(t, s, core.EventRetry)
	if e.Retry.Attempt != 1 || e.Retry.MaxAttempts != 2 || e.Retry.DelaySeconds < 1 {
		t.Fatalf("missing bounded recovery progress: %+v", e.Retry)
	}
	// Duplicated terminal/start/idle notifications from the failed turn must
	// neither enqueue another retry nor finalize the user-visible turn.
	recoveryNotification(s, "turn/completed", "original-turn", "failed", errors.New("429"))
	recoveryNotification(s, "turn/started", "original-turn", "inProgress", nil)
	s.handleNotification("thread/status/changed", []byte(`{"threadId":"saved-seed-thread","status":{"type":"idle"}}`))
	r := takeRecoveryRPC(t, w)
	if r.Method != "turn/start" || r.Params["threadId"] != "saved-seed-thread" || r.Params["model"] != "gpt-6-astra" || r.Params["effort"] != "xhigh" {
		t.Fatalf("recovery changed thread or settings: %#v", r.Params)
	}
	input := r.Params["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["text"] != autoRetryContinuation || strings.Contains(fmt.Sprint(input), "ORIGINAL_TASK_ONCE") {
		t.Fatal("recovery duplicated the original user request or attachments")
	}
	acceptRecoveryRPC(s, r, "recovery-turn")
	recoveryNotification(s, "turn/completed", "original-turn", "failed", errors.New("429"))
	recoveryNotification(s, "turn/completed", "recovery-turn", "completed", nil)
	result := takeRecoveryEvent(t, s, core.EventResult)
	if !result.Done || result.SessionID != "saved-seed-thread" || s.modelProvider != "Seed" {
		t.Fatal("recovery lost session/provider identity")
	}
	assertNoRecoveryRPC(t, w, 40*time.Millisecond)
}

func TestAutoRetry_StopsAtLimitAndResetsForNextUserMessage(t *testing.T) {
	s, w := newRecoveryFixture(t)
	startRecoveryUserTurn(t, s, w, "task", "t0")
	for i := 0; i < 2; i++ {
		recoveryNotification(s, "turn/completed", fmt.Sprint("t", i), "failed", errors.New("last status: 429"))
		if got := takeRecoveryEvent(t, s, core.EventRetry); got.Retry.Attempt != i+1 {
			t.Fatal("attempt counter restarted inside recovery")
		}
		acceptRecoveryRPC(s, takeRecoveryRPC(t, w), fmt.Sprint("t", i+1))
	}
	recoveryNotification(s, "turn/completed", "t2", "failed", errors.New("last status: 429"))
	final := takeRecoveryEvent(t, s, core.EventError)
	var failure *core.AgentFailure
	if !errors.As(final.Error, &failure) || failure.RetryCount != 2 || failure.Kind != core.FailureRateLimit {
		t.Fatalf("lost exhaustion details: %v", final.Error)
	}
	assertNoRecoveryRPC(t, w, 40*time.Millisecond)
	startRecoveryUserTurn(t, s, w, "new user task", "t3")
	recoveryNotification(s, "turn/completed", "t3", "failed", errors.New("last status: 429"))
	if got := takeRecoveryEvent(t, s, core.EventRetry); got.Retry.Attempt != 1 {
		t.Fatal("new user task inherited exhausted retry budget")
	}
}

func TestAutoRetry_LateRPCFailureDoesNotCancelNextAttempt(t *testing.T) {
	s, w := newRecoveryFixture(t)
	startRecoveryUserTurn(t, s, w, "task", "t0")
	recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
	takeRecoveryEvent(t, s, core.EventRetry)
	first := takeRecoveryRPC(t, w)
	// The turn's failed notification may precede the turn/start RPC response.
	recoveryNotification(s, "turn/started", "t1", "inProgress", nil)
	recoveryNotification(s, "turn/completed", "t1", "failed", errors.New("last status: 429"))
	if e := takeRecoveryEvent(t, s, core.EventRetry); e.Retry.Attempt != 2 {
		t.Fatal("second recovery not scheduled")
	}
	s.handleResponse(rpcResponseEnvelope{ID: first.ID, Error: &rpcError{Message: "late response for the previous failed turn"}})
	second := takeRecoveryRPC(t, w)
	acceptRecoveryRPC(s, second, "t2")
	recoveryNotification(s, "turn/completed", "t2", "completed", nil)
	takeRecoveryEvent(t, s, core.EventResult)
}

func recoveryItemNotification(s *appServerSession, threadID, turnID string, item map[string]any) {
	data, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
	s.handleNotification("item/completed", data)
}

func TestAutoRetry_ConfirmedProgressResetsFailureStreakAndWaitBudget(t *testing.T) {
	for _, item := range []map[string]any{
		{"type": "agentMessage", "text": "Completed the first step."},
		{"type": "commandExecution", "status": "completed", "exitCode": 0},
		{"type": "fileChange", "status": "completed"},
		{"type": "mcpToolCall", "status": "completed", "result": map[string]any{"ok": true}},
		{"type": "dynamicToolCall", "status": "completed", "success": true},
		{"type": "webSearch", "query": "recovery"},
	} {
		t.Run(item["type"].(string), func(t *testing.T) {
			s, w := newRecoveryFixture(t)
			startRecoveryUserTurn(t, s, w, "task", "t0")
			for i := 0; i < 2; i++ {
				recoveryNotification(s, "turn/completed", fmt.Sprint("t", i), "failed", errors.New("last status: 429"))
				takeRecoveryEvent(t, s, core.EventRetry)
				acceptRecoveryRPC(s, takeRecoveryRPC(t, w), fmt.Sprint("t", i+1))
			}
			recoveryItemNotification(s, s.CurrentSessionID(), "t2", item)
			s.stateMu.Lock()
			attempts, waited, active := s.autoRetry.attempts, s.autoRetry.waited, s.currentTurn
			s.stateMu.Unlock()
			if attempts != 0 || waited != 0 || active != "t2" {
				t.Fatalf("progress did not reset recovery while preserving the turn: attempts=%d waited=%s turn=%s", attempts, waited, active)
			}
			recoveryNotification(s, "turn/completed", "t2", "failed", errors.New("last status: 429"))
			if event := takeRecoveryEvent(t, s, core.EventRetry); event.Retry.Attempt != 1 {
				t.Fatalf("new failure did not restart at attempt 1: %+v", event.Retry)
			}
			s.stateMu.Lock()
			waited = s.autoRetry.waited
			s.stateMu.Unlock()
			if waited != 15*time.Millisecond {
				t.Fatalf("backoff did not restart at the base delay: %s", waited)
			}
			acceptRecoveryRPC(s, takeRecoveryRPC(t, w), "t3")
			recoveryNotification(s, "turn/completed", "t3", "completed", nil)
			takeRecoveryEvent(t, s, core.EventResult)
			s.stateMu.Lock()
			attempts, waited = s.autoRetry.attempts, s.autoRetry.waited
			s.stateMu.Unlock()
			if attempts != 0 || waited != 0 {
				t.Fatal("successful turn retained a recovery failure streak")
			}
		})
	}
}

func TestAutoRetry_UnconfirmedOrForeignProgressDoesNotResetFailureStreak(t *testing.T) {
	for _, tc := range []struct {
		name, thread, turn string
		item               map[string]any
	}{
		{"child_thread", "child", "t1", map[string]any{"type": "agentMessage", "text": "done"}},
		{"retired_turn", "saved-seed-thread", "t0", map[string]any{"type": "agentMessage", "text": "done"}},
		{"unscoped_thread", "", "t1", map[string]any{"type": "agentMessage", "text": "done"}},
		{"unscoped_turn", "saved-seed-thread", "", map[string]any{"type": "agentMessage", "text": "done"}},
		{"empty_message", "saved-seed-thread", "t1", map[string]any{"type": "agentMessage", "text": " "}},
		{"reasoning_only", "saved-seed-thread", "t1", map[string]any{"type": "reasoning", "text": "thinking"}},
		{"echoed_input", "saved-seed-thread", "t1", map[string]any{"type": "userMessage", "text": "continue"}},
		{"failed_tool", "saved-seed-thread", "t1", map[string]any{"type": "commandExecution", "status": "failed"}},
		{"failed_command", "saved-seed-thread", "t1", map[string]any{"type": "commandExecution", "status": "completed", "exitCode": 1}},
		{"incomplete_tool", "saved-seed-thread", "t1", map[string]any{"type": "commandExecution", "status": "inProgress"}},
		{"missing_status", "saved-seed-thread", "t1", map[string]any{"type": "mcpToolCall"}},
		{"mcp_error", "saved-seed-thread", "t1", map[string]any{"type": "mcpToolCall", "status": "completed", "error": map[string]any{"message": "failed"}}},
		{"dynamic_error", "saved-seed-thread", "t1", map[string]any{"type": "dynamicToolCall", "status": "completed", "success": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := newRecoveryFixture(t)
			startRecoveryUserTurn(t, s, w, "task", "t0")
			recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
			takeRecoveryEvent(t, s, core.EventRetry)
			acceptRecoveryRPC(s, takeRecoveryRPC(t, w), "t1")
			recoveryItemNotification(s, tc.thread, tc.turn, tc.item)
			s.stateMu.Lock()
			attempts, waited := s.autoRetry.attempts, s.autoRetry.waited
			s.stateMu.Unlock()
			if attempts != 1 || waited != 15*time.Millisecond {
				t.Fatalf("unconfirmed progress reset the budget: attempts=%d waited=%s", attempts, waited)
			}
		})
	}
}

func TestAutoRetry_LateRPCFailureCannotCancelRestartedFailureStreak(t *testing.T) {
	s, w := newRecoveryFixture(t)
	startRecoveryUserTurn(t, s, w, "task", "t0")
	recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
	takeRecoveryEvent(t, s, core.EventRetry)
	first := takeRecoveryRPC(t, w)
	recoveryNotification(s, "turn/started", "t1", "inProgress", nil)
	recoveryItemNotification(s, s.CurrentSessionID(), "t1", map[string]any{"type": "agentMessage", "text": "saved progress"})
	recoveryNotification(s, "turn/completed", "t1", "failed", errors.New("last status: 429"))
	if event := takeRecoveryEvent(t, s, core.EventRetry); event.Retry.Attempt != 1 {
		t.Fatalf("new failure did not restart at attempt 1: %+v", event.Retry)
	}
	s.handleResponse(rpcResponseEnvelope{ID: first.ID, Error: &rpcError{Message: "late start failure from the previous streak"}})
	acceptRecoveryRPC(s, takeRecoveryRPC(t, w), "t2")
	recoveryNotification(s, "turn/completed", "t2", "completed", nil)
	takeRecoveryEvent(t, s, core.EventResult)
}

func TestAutoRetry_CumulativeWaitBudgetAndAmbiguousStartFailure(t *testing.T) {
	t.Run("wait_budget", func(t *testing.T) {
		s, w := newRecoveryFixture(t)
		s.autoRetry.policy.maxAttempts = 10
		s.autoRetry.policy.maxWait = 25 * time.Millisecond
		startRecoveryUserTurn(t, s, w, "task", "t0")
		recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
		takeRecoveryEvent(t, s, core.EventRetry)
		acceptRecoveryRPC(s, takeRecoveryRPC(t, w), "t1")
		recoveryNotification(s, "turn/completed", "t1", "failed", errors.New("last status: 429"))
		event := takeRecoveryEvent(t, s, core.EventError)
		var failure *core.AgentFailure
		if !errors.As(event.Error, &failure) || failure.RetryCount != 1 {
			t.Fatal("cumulative wait budget did not stop recovery")
		}
		assertNoRecoveryRPC(t, w, 40*time.Millisecond)
	})
	t.Run("ambiguous_start", func(t *testing.T) {
		s, w := newRecoveryFixture(t)
		startRecoveryUserTurn(t, s, w, "task", "t0")
		recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
		takeRecoveryEvent(t, s, core.EventRetry)
		rpc := takeRecoveryRPC(t, w)
		s.handleResponse(rpcResponseEnvelope{ID: rpc.ID, Error: &rpcError{Message: "unknown start outcome"}})
		takeRecoveryEvent(t, s, core.EventError)
		assertNoRecoveryRPC(t, w, 40*time.Millisecond)
	})
}

func TestAutoRetry_BackoffNeverExceedsTenSeconds(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprint("jitter=", jitter), func(t *testing.T) {
			s, _ := newRecoveryFixture(t)
			policy, err := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 20, "auto_retry_max_wait_seconds": 3600})
			if err != nil {
				t.Fatal(err)
			}
			policy.jitter = jitter
			s.autoRetry.policy = policy
			s.currentTurn = "failed-turn"
			for _, seconds := range []int{2, 4, 8, 10, 10, 10, 10, 10} {
				delay := scheduledRecoveryDelay(t, s)
				minimum := time.Duration(seconds) * time.Second
				maximum := minimum
				if jitter {
					maximum = min(minimum+minimum/10, 10*time.Second)
				}
				if delay < minimum || delay > maximum {
					t.Fatalf("delay = %s, want [%s, %s]", delay, minimum, maximum)
				}
			}
		})
	}
}

// Inspect the scheduled delay and cancel its timer, so even the full hour's
// recovery budget is exercised without waiting or sending continuation RPCs.
func scheduledRecoveryDelay(t *testing.T, s *appServerSession) time.Duration {
	t.Helper()
	s.stateMu.Lock()
	before := s.autoRetry.waited
	s.stateMu.Unlock()
	if !s.scheduleAutoRetry(errors.New("last status: 429")) {
		t.Fatal("recovery was not scheduled")
	}
	event := takeRecoveryEvent(t, s, core.EventRetry)
	s.stateMu.Lock()
	delay := s.autoRetry.waited - before
	s.autoRetry.invalidate()
	s.stateMu.Unlock()
	if event.Retry.DelaySeconds != int((delay+time.Second-1)/time.Second) {
		t.Fatalf("displayed delay does not match scheduled wait: %+v, %s", event.Retry, delay)
	}
	return delay
}

func TestAutoRetry_TenSecondCapAllows361AttemptsWithinHour(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprint("jitter=", jitter), func(t *testing.T) {
			s, _ := newRecoveryFixture(t)
			policy, err := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 361, "auto_retry_max_wait_seconds": 3600})
			if err != nil {
				t.Fatal(err)
			}
			policy.jitter = jitter
			s.autoRetry.policy = policy
			s.currentTurn = "failed-turn"
			var total time.Duration
			for range 361 {
				total += scheduledRecoveryDelay(t, s)
			}
			// 2 + 4 + 8 + 358*10 = 3594 seconds. Positive jitter adds
			// at most 1.4 seconds before the cap; another attempt exceeds 3600.
			if total < 3594*time.Second || total > 3595400*time.Millisecond {
				t.Fatalf("unexpected total wait for 361 recoveries: %s", total)
			}
			if s.scheduleAutoRetry(errors.New("last status: 429")) {
				t.Fatal("scheduled more than 361 recoveries")
			}
			s.stateMu.Lock()
			s.autoRetry.policy.maxAttempts++
			s.stateMu.Unlock()
			if s.scheduleAutoRetry(errors.New("last status: 429")) {
				t.Fatal("362nd recovery exceeded the cumulative wait budget")
			}
		})
	}
}

func TestAutoRetry_ServerMinimumAboveCapStopsWithoutShorteningWait(t *testing.T) {
	s, _ := newRecoveryFixture(t)
	policy, err := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 20, "auto_retry_max_wait_seconds": 3600})
	if err != nil {
		t.Fatal(err)
	}
	s.autoRetry.policy = policy
	s.currentTurn = "failed-turn"
	s.providerProxy = &providerProxy{transport: &http.Transport{}, server: &http.Server{}}
	s.providerProxy.rememberRetryResponse(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}})
	failure := errors.New("last status: 429")
	if s.scheduleAutoRetry(failure) {
		t.Fatal("scheduled recovery despite the server minimum exceeding 10 seconds")
	}
	var detail *core.AgentFailure
	if !errors.As(s.recoveryFailure(failure), &detail) || detail.RetryAfter != "60 s" || detail.RetryCount != 0 {
		t.Fatalf("server's minimum wait was lost: %+v", detail)
	}
}

func TestAutoRetry_StopCancelsBackoffAndNewInputSupersedesIt(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(fmt.Sprint("stop=", stop), func(t *testing.T) {
			s, w := newRecoveryFixture(t)
			s.autoRetry.policy.baseDelay = 150 * time.Millisecond
			startRecoveryUserTurn(t, s, w, "task", "t0")
			recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
			takeRecoveryEvent(t, s, core.EventRetry)
			if stop {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				r := startRecoveryUserTurn(t, s, w, "new supplement", "user-replaced-turn")
				if !strings.Contains(fmt.Sprint(r.Params["input"]), "new supplement") {
					t.Fatal("supplement was lost")
				}
				recoveryNotification(s, "turn/completed", "user-replaced-turn", "completed", nil)
				takeRecoveryEvent(t, s, core.EventResult)
			}
			assertNoRecoveryRPC(t, w, 200*time.Millisecond)
		})
	}
}

func TestAutoRetry_InterruptedTurnNeverResumesEvenWithTransientError(t *testing.T) {
	s, w := newRecoveryFixture(t)
	startRecoveryUserTurn(t, s, w, "task", "t0")
	recoveryNotification(s, "turn/completed", "t0", "interrupted", errors.New("last status: 429"))
	takeRecoveryEvent(t, s, core.EventError)
	assertNoRecoveryRPC(t, w, 40*time.Millisecond)
}

func TestAutoRetry_RespectsServerMinimumWaitAndHardLimits(t *testing.T) {
	for _, tc := range []struct {
		header, value string
		wait          bool
	}{
		{"Retry-After", "0.08", true}, {"Retry-After", "100000", false},
		{"X-RateLimit-Dimension", "daily_key_model", false}, {"X-Should-Retry", "false", false},
	} {
		t.Run(tc.header+tc.value, func(t *testing.T) {
			s, w := newRecoveryFixture(t)
			s.providerProxy = &providerProxy{transport: &http.Transport{}, server: &http.Server{}}
			startRecoveryUserTurn(t, s, w, "task", "t0")
			s.providerProxy.rememberRetryResponse(&http.Response{StatusCode: 429, Header: http.Header{http.CanonicalHeaderKey(tc.header): []string{tc.value}}})
			until := s.providerProxy.retryHint.until
			recoveryNotification(s, "turn/completed", "t0", "failed", errors.New("last status: 429"))
			if tc.wait {
				takeRecoveryEvent(t, s, core.EventRetry)
				r := takeRecoveryRPC(t, w)
				if r.sentAt.Before(until) {
					t.Fatal("retried before server's minimum wait")
				}
				acceptRecoveryRPC(s, r, "t1")
			} else {
				takeRecoveryEvent(t, s, core.EventError)
				assertNoRecoveryRPC(t, w, 30*time.Millisecond)
			}
		})
	}
}

func TestAutoRetry_ClassifiesStructuredAndLegacyFailures(t *testing.T) {
	for _, info := range []string{`"rateLimitExceeded"`, `"serverOverloaded"`, `{"responseTooManyFailedAttempts":{"httpStatusCode":429}}`, `{"responseStreamDisconnected":{"httpStatusCode":200}}`, `{"httpConnectionFailed":{"httpStatusCode":null}}`} {
		if !retryableTurnFailure(&codexTurnFailure{Message: "temporary failure", Info: json.RawMessage(info)}) {
			t.Errorf("did not retry %s", info)
		}
	}
	for _, info := range []string{`"usageLimitExceeded"`, `"contextWindowExceeded"`, `"sessionBudgetExceeded"`, `"unauthorized"`, `{"responseTooManyFailedAttempts":{"httpStatusCode":401}}`, `{"responseTooManyFailedAttempts":{"httpStatusCode":400}}`} {
		if retryableTurnFailure(&codexTurnFailure{Message: "last status: 429", Info: json.RawMessage(info)}) {
			t.Errorf("retried permanent structured error %s", info)
		}
	}
	for _, message := range []string{"exceeded retry limit, last status: 429 Too Many Requests", "stream disconnected before completion: stream closed before response.completed", "HTTP 503 Service Unavailable"} {
		if !retryableTurnFailure(errors.New(message)) {
			t.Errorf("ignored transient legacy error %q", message)
		}
	}
	for _, message := range []string{"bad request (429 was quoted)", "permission denied", "insufficient_quota", "canceled", "unknown failure"} {
		if retryableTurnFailure(errors.New(message)) {
			t.Errorf("retried permanent/unknown error %q", message)
		}
	}
}

func TestAutoRetry_ConfigurationSurvivesWorkspaceCloning(t *testing.T) {
	a, err := New(map[string]any{"cmd": "go", "backend": "app_server", "auto_retry_max_attempts": 361, "auto_retry_max_wait_seconds": 3600})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []map[string]any{a.(*Agent).WorkspaceAgentOptions(), a.(*Agent).WorkspaceAgentOptionSnapshot()} {
		policy, err := parseAutoRetryPolicy(opts)
		if err != nil || policy.maxAttempts != 361 || policy.maxWait != time.Hour {
			t.Fatalf("workspace lost retry policy: %+v %v", policy, err)
		}
	}
	for _, opts := range []map[string]any{{"auto_retry_max_attempts": -1}, {"auto_retry_max_attempts": 362}, {"auto_retry_max_wait_seconds": 0}, {"auto_retry_max_wait_seconds": 3601}, {"auto_retry_max_attempts": "secret-invalid"}} {
		if _, err := parseAutoRetryPolicy(opts); err == nil || strings.Contains(err.Error(), "secret-invalid") {
			t.Fatal("invalid policy accepted or value leaked")
		}
	}
}
