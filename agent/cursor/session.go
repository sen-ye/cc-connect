package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// cursorSession manages multi-turn conversations with the Cursor Agent CLI.
// Each Send() launches a new `agent --print` process with --resume for continuity.
type cursorSession struct {
	cmd       string   // CLI binary name
	extraArgs []string // extra args from cmd, prepended before agent args
	workDir   string
	model     string
	mode      string
	extraEnv  []string
	events    chan core.Event
	chatID    atomic.Value // stores string — Cursor chat/session ID
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
	launchMu  sync.Mutex // serializes Send with Close and protects running
	running   bool
	closeOnce sync.Once

	thinkingBuf strings.Builder // accumulate thinking deltas

	// Permission handling: each Send() creates a new process whose stdin is used
	// to respond to interaction_query permission requests.
	stdinMu sync.Mutex
	stdin   io.WriteCloser // current process stdin; nil when no process is running

	pendingMu sync.Mutex
	pending   *pendingInteractionQuery // most recent unresolved interaction_query/request
}

// pendingInteractionQuery holds the info needed to write a response back to Cursor.
type pendingInteractionQuery struct {
	id        int    // query["id"] value from the original request
	queryType string // e.g. "webFetchRequestQuery", "shellRequestQuery"
}

func newCursorSession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, resumeID string, extraEnv []string) (*cursorSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	cs := &cursorSession{
		cmd:       cmd,
		extraArgs: extraArgs,
		workDir:   workDir,
		model:     model,
		mode:      mode,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 64),
		ctx:       sessionCtx,
		cancel:    cancel,
	}
	cs.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		cs.chatID.Store(resumeID)
	}

	return cs, nil
}

func (cs *cursorSession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	cs.launchMu.Lock()
	defer cs.launchMu.Unlock()
	if !cs.alive.Load() {
		return fmt.Errorf("cursor: session is closed")
	}
	if cs.running {
		return fmt.Errorf("cursor: previous turn is still running")
	}
	if len(images) > 0 {
		// The Cursor Agent CLI has no image flag, but it can open image files
		// from disk and see their content. So route images through the same
		// on-disk channel as files instead of dropping them.
		imgFiles := make([]core.FileAttachment, 0, len(images))
		for i, im := range images {
			name := im.FileName
			if name == "" {
				// Feishu image messages carry no filename — synthesise one so
				// the file lands with a correct extension.
				name = fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, core.ExtFromMime(im.MimeType))
			}
			imgFiles = append(imgFiles, core.FileAttachment{
				MimeType: im.MimeType,
				Data:     im.Data,
				FileName: name,
			})
		}
		imagePaths := core.SaveFilesToDisk(cs.workDir, messageID, imgFiles)
		prompt = core.AppendImageRefs(prompt, imagePaths)
	}
	if len(files) > 0 {
		filePaths := core.SaveFilesToDisk(cs.workDir, messageID, files)
		prompt = core.AppendFileRefs(prompt, filePaths)
	}
	if !cs.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	chatID := cs.CurrentSessionID()
	isResume := chatID != ""

	args := append(append([]string{}, cs.extraArgs...),
		"--print",
		"--output-format", "stream-json",
	)

	switch cs.mode {
	case "force":
		args = append(args, "--force")
	case "plan":
		args = append(args, "--mode", "plan")
	case "ask":
		args = append(args, "--mode", "ask")
	}

	if isResume {
		args = append(args, "--resume", chatID)
	}
	if cs.model != "" {
		args = append(args, "--model", cs.model)
	}
	args = append(args, "--workspace", cs.workDir, "--", prompt)

	slog.Debug("cursorSession: launching", "resume", isResume, "args", core.RedactArgs(args))

	cmd := exec.CommandContext(cs.ctx, cs.cmd, args...)
	cmd.Dir = cs.workDir
	env := os.Environ()
	if len(cs.extraEnv) > 0 {
		env = core.MergeEnv(env, cs.extraEnv)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cursorSession: stdout pipe: %w", err)
	}

	// Set up a stdin pipe so we can write interaction_query responses back to Cursor.
	// Without a connected stdin, Cursor gets EOF and auto-rejects all permission requests.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cursorSession: stdin pipe: %w", err)
	}

	var stderrBuf cursorTailBuffer
	cmd.Stderr = &stderrBuf

	started := time.Now()
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("cursorSession: start: %w", err)
	}

	cs.stdinMu.Lock()
	cs.stdin = stdin
	cs.stdinMu.Unlock()

	cs.running = true
	cs.thinkingBuf.Reset()
	cs.wg.Add(1)
	go cs.readLoop(cmd, stdout, stdin, &stderrBuf, started)

	return nil
}

