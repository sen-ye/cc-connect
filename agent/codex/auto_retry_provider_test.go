package codex

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAutoRetry_ProviderCapDoesNotChangeOtherSessions(t *testing.T) {
	base, err := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 361, "auto_retry_max_wait_seconds": 3600})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := base.forProvider(&core.ProviderConfig{Name: "limited", CodexAutoRetryMaxDelaySeconds: 32})
	if err != nil || limited.maxDelay != 32*time.Second || limited.maxAttempts != 115 || limited.maxWait != time.Hour {
		t.Fatalf("provider policy = %+v, %v", limited, err)
	}
	for _, provider := range []*core.ProviderConfig{nil, {Name: "other"}} {
		other, err := base.forProvider(provider)
		if err != nil || other != base || other.maxDelay != 10*time.Second || other.maxAttempts != 361 {
			t.Fatal("provider override changed an unrelated conversation or agent default")
		}
	}
	for _, invalid := range []int{-1, 3601} {
		if _, err := base.forProvider(&core.ProviderConfig{CodexAutoRetryMaxDelaySeconds: invalid}); err == nil {
			t.Fatal("invalid provider cap was accepted")
		}
	}
	base.maxAttempts = 0
	disabled, err := base.forProvider(&core.ProviderConfig{CodexAutoRetryMaxDelaySeconds: 32})
	if err != nil || disabled.maxAttempts != 0 {
		t.Fatal("provider override enabled a disabled recovery policy")
	}
}

func TestAutoRetry_Provider32SecondCapResetsAfterProgress(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprint("jitter=", jitter), func(t *testing.T) {
			s, _ := newRecoveryFixture(t)
			base, _ := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 361, "auto_retry_max_wait_seconds": 3600})
			policy, err := base.forProvider(&core.ProviderConfig{CodexAutoRetryMaxDelaySeconds: 32})
			if err != nil {
				t.Fatal(err)
			}
			policy.jitter = jitter
			s.autoRetry.policy = policy
			s.currentTurn = "failed-turn"
			for _, seconds := range []int{2, 4, 8, 16, 32, 32, 32} {
				delay := scheduledRecoveryDelay(t, s)
				minimum := time.Duration(seconds) * time.Second
				maximum := minimum
				if jitter {
					maximum = min(minimum+minimum/10, 32*time.Second)
				}
				if delay < minimum || delay > maximum {
					t.Fatalf("delay = %s, want [%s, %s]", delay, minimum, maximum)
				}
			}
			s.currentTurn = "resumed-turn"
			s.resetAutoRetryAfterProgress(s.CurrentSessionID(), "resumed-turn", map[string]any{"type": "agentMessage", "text": "completed useful progress"})
			if s.autoRetry.attempts != 0 || s.autoRetry.waited != 0 {
				t.Fatal("real progress did not clear the provider failure streak")
			}
			if delay := scheduledRecoveryDelay(t, s); delay < 2*time.Second || delay > 2200*time.Millisecond {
				t.Fatalf("retry after progress resumed at the old backoff: %s", delay)
			}
		})
	}
}

func TestAutoRetry_Provider32SecondCapFits115AttemptsInHour(t *testing.T) {
	for _, jitter := range []bool{false, true} {
		t.Run(fmt.Sprint("jitter=", jitter), func(t *testing.T) {
			s, _ := newRecoveryFixture(t)
			base, _ := parseAutoRetryPolicy(map[string]any{"auto_retry_max_attempts": 361, "auto_retry_max_wait_seconds": 3600})
			policy, _ := base.forProvider(&core.ProviderConfig{CodexAutoRetryMaxDelaySeconds: 32})
			policy.jitter = jitter
			s.autoRetry.policy = policy
			s.currentTurn = "failed-turn"
			var total time.Duration
			for range 115 {
				total += scheduledRecoveryDelay(t, s)
			}
			// 2 + 4 + 8 + 16 + 111*32 = 3582 seconds; jitter adds <= 3.
			if total < 3582*time.Second || total > 3585*time.Second {
				t.Fatalf("unexpected provider wait budget: %s", total)
			}
			if s.scheduleAutoRetry(errors.New("last status: 429")) {
				t.Fatal("scheduled past the provider's 115-attempt limit")
			}
			s.autoRetry.policy.maxAttempts++
			if s.scheduleAutoRetry(errors.New("last status: 429")) {
				t.Fatal("scheduled a 116th attempt beyond the cumulative wait budget")
			}
		})
	}
}
