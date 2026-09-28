package claudecode

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// A group consists of one foreground input and its /ps supplements. Retaining
// unconsumed inputs with their original group prevents a late result from
// completing a newer foreground request.
type claudeTurnTracker struct {
	sequence      uint64
	active        uint64
	primary       string
	pending       map[string]uint64
	correlated    bool
	finished      map[string]bool
	finishedOrder []string
	results       map[string]bool
	resultOrder   []string
}

func (cs *claudeSession) writeUserMessage(content any, messageID string) error {
	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("claude: generate input UUID: %w", err)
	}
	key := id.String()
	cs.stdinMu.Lock()
	defer cs.stdinMu.Unlock()
	cs.turnMu.Lock()
	t := &cs.turns
	if t.pending == nil {
		t.pending = make(map[string]uint64)
	}
	if t.active == 0 || messageID != "" {
		t.sequence++
		t.active = t.sequence
		t.primary = key
	}
	t.pending[key] = t.active
	cs.turnMu.Unlock()
	err = cs.writeJSONLocked(map[string]any{"type": "user", "uuid": key, "message": map[string]any{"role": "user", "content": content}})
	if err != nil {
		cs.turnMu.Lock()
		delete(t.pending, key)
		if t.primary == key {
			t.active = 0
			t.primary = ""
		}
		cs.turnMu.Unlock()
	}
	return err
}

func (cs *claudeSession) handleCommandLifecycle(raw map[string]any) {
	id, _ := raw["command_uuid"].(string)
	state, _ := raw["state"].(string)
	cs.turnMu.Lock()
	_, known := cs.turns.pending[id]
	if known {
		cs.turns.correlated = true
	}
	cancelled := known && state == "cancelled" && cs.turns.primary == id
	cs.turnMu.Unlock()
	if cancelled {
		cs.handleResult(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "user_message_uuid": id, "errors": []any{context.Canceled.Error()}})
	}
}

func claudeResultFailed(raw map[string]any) bool {
	failed, _ := raw["is_error"].(bool)
	reason, _ := raw["terminal_reason"].(string)
	return failed || strings.HasPrefix(resultSubtype(raw), "error_") || strings.HasPrefix(reason, "aborted_")
}

func resultUserUUIDs(raw map[string]any) []string {
	var ids []string
	if values, ok := raw["user_message_uuids"].([]any); ok {
		for _, value := range values {
			if id, ok := value.(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	if id, ok := raw["user_message_uuid"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	return ids
}

func rememberResultKey(keys *map[string]bool, order *[]string, key string) {
	if key == "" {
		return
	}
	if *keys == nil {
		*keys = make(map[string]bool)
	}
	if (*keys)[key] {
		return
	}
	(*keys)[key] = true
	*order = append(*order, key)
	if len(*order) > 256 {
		delete(*keys, (*order)[0])
		*order = (*order)[1:]
	}
}

// Modern Claude returns client UUIDs even for local commands and empty
// successes. Missing UUIDs on a correlated stream indicate autonomous work,
// except session-wide failures. Legacy producers retain their existing result
// behavior; no timing or response-text heuristic is used.
func (cs *claudeSession) resultScope(raw map[string]any, intermediate bool) (background, drop bool) {
	cs.turnMu.Lock()
	defer cs.turnMu.Unlock()
	t := &cs.turns
	id, _ := raw["uuid"].(string)
	historical, _ := raw["historical"].(bool)
	if historical || (id != "" && t.results[id]) {
		return false, true
	}
	if child, _ := raw["parent_tool_use_id"].(string); child != "" {
		return false, true
	}
	ids := resultUserUUIDs(raw)
	matched, known, finished := false, false, false
	for _, key := range ids {
		group, ok := t.pending[key]
		known = known || ok
		matched = matched || (ok && group == t.active && t.active != 0)
		finished = finished || t.finished[key]
	}
	if len(ids) > 0 {
		t.correlated = true
	}
	if finished && !known {
		return false, true
	}
	origin, _ := raw["origin"].(map[string]any)
	kind, _ := origin["kind"].(string)
	if !matched {
		background = len(ids) > 0 || kind == "task-notification" || (t.active != 0 && t.correlated && !claudeResultFailed(raw))
	}
	if !intermediate {
		rememberResultKey(&t.results, &t.resultOrder, id)
		for _, key := range ids {
			if _, ok := t.pending[key]; ok {
				delete(t.pending, key)
				rememberResultKey(&t.finished, &t.finishedOrder, key)
			}
		}
		if !background {
			// Legacy or session-wide failures have no per-input correlation.
			if len(ids) == 0 {
				for key, group := range t.pending {
					if group == t.active {
						delete(t.pending, key)
						rememberResultKey(&t.finished, &t.finishedOrder, key)
					}
				}
			}
			t.active = 0
			t.primary = ""
		}
	}
	if background {
		slog.Debug("claude: autonomous result does not complete foreground request", "result_id", id, "subtype", resultSubtype(raw), "origin", kind, "pending_inputs", len(t.pending))
	}
	return background, false
}
