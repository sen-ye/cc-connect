package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

const maxMirroredInstructionsBytes = 1 << 20

// Codex emits its model-specific base instructions as the first developer
// message, after an optional additional_tools declaration. Copy that message,
// rather than guessing a model family or inserting a frozen prompt snapshot.
// RawMessage preserves history, IDs, encrypted content, tools and large numbers.
func mirrorDeveloperInstructions(raw []byte) ([]byte, bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return raw, false
	}
	if _, chat := body["messages"]; chat {
		return raw, false
	}
	existing := bytes.TrimSpace(body["instructions"])
	if len(existing) != 0 && !bytes.Equal(existing, []byte("null")) {
		var text string
		if json.Unmarshal(existing, &text) != nil || strings.TrimSpace(text) != "" {
			return raw, false // Leave invalid caller values to upstream validation too.
		}
	}
	var input []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body["input"], &input) != nil {
		return raw, false
	}
	for _, item := range input {
		if item.Role != "developer" || (item.Type != "" && item.Type != "message") {
			continue
		}
		text, ok := developerInstructionText(item.Content)
		if !ok || text == "" || len(text) > maxMirroredInstructionsBytes {
			return raw, false // Never substitute a later permissions/user-context block.
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return raw, false
		}
		body["instructions"] = encoded
		out, err := json.Marshal(body)
		if err != nil {
			return raw, false
		}
		return out, true
	}
	return raw, false
}

func developerInstructionText(content json.RawMessage) (string, bool) {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return strings.TrimSpace(plain), true
	}
	var parts []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return "", false
	}
	var text strings.Builder
	for _, part := range parts {
		if (part.Type != "input_text" && part.Type != "text") || part.Text == nil {
			return "", false
		}
		text.WriteString(*part.Text)
		if text.Len() > maxMirroredInstructionsBytes {
			return "", false
		}
	}
	return strings.TrimSpace(text.String()), true
}
