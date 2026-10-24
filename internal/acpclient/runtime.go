package acpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// RuntimeOptions exposes the shared protocol implementation to agent adapters.
// Interruptible is opt-in: ACP itself does not guarantee overlapping prompts
// can safely replace one another. This implementation cancels and drains first.
type RuntimeOptions struct {
	Command, WorkDir, SessionID, AuthMethod, Mode, Model string
	Args, Env                                            []string
	Interruptible, StrictResume, ParameterizedModels     bool
	ErrorMapper                                          func(error) error
}

func Start(ctx context.Context, o RuntimeOptions) (core.AgentSession, error) {
	s, err := newACPSession(ctx, acpSessionConfig{
		command: o.Command, args: o.Args, extraEnv: o.Env, workDir: o.WorkDir,
		resumeSessionID: o.SessionID, authMethod: o.AuthMethod, initialMode: o.Mode,
		initialModel: o.Model, interruptible: o.Interruptible,
		strictResume: o.StrictResume, parameterizedModels: o.ParameterizedModels,
		errorMapper: o.ErrorMapper,
	})
	if err != nil {
		return nil, err
	}
	if o.Interruptible {
		return &appendingSession{acpSession: s}, nil
	}
	return s, nil
}

type appendingSession struct{ *acpSession }

func (s *appendingSession) AppendMessage(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	return s.appendMessage(prompt, messageID, images, files)
}

type asyncTurn struct {
	done       chan struct{}
	superseded bool // guarded by turnMu
}

func (s *acpSession) promptBlocks(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) []any {
	paths := core.SaveFilesToDisk(s.workDir, messageID, files)
	prompt = core.AppendFileRefs(prompt, paths)
	blocks := []any{map[string]any{"type": "text", "text": prompt}}
	for _, img := range images {
		mime := img.MimeType
		if mime == "" {
			mime = "image/png"
		}
		blocks = append(blocks, map[string]any{"type": "image", "mimeType": mime,
			"data": base64.StdEncoding.EncodeToString(img.Data)})
	}
	return blocks
}

func (s *acpSession) sendAsync(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.startAsyncLocked(s.promptBlocks(prompt, messageID, images, files))
}

func (s *acpSession) startAsyncLocked(blocks []any) error {
	if !s.alive.Load() || s.ctx.Err() != nil {
		return fmt.Errorf("acp: session closed")
	}
	s.turnMu.Lock()
	if s.turn != nil {
		s.turnMu.Unlock()
		return fmt.Errorf("acp: previous turn is still running")
	}
	turn := &asyncTurn{done: make(chan struct{})}
	s.turn = turn
	s.promptInFlight.Add(1)
	s.turnMu.Unlock()
	wait, err := s.tr.beginCall(s.ctx, "session/prompt", map[string]any{
		"sessionId": s.currentACPSessionID(), "prompt": blocks,
	})
	if err != nil {
		s.turnMu.Lock()
		s.turn = nil
		close(turn.done)
		s.promptInFlight.Add(-1)
		s.turnMu.Unlock()
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_, err := wait()
		s.flushThinking()
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		s.turn = nil
		defer close(turn.done)
		defer s.promptInFlight.Add(-1)
		if turn.superseded || s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("acp: prompt: %w", err)})
		} else {
			s.emit(core.Event{Type: core.EventResult, Done: true})
		}
	}()
	return nil
}

// AppendMessage preserves the native session and adds an instruction after the
// cancelled request has drained. Only the replacement request may finish the
// engine's task; stale permission callbacks are discarded during the handover.
func (s *acpSession) appendMessage(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.turnMu.Lock()
	turn := s.turn
	if turn == nil {
		s.turnMu.Unlock()
		return fmt.Errorf("acp: no running turn to supplement")
	}
	turn.superseded = true
	// Suppress synthetic background completion events between the requests.
	s.promptInFlight.Add(1)
	s.turnMu.Unlock()
	defer s.promptInFlight.Add(-1)
	if err := s.CancelTurn(); err != nil {
		s.turnMu.Lock()
		turn.superseded = false
		finished := s.turn == nil
		s.turnMu.Unlock()
		if finished {
			s.emit(core.Event{Type: core.EventError, Error: err})
		}
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case <-turn.done:
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-deadline.C:
		err := fmt.Errorf("acp: timed out cancelling the previous generation")
		s.emit(core.Event{Type: core.EventError, Error: err})
		s.cancel()
		return err
	}
	s.permMu.Lock()
	clear(s.permByID)
	s.permMu.Unlock()
	s.emit(core.Event{Type: core.EventTurnSuperseded, Content: prompt})
	prompt = "Additional instruction for the current task. Continue the original task using the existing conversation history and incorporate the following user message:\n\n" + prompt
	if err := s.startAsyncLocked(s.promptBlocks(prompt, messageID, images, files)); err != nil {
		s.emit(core.Event{Type: core.EventError, Error: err})
		return err
	}
	return nil
}

func (s *acpSession) flushThinking() {
	s.thinkingMu.Lock()
	text := s.thinking.String()
	s.thinking.Reset()
	s.thinkingMu.Unlock()
	if strings.TrimSpace(text) != "" {
		s.emit(core.Event{Type: core.EventThinking, Content: text})
	}
}

func (s *acpSession) GetModel() string {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	return s.currentModel
}

func (s *acpSession) absorbModel(raw json.RawMessage) {
	var response struct {
		Models struct {
			Current string `json:"currentModelId"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &response) == nil && response.Models.Current != "" {
		s.modelsMu.Lock()
		s.currentModel = response.Models.Current
		s.modelsMu.Unlock()
	}
}

func (s *acpSession) applyModel(model string) error {
	base, suffix, parameterized := strings.Cut(model, "[")
	base = strings.TrimSpace(base)
	params := [][2]string{{"model", base}}
	if parameterized {
		if !strings.HasSuffix(suffix, "]") {
			return fmt.Errorf("acp: invalid model parameters")
		}
		for _, pair := range strings.Split(strings.TrimSuffix(suffix, "]"), ",") {
			if strings.TrimSpace(pair) == "" {
				continue
			}
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				return fmt.Errorf("acp: invalid model parameter")
			}
			params = append(params, [2]string{strings.TrimSpace(key), strings.TrimSpace(value)})
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	for _, param := range params {
		if _, err := s.tr.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": s.currentACPSessionID(), "configId": param[0], "value": param[1],
		}); err != nil {
			return fmt.Errorf("acp: configure model %s: %w", param[0], err)
		}
	}
	s.modelsMu.Lock()
	s.currentModel = model
	s.modelsMu.Unlock()
	return nil
}
