package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

var cursorLogUser = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// A diagnostic reader belongs to one Send/process. It never follows latest.log.
type cursorDiagnostics struct {
	failure   *core.AgentFailure
	group     string
	requests  map[string]bool
	notified  int
	failed    bool
	sessionID string
}

func parseCursorDiagnostic(line string) map[string]any {
	start := strings.Index(line, "] ")
	if start < 0 {
		return nil
	}
	line = line[start+2:]
	var body string
	switch {
	case strings.HasPrefix(line, "logger "):
		body = strings.TrimPrefix(line, "logger ")
	case strings.HasPrefix(line, "structured-log.info "):
		body = strings.TrimPrefix(line, "structured-log.info ")
	default:
		return nil
	}
	var raw map[string]any
	if json.Unmarshal([]byte(body), &raw) != nil {
		return nil
	}
	return raw
}

func (d *cursorDiagnostics) observe(line, sessionID string) *core.AgentRetry {
	raw := parseCursorDiagnostic(line)
	if raw == nil {
		return nil
	}
	message := stringField(raw, "message")
	meta := object(raw["metadata"])
	if id := stringField(meta, "conversationId"); id != "" && sessionID != "" && id != sessionID {
		return nil
	}
	isDiagnostic := strings.HasPrefix(message, "[AGENT_ERROR_DIAGNOSTICS] ")
	isNoProgress := message == "[nal_agent_retries] Resume attempts are streaming but not producing checkpoints; stopping automatic resume"
	isFailure := message == "[nal_agent_retries] Error not retryable"
	isOutcome := message == "agent_cli.turn.outcome"
	isAbort := message == "Agent client stream abort"
	if !isDiagnostic && !isNoProgress && !isFailure && !isOutcome && !isAbort {
		return nil
	}
	requestID, accepted := d.acceptRequest(meta, isOutcome || isAbort)
	if !accepted {
		return nil
	}
	if id := stringField(meta, "conversationId"); id != "" {
		d.sessionID = id
	}
	if isAbort {
		return nil
	}
	if d.failure == nil {
		d.failure = &core.AgentFailure{}
	}
	if requestID != "" {
		d.failure.RequestID = requestID
	}
	if isDiagnostic {
		return d.observeRetry(meta, requestID)
	}
	if isNoProgress {
		d.failed = true
		d.failure.Kind = core.FailureNoProgress
		d.failure.RetryCount = max(d.failure.RetryCount, number(meta["noProgressResumeFailures"]))
	}
	if isFailure {
		d.failed = true
		extra := cursorFailure(object(meta["error"]))
		d.failure = mergeCursorFailure(extra, d.failure)
		d.failure.RetryCount = max(d.failure.RetryCount, number(meta["attempt"]))
	}
	if isOutcome {
		switch stringField(meta, "outcome") {
		case "error":
			d.failed = true
			extra := cursorFailure(map[string]any{"error_code": meta["error_code"], "message": meta["error_text"]})
			d.failure = mergeCursorFailure(d.failure, extra)
			if kind := stringField(meta, "error_type"); kind == "retriable" || kind == "non_retriable" {
				retryable := kind == "retriable"
				d.failure.Retryable = &retryable
			}
		case "cancelled":
			d.failed = true
			d.failure.Kind = core.FailureCancelled
		}
	}
	return nil
}

func (d *cursorDiagnostics) acceptRequest(meta map[string]any, isOutcome bool) (string, bool) {
	group := stringField(meta, "originalRequestId")
	if group != "" {
		if d.group != "" && d.group != group {
			return "", false
		}
		d.group = group
	}
	requestID := stringField(meta, "requestId")
	if requestID == "" {
		requestID = stringField(meta, "request_id")
	}
	if d.requests == nil {
		d.requests = make(map[string]bool)
	}
	if isOutcome && d.group != "" && requestID != "" && requestID != d.group && !d.requests[requestID] {
		return "", false
	}
	if requestID != "" {
		d.requests[requestID] = true
	}
	return requestID, true
}

