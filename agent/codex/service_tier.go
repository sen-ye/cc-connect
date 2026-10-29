package codex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func (a *Agent) SupportsSessionServiceTier() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.backend == "app_server"
}

// Bridge conversations start in standard mode even if the CLI defaults to Fast.
func (a *Agent) DefaultSessionServiceTier() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.backend != "app_server" {
		return ""
	}
	if a.defaultServiceTier != "" {
		return a.defaultServiceTier
	}
	return "default"
}

func (s *appServerSession) applyReportedServiceTier(tier *string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.serviceTier = stringValue(tier)
	if s.serviceTier == "priority" {
		s.serviceTier = "fast"
	}
	if s.serviceTier == "" {
		s.serviceTier = "default"
	}
	if s.serviceTierNotify != nil {
		select {
		case s.serviceTierNotify <- struct{}{}:
		default:
		}
	}
}

func (s *appServerSession) handleServiceTierUpdate(raw json.RawMessage) {
	var update struct {
		ThreadID string `json:"threadId"`
		Settings struct {
			ServiceTier json.RawMessage `json:"serviceTier"`
		} `json:"threadSettings"`
	}
	if json.Unmarshal(raw, &update) != nil || update.ThreadID != s.CurrentSessionID() || len(update.Settings.ServiceTier) == 0 {
		return
	}
	var tier *string
	if json.Unmarshal(update.Settings.ServiceTier, &tier) == nil {
		s.applyReportedServiceTier(tier)
	}
}

func (s *appServerSession) GetServiceTier() string {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.serviceTier
}

// This native RPC updates only subsequent turns on this thread. It does not
// rewrite the CLI configuration, interrupt a turn, or send a prompt to a model.
func (s *appServerSession) SetServiceTier(tier string) error {
	if tier != "fast" && tier != "default" {
		return fmt.Errorf("codex: unsupported conversation service tier %q", tier)
	}
	s.serviceTierMu.Lock()
	defer s.serviceTierMu.Unlock()
	threadID := s.CurrentSessionID()
	if !s.Alive() || threadID == "" {
		return fmt.Errorf("codex: cannot change service tier on a closed or uninitialized session")
	}
	changed := make(chan struct{}, 1)
	s.runtimeMu.Lock()
	s.serviceTierNotify = changed
	s.runtimeMu.Unlock()
	defer func() {
		s.runtimeMu.Lock()
		s.serviceTierNotify = nil
		s.runtimeMu.Unlock()
	}()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	if err := s.requestWithTimeout("thread/settings/update", map[string]any{
		"threadId": threadID, "serviceTier": tier,
	}, nil, 15*time.Second); err != nil {
		return fmt.Errorf("codex: update conversation service tier: %w", err)
	}
	// Some CLI versions acknowledge settings that the model cannot use. Only
	// report success after the native thread confirms the effective tier.
	for s.GetServiceTier() != tier {
		select {
		case <-changed:
		case <-s.ctx.Done():
			return fmt.Errorf("codex: await service tier update: %w", s.ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("codex: service tier %q was not confirmed (effective: %q)", tier, s.GetServiceTier())
		}
	}
	return nil
}

var _ core.SessionServiceTierSupport = (*Agent)(nil)
var _ core.SessionServiceTierController = (*appServerSession)(nil)
