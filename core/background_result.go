package core

// An autonomous completion may be delivered while a user request or a
// compaction command is active, but cannot close its progress card or release
// its session lock. Result ownership is determined by the agent adapter.
func (e *Engine) deliverBackgroundResult(p Platform, replyCtx any, session *Session, sessions *SessionManager, event Event) {
	if !event.Done || event.Content == "" {
		return
	}
	for _, chunk := range SplitMessageCodeFenceAware(event.Content, maxPlatformMessageLen) {
		e.send(p, replyCtx, chunk)
	}
	session.AddHistory("assistant", event.Content)
	sessions.Save()
}