func (cs *cursorSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "system":
		cs.handleSystem(raw)

	case "user":
		// User echo — nothing to do

	case "thinking":
		cs.handleThinking(raw)

	case "assistant":
		cs.handleAssistant(raw)

	case "tool_call":
		cs.handleToolCall(raw)

	case "interaction_query":
		cs.handleInteractionQuery(raw)

	case "error":
		cs.emit(cs.errorEvent(raw))

	case "result":
		cs.handleResult(raw)

	default:
		slog.Debug("cursorSession: unhandled event", "type", eventType)
	}
}

func (cs *cursorSession) handleSystem(raw map[string]any) {
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.chatID.Store(sid)
		slog.Debug("cursorSession: session init", "session_id", sid)

		model, _ := raw["model"].(string)
		evt := core.Event{Type: core.EventText, SessionID: sid, Content: "", ToolName: model}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
}

func (cs *cursorSession) handleThinking(raw map[string]any) {
	subtype, _ := raw["subtype"].(string)
	switch subtype {
	case "delta":
		if text, _ := raw["text"].(string); text != "" {
			cs.thinkingBuf.WriteString(text)
		}
	default:
		text := cs.thinkingBuf.String()
		cs.thinkingBuf.Reset()
		if text != "" {
			evt := core.Event{Type: core.EventThinking, Content: text}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		}
	}
}

func (cs *cursorSession) handleAssistant(raw map[string]any) {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return
	}
	contentArr, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, contentItem := range contentArr {
		item, ok := contentItem.(map[string]any)
		if !ok {
			continue
		}
		contentType, _ := item["type"].(string)
		if contentType == "text" {
			if text, ok := item["text"].(string); ok && text != "" {
				evt := core.Event{Type: core.EventText, Content: text}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		}
	}
}

func (cs *cursorSession) handleToolCall(raw map[string]any) {
	subtype, _ := raw["subtype"].(string)
	tc, _ := raw["tool_call"].(map[string]any)
	if tc == nil {
		return
	}

	if subtype == "started" {
		name, input := extractToolInfo(tc)
		if name != "" {
			evt := core.Event{Type: core.EventToolUse, ToolName: name, ToolInput: input}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		}
	}
	// "completed" tool_call events contain results; we log but don't emit to chat
	if subtype == "completed" {
		name, _ := extractToolInfo(tc)
		slog.Debug("cursorSession: tool completed", "tool", name)
	}
}

func (cs *cursorSession) handleInteractionQuery(raw map[string]any) {
	subtype, _ := raw["subtype"].(string)
	if subtype != "request" {
		return
	}

	queryType, _ := raw["query_type"].(string)
	query, _ := raw["query"].(map[string]any)
	if query == nil {
		return
	}

	// query["id"] is a JSON number — decoded as float64 by Go's json package.
	idFloat, _ := query["id"].(float64)
	queryID := int(idFloat)

	toolName, inputStr := extractInteractionQueryInfo(queryType, query)
	if toolName == "" {
		// Unknown query type — deny immediately to unblock Cursor.
		cs.writeInteractionResponse(queryID, queryType, false, "unsupported query type")
		return
	}

	// Check skipApproval flag: some queries are pre-approved by Cursor itself.
	if skipApproval := extractSkipApproval(queryType, query); skipApproval {
		cs.writeInteractionResponse(queryID, queryType, true, "")
		return
	}

	// In non-default modes there is no interactive approval path; deny to unblock.
	if cs.mode != "default" {
		cs.writeInteractionResponse(queryID, queryType, false, "permission mode does not allow interactive approval")
		return
	}

	// Store pending query so RespondPermission can write the right response.
	cs.pendingMu.Lock()
	cs.pending = &pendingInteractionQuery{id: queryID, queryType: queryType}
	cs.pendingMu.Unlock()

	// Build a stable requestID that the engine passes back to RespondPermission.
	requestID := fmt.Sprintf("%s:%d", queryType, queryID)

	slog.Info("cursorSession: permission request", "request_id", requestID, "tool", toolName)
	evt := core.Event{
		Type:      core.EventPermissionRequest,
		RequestID: requestID,
		ToolName:  toolName,
		ToolInput: inputStr,
	}
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
		return
	}
}

