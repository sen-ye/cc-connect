package cursor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Written by exec's stderr copier; only read after cmd.Wait has joined it.
type cursorTailBuffer struct{ data []byte }

func (b *cursorTailBuffer) Write(p []byte) (int, error) {
	const limit = 16 * 1024
	n := len(p)
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if drop := len(b.data) + len(p) - limit; drop > 0 {
		b.data = b.data[drop:]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (cs *cursorSession) emit(event core.Event) {
	select {
	case cs.events <- event:
	case <-cs.ctx.Done():
	}
}

func (cs *cursorSession) errorEvent(raw map[string]any) core.Event {
	if sid := stringField(raw, "session_id"); sid != "" {
		cs.chatID.Store(sid)
	}
	failure := cursorFailure(raw)
	failure.Detail = safeCursorDetail(failure.Detail, cs.extraEnv)
	failure.Model = cs.model
	return core.Event{Type: core.EventError, SessionID: cs.CurrentSessionID(), Error: failure}
}

func (cs *cursorSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stdin io.WriteCloser, stderr *cursorTailBuffer, started time.Time) {
	defer cs.wg.Done()
	diagnostics, stopDiagnostics := watchCursorDiagnostics(cs.ctx, cmd.Process.Pid, started, cmd.Env, cs.CurrentSessionID, cs.emit)
	var terminal *core.Event
	var plain cursorTailBuffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			_, _ = plain.Write(line)
			_, _ = plain.Write([]byte("\n"))
			continue
		}
		switch stringField(raw, "type") {
		case "result", "error":
			event := cs.resultEvent(raw)
			if stringField(raw, "type") == "error" {
				event = cs.errorEvent(raw)
			}
			// A late failure wins over an earlier success. Do not publish a
			// result until process cleanup has finished and stderr is complete.
			if terminal == nil || terminal.Type != core.EventError {
				terminal = &event
			}
		default:
			cs.handleEvent(raw)
		}
	}
	readErr := scanner.Err()
	_ = stdout.Close()
	_ = stdin.Close()
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	stopDiagnostics() // joins final log flush before choosing the one terminal event
	if cs.CurrentSessionID() == "" && diagnostics.sessionID != "" {
		cs.chatID.Store(diagnostics.sessionID)
	}
	if terminal != nil && terminal.SessionID == "" {
		terminal.SessionID = cs.CurrentSessionID()
	}
	cs.stdinMu.Lock()
	if cs.stdin == stdin {
		cs.stdin = nil
	}
	cs.stdinMu.Unlock()
	cs.pendingMu.Lock()
	cs.pending = nil
	cs.pendingMu.Unlock()
	cs.launchMu.Lock()
	cs.running = false
	cs.launchMu.Unlock()
	if cs.ctx.Err() != nil {
		return
	} // user /stop already owns its notification
	if terminal != nil && terminal.Type == core.EventResult && waitErr == nil && readErr == nil {
		cs.emit(*terminal)
		return
	}
	cs.finishFailedTurn(terminal, diagnostics, string(stderr.data), string(plain.data), readErr, waitErr, cmd.Env)

}

func (cs *cursorSession) finishFailedTurn(terminal *core.Event, diagnostics *cursorDiagnostics, stderr, plain string, readErr, waitErr error, env []string) {
	failure := cursorFailure(map[string]any{"message": strings.TrimSpace(stderr)})
	if diagnostics.failed {
		failure = mergeCursorFailure(diagnostics.failure, failure)
	}
	if terminal != nil && terminal.Type == core.EventError {
		if structured, ok := terminal.Error.(*core.AgentFailure); ok {
			failure = mergeCursorFailure(structured, failure)
		}
	}
	if failure.Detail == "" {
		switch {
		case readErr != nil:
			failure.Detail = fmt.Sprintf("read stdout: %v", readErr)
		case len(plain) > 0:
			failure.Detail = plain
		case waitErr != nil:
			failure.Detail = fmt.Sprintf("process exited: %v", waitErr)
		default:
			failure.Detail = "process exited without a final result"
		}
	}
	if failure.Kind == "" {
		failure.Kind = core.FailureProcess
	}
	failure.Model = cs.model
	failure.Detail = safeCursorDetail(failure.Detail, env)
	failure.RequestID = safeCursorDetail(failure.RequestID, env)
	failure.RetryAfter = safeCursorDetail(failure.RetryAfter, env)
	slog.Warn("cursorSession: turn failed", "kind", failure.Kind, "request_id", failure.RequestID, "retries", failure.RetryCount, "error", failure)
	cs.emit(core.Event{Type: core.EventError, SessionID: cs.CurrentSessionID(), Error: failure})
}
