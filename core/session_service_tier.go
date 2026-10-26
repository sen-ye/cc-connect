package core

import (
	"fmt"
	"strings"
)

func (s *Session) GetServiceTier() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ServiceTier
}

// The caller holds session.settingsMu. Holding the manager lock prevents other
// snapshots from persisting a tentative setting if the write fails.
func (sm *SessionManager) setSessionServiceTier(session *Session, tier string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	session.mu.Lock()
	previous := session.ServiceTier
	session.ServiceTier = tier
	session.mu.Unlock()
	if err := sm.saveLockedWithError(); err != nil {
		session.mu.Lock()
		session.ServiceTier = previous
		session.mu.Unlock()
		return fmt.Errorf("save conversation service tier: %w", err)
	}
	return nil
}

func restoreSessionServiceTier(started AgentSession, tier string) error {
	if tier == "" {
		return nil
	}
	controller, ok := started.(SessionServiceTierController)
	if !ok {
		return fmt.Errorf("agent session does not support saved service tier %q", tier)
	}
	return controller.SetServiceTier(tier)
}

func (e *Engine) fastLabel(tier string) string {
	switch tier {
	case "fast", "priority":
		return e.i18n.T(MsgFastOn)
	case "default":
		return e.i18n.T(MsgFastOff)
	default:
		return tier
	}
}

func (e *Engine) cmdFast(p Platform, msg *Message, args []string) {
	action := "status"
	if len(args) == 1 {
		action = strings.ToLower(args[0])
	}
	if len(args) > 1 || (action != "status" && action != "on" && action != "off") {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFastUsage))
		return
	}
	agent, sessions, key, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	capability, ok := agent.(SessionServiceTierSupport)
	if !ok || !capability.SupportsSessionServiceTier() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFastNotSupported))
		return
	}
	session := sessions.GetOrCreateActive(msg.SessionKey)
	// Match startup's lock order. Once settingsMu is held, a concurrently
	// starting process cannot miss a setting saved before it was published.
	e.interactiveMu.Lock()
	session.settingsMu.Lock()
	var controller SessionServiceTierController
	if state := e.interactiveStates[key]; state != nil {
		state.mu.Lock()
		live := state.agentSession
		if live != nil && live.Alive() && live.CurrentSessionID() == session.GetAgentSessionID() {
			controller, _ = live.(SessionServiceTierController)
		}
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	defer session.settingsMu.Unlock()
	if action == "status" {
		e.replyFastStatus(p, msg, session, controller)
		return
	}
	tier := "default"
	if action == "on" {
		tier = "fast"
	}
	if err := updateSessionServiceTier(sessions, session, controller, tier); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgFastChangeFailed, err))
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgFastChanged, e.fastLabel(tier)))
}

func (e *Engine) replyFastStatus(p Platform, msg *Message, session *Session, controller SessionServiceTierController) {
	tier := session.GetServiceTier()
	if controller != nil {
		if actual := controller.GetServiceTier(); actual != "" {
			tier = actual
		}
	}
	if tier == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFastInherited))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgFastStatus, e.fastLabel(tier)))
	}
}

func updateSessionServiceTier(sessions *SessionManager, session *Session, controller SessionServiceTierController, tier string) error {
	previous := ""
	if controller != nil {
		previous = controller.GetServiceTier()
		if err := controller.SetServiceTier(tier); err != nil {
			return err
		}
	}
	if err := sessions.setSessionServiceTier(session, tier); err != nil {
		if controller != nil {
			if rollbackErr := controller.SetServiceTier(previous); rollbackErr != nil {
				return fmt.Errorf("%w; restore previous runtime tier: %v", err, rollbackErr)
			}
		}
		return err
	}
	return nil
}