// writeInteractionResponse writes a Cursor interaction_query response JSON line to the
// current process stdin.  approved=true → {"approved":{}}; false → {"rejected":{"reason":...}}.
func (cs *cursorSession) writeInteractionResponse(queryID int, queryType string, approved bool, reason string) {
	// Derive the response field name from the request query type.
	// "webFetchRequestQuery" → "webFetchRequestResponse"
	// "shellRequestQuery"   → "shellRequestResponse"
	responseKey := strings.TrimSuffix(queryType, "Query") + "Response"

	var resultValue any
	if approved {
		resultValue = map[string]any{"approved": map[string]any{}}
	} else {
		resultValue = map[string]any{"rejected": map[string]any{"reason": reason}}
	}

	resp := map[string]any{
		"type":       "interaction_query",
		"subtype":    "response",
		"query_type": queryType,
		"response": map[string]any{
			"id":        queryID,
			responseKey: resultValue,
		},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		slog.Error("cursorSession: marshal interaction response", "error", err)
		return
	}
	b = append(b, '\n')

	cs.stdinMu.Lock()
	defer cs.stdinMu.Unlock()
	if cs.stdin == nil {
		slog.Warn("cursorSession: stdin unavailable, cannot write interaction response")
		return
	}
	if _, err := cs.stdin.Write(b); err != nil {
		slog.Warn("cursorSession: write interaction response", "error", err)
	}
}

// extractSkipApproval returns true when Cursor marks a query as pre-approved.
func extractSkipApproval(queryType string, query map[string]any) bool {
	// The inner query object is nested under the queryType key.
	inner, _ := query[queryType].(map[string]any)
	if inner == nil {
		return false
	}
	skip, _ := inner["skipApproval"].(bool)
	return skip
}

func extractInteractionQueryInfo(queryType string, query map[string]any) (string, string) {
	switch queryType {
	case "webFetchRequestQuery":
		if inner, ok := query["webFetchRequestQuery"].(map[string]any); ok {
			if args, ok := inner["args"].(map[string]any); ok {
				url, _ := args["url"].(string)
				return "WebFetch", url
			}
		}
	case "shellRequestQuery":
		if inner, ok := query["shellRequestQuery"].(map[string]any); ok {
			if args, ok := inner["args"].(map[string]any); ok {
				cmd, _ := args["command"].(string)
				return "Bash", cmd
			}
		}
	}

	name := strings.TrimSuffix(queryType, "RequestQuery")
	name = strings.TrimSuffix(name, "Query")
	if name == "" {
		name = queryType
	}
	return name, ""
}

// extractToolInfo parses the nested tool_call structure from Cursor's stream-json.
// Tool calls can be shellToolCall, readToolCall, editToolCall, etc.
func extractToolInfo(tc map[string]any) (name string, input string) {
	toolTypes := []struct {
		key      string
		toolName string
	}{
		{"shellToolCall", "Bash"},
		{"readToolCall", "Read"},
		{"editToolCall", "Edit"},
		{"writeToolCall", "Write"},
		{"listToolCall", "List"},
		{"searchToolCall", "Search"},
		{"grepToolCall", "Grep"},
		{"globToolCall", "Glob"},
		{"webFetchToolCall", "WebFetch"},
	}

	for _, tt := range toolTypes {
		if call, ok := tc[tt.key].(map[string]any); ok {
			name = tt.toolName
			input = extractToolInput(name, call)
			return
		}
	}

	// Generic: try "description" field at top level
	if desc, ok := tc["description"].(string); ok && desc != "" {
		return "Tool", truncateStr(desc, 200)
	}

	return "", ""
}

func extractToolInput(toolName string, call map[string]any) string {
	args, _ := call["args"].(map[string]any)
	if args == nil {
		if desc, ok := call["description"].(string); ok {
			return desc
		}
		return ""
	}

	switch toolName {
	case "Bash":
		if cmd, ok := args["command"].(string); ok {
			return cmd
		}
	case "Read", "Edit", "Write":
		if p := cursorToolArg(args, "path", "filePath", "file_path"); p != "" {
			return p
		}
	case "Grep":
		if pattern := cursorToolArg(args, "pattern", "regex"); pattern != "" {
			if dir := cursorToolArg(args, "path", "targetDirectory", "target_directory"); dir != "" {
				return dir + " · " + pattern
			}
			return pattern
		}
	case "Glob":
		if input := cursorGlobInput(args); input != "" {
			return input
		}
	case "List":
		if dir := cursorToolArg(args, "path", "targetDirectory", "target_directory"); dir != "" {
			return dir
		}
	case "Search":
		if query := cursorToolArg(args, "query", "pattern"); query != "" {
			return query
		}
	case "WebFetch":
		if url := cursorToolArg(args, "url"); url != "" {
			return url
		}
	}

	if desc, ok := call["description"].(string); ok && desc != "" {
		return desc
	}

	b, _ := json.Marshal(args)
	return string(b)
}