func (d *cursorDiagnostics) observeRetry(meta map[string]any, requestID string) *core.AgentRetry {
	attempt := max(0, number(meta["attempt"]))
	if _, known := meta["attempt"]; known {
		d.failure.RetryCountKnown = true
	}
	d.failure.RetryCount = max(d.failure.RetryCount, attempt)
	if stringField(meta, "transportFailureKind") == "stall" || stringField(meta, "abortOrigin") == "stall_detector" {
		if d.failure.Kind != core.FailureNoProgress {
			d.failure.Kind = core.FailureStalled
		}
	}
	if stringField(meta, "abortOrigin") == "owner" {
		d.failure.Kind = core.FailureCancelled
	}
	willRetry, known := meta["willRetry"].(bool)
	if known && !willRetry {
		d.failed = true
	}
	if willRetry && attempt+1 > d.notified {
		d.notified = attempt + 1
		return &core.AgentRetry{Attempt: attempt + 1, RequestID: requestID}
	}
	return nil
}

type cursorLogTail struct {
	offset  int64
	pending []byte
	skip    bool
}

// Consume bounded chunks while retaining partial lines and skipping oversized
// payloads. An embedded image cannot prevent reading later error records.
func (t *cursorLogTail) consume(data []byte, consume func(string)) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		end := len(data)
		if i >= 0 {
			end = i + 1
		}
		if !t.skip {
			if len(t.pending)+end > 256*1024 {
				t.pending = nil
				t.skip = true
			} else {
				t.pending = append(t.pending, data[:end]...)
			}
		}
		if i >= 0 {
			if !t.skip {
				consume(string(bytes.TrimSpace(t.pending)))
			}
			t.pending = nil
			t.skip = false
		}
		data = data[end:]
	}
}

func cursorLogDirectory(env []string) string {
	values := make(map[string]string)
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			values[k] = v
		}
	}
	tmp := os.TempDir()
	keys := []string{"TMPDIR", "TMP", "TEMP"}
	if runtime.GOOS == "windows" {
		keys = []string{"TEMP", "TMP"}
	}
	for _, key := range keys {
		if values[key] != "" {
			tmp = values[key]
			break
		}
	}
	uid := "default"
	if os.Getuid() >= 0 {
		uid = strconv.Itoa(os.Getuid())
	} else if current, err := user.Current(); err == nil {
		uid = cursorLogUser.ReplaceAllString(current.Username, "_")
	}
	return filepath.Join(tmp, "cursor-agent-logs-"+uid)
}

func cursorProcessLog(name string, pid int, started time.Time) bool {
	// session-2026-10-23T14-49-08-943Z-2953754-1.log
	if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".log") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(name, ".log"), "-")
	if len(parts) != 9 || parts[7] != strconv.Itoa(pid) {
		return false
	}
	stamp := strings.Join(parts[1:6], "-") + "." + parts[6]
	when, err := time.Parse("2006-01-02T15-04-05.000Z", stamp)
	return err == nil && !when.Before(started.Add(-time.Second))
}

// watchCursorDiagnostics returns a stop function which flushes and joins the
// watcher. The state may only be read after stop returns.
func watchCursorDiagnostics(ctx context.Context, pid int, started time.Time, env []string, sessionID func() string, emit func(core.Event)) (*cursorDiagnostics, func()) {
	state := &cursorDiagnostics{}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tails := make(map[string]*cursorLogTail)
		directory := cursorLogDirectory(env)
		warned := false
		poll := func() {
			entries, err := os.ReadDir(directory)
			if err != nil {
				if !os.IsNotExist(err) && !warned {
					slog.Warn("cursor: cannot read diagnostic directory", "error", err)
					warned = true
				}
				return // logs may be disabled or not yet created
			}
			for _, entry := range entries {
				if !entry.Type().IsRegular() || !cursorProcessLog(entry.Name(), pid, started) {
					continue
				}
				path := filepath.Join(directory, entry.Name())
				tail := tails[path]
				if tail == nil {
					tail = &cursorLogTail{}
					tails[path] = tail
				}
				if err := readCursorLog(path, tail, func(line string) {
					if retry := state.observe(line, sessionID()); retry != nil {
						emit(core.Event{Type: core.EventRetry, SessionID: sessionID(), Retry: retry})
					}
				}); err != nil {
					if !os.IsNotExist(err) && !warned {
						slog.Warn("cursor: diagnostic log unavailable", "error", err)
						warned = true
					}
				}
			}
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				poll()
			case <-stop:
				poll()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return state, func() { close(stop); <-done }
}

func readCursorLog(path string, tail *cursorLogTail, consume func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < tail.offset {
		*tail = cursorLogTail{}
	}
	if _, err = f.Seek(tail.offset, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4*1024*1024))
	if err != nil {
		return fmt.Errorf("read diagnostic log: %w", err)
	}
	tail.offset += int64(len(data))
	tail.consume(data, consume)
	return nil
}