func cursorGlobInput(args map[string]any) string {
	// Cursor's stream-json uses protobuf camelCase, while its stored
	// history uses snake_case. Older CLI versions used plain pattern.
	pattern := cursorToolArg(args, "globPattern", "glob_pattern", "pattern")
	dir := cursorToolArg(args, "targetDirectory", "target_directory", "path")
	if pattern != "" {
		if dir != "" && !filepath.IsAbs(pattern) {
			return filepath.Join(dir, pattern)
		}
		return pattern
	}
	if dir != "" {
		return dir
	}
	return ""
}

func cursorToolArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func (cs *cursorSession) handleResult(raw map[string]any) {
	cs.emit(cs.resultEvent(raw))
}

func (cs *cursorSession) resultEvent(raw map[string]any) core.Event {
	if failedCursorResult(raw) {
		return cs.errorEvent(raw)
	}
	var content string
	if result, ok := raw["result"].(string); ok {
		content = result
	}
	if sid, ok := raw["session_id"].(string); ok && sid != "" {
		cs.chatID.Store(sid)
	}
	// Cursor emits a `usage` block on the result event with camelCase keys
	// (inputTokens / outputTokens / cacheReadTokens / cacheWriteTokens).
	// Without this, the engine's turn-complete log always reports 0 tokens
	// and the reply footer is suppressed by #701's workdir-only guard. See
	// issue #1810.
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int
	if usage, ok := raw["usage"].(map[string]any); ok {
		inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens = parseCursorUsage(usage)
	}
	evt := core.Event{
		Type:                     core.EventResult,
		Content:                  content,
		SessionID:                cs.CurrentSessionID(),
		Done:                     true,
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheReadInputTokens:     cacheReadTokens,
		CacheCreationInputTokens: cacheWriteTokens,
	}
	return evt
}

// parseCursorUsage extracts token counts from a Cursor result event's `usage`
// object. The Cursor Agent CLI emits camelCase keys (inputTokens,
// outputTokens, cacheReadTokens, cacheWriteTokens) — different from
// Claude Code's snake_case — so this helper mirrors agent/claudecode's
// parseClaudeUsage but uses the right field names. Returns 0 for any
// field that is missing or not numeric, so callers can safely emit an
// Event with partial usage (e.g. when the CLI downgrades the schema).
func parseCursorUsage(usage map[string]any) (input, output, cacheRead, cacheWrite int) {
	if v, ok := usage["inputTokens"].(float64); ok {
		input = int(v)
	}
	if v, ok := usage["outputTokens"].(float64); ok {
		output = int(v)
	}
	if v, ok := usage["cacheReadTokens"].(float64); ok {
		cacheRead = int(v)
	}
	if v, ok := usage["cacheWriteTokens"].(float64); ok {
		cacheWrite = int(v)
	}
	return
}

// RespondPermission writes the user's approval/denial decision back to the Cursor Agent
// CLI via stdin.  Cursor is blocked waiting for this response when in default mode.
func (cs *cursorSession) RespondPermission(_ string, result core.PermissionResult) error {
	cs.pendingMu.Lock()
	pq := cs.pending
	cs.pending = nil
	cs.pendingMu.Unlock()

	if pq == nil {
		// No interaction query is pending — nothing to respond to.
		return nil
	}

	approved := result.Behavior == "allow"
	reason := result.Message
	if !approved && reason == "" {
		reason = "User denied"
	}
	cs.writeInteractionResponse(pq.id, pq.queryType, approved, reason)
	return nil
}

func (cs *cursorSession) Events() <-chan core.Event {
	return cs.events
}

func (cs *cursorSession) CurrentSessionID() string {
	v, _ := cs.chatID.Load().(string)
	return v
}

func (cs *cursorSession) Alive() bool {
	return cs.alive.Load()
}

func (cs *cursorSession) Close() error {
	cs.closeOnce.Do(func() {
		cs.launchMu.Lock()
		cs.alive.Store(false)
		cs.cancel()
		cs.launchMu.Unlock()
		done := make(chan struct{})
		go func() { cs.wg.Wait(); close(cs.events); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			slog.Warn("cursorSession: close timed out; channels close after readers exit")
		}
	})
	return nil
}

func truncateStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
