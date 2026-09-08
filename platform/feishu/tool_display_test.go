package feishu

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestBuildToolDisplay_KeepsBashHeredocAndQuotedFlags(t *testing.T) {
	cases := []struct {
		name       string
		tool       string
		input      string
		wantSubstr []string
		notEqual   []string
		notContain []string
	}{
		{
			name: "python heredoc delimiter is not the command",
			tool: "Bash",
			input: "python3 - <<'PY'\n" +
				"from pathlib import Path\n" +
				"p=Path('research/new-bounties/puter-feasibility.md')\n" +
				"p.write_text('''# Puter\\n官方私人报告渠道：**security@puter.com**。\\n''')\n" +
				"PY",
			wantSubstr: []string{"python3 - <<'PY'", "puter-feasibility.md"},
			notEqual:   []string{"PY", "security@puter.com"},
			notContain: []string{"security@puter.com"},
		},
		{
			name:       "sed range quotes stay attached to sed",
			tool:       "Bash",
			input:      "sed -n '90,170p' work/puter/AGENTS.md && sed -n '1,220p' work/puter/doc/self-hosting.md",
			wantSubstr: []string{"sed -n '90,170p'", "AGENTS.md"},
			notEqual:   []string{"90,170p", "1,220p"},
		},
		{
			name:       "rg -g glob is not the whole command",
			tool:       "Bash",
			input:      "rg --files work/puter -g 'AGENTS.md' -g 'SECURITY.md' -g 'package.json'",
			wantSubstr: []string{"rg --files work/puter", "AGENTS.md"},
			notEqual:   []string{"AGENTS.md", "SECURITY.md"},
		},
		{
			name:       "jq object is not the whole command",
			tool:       "Bash",
			input:      "bash tools/github.sh api repos/HeyPuter/puter --jq '{default_branch,size,pushed_at,archived,open_issues_count}'",
			wantSubstr: []string{"tools/github.sh", "HeyPuter/puter"},
			notEqual:   []string{"{default_branch,size,pushed_at,archived,open_issues_count}"},
		},
		{
			name:       "node && rg quoted globs",
			tool:       "Bash",
			input:      "node --version && npm --version && rg --files .cache -g 'node' -g 'npm' -g '*node*.tar*'",
			wantSubstr: []string{"node --version", "npm --version"},
			notEqual:   []string{"node", "npm"},
		},
		{
			name:       "git user.email is redacted",
			tool:       "Bash",
			input:      "git -c user.name=sen-ye -c user.email=dev@example.com commit -F msg.txt",
			wantSubstr: []string{"git -c user.name=sen-ye", "[email]"},
			notContain: []string{"dev@example.com"},
		},
		{
			name:       "shell builtin command -v is kept",
			tool:       "Bash",
			input:      "command -v gh",
			wantSubstr: []string{"command -v gh"},
			notEqual:   []string{"-v gh", "gh"},
		},
		{
			name:       "structured bash json still unwraps command",
			tool:       "Bash",
			input:      `{"command":"python3 - <<'PY'\nprint(1)\nPY"}`,
			wantSubstr: []string{"python3 - <<'PY'", "print(1)"},
			notEqual:   []string{"PY", "command"},
		},
		{
			name:       "prose run-command summary still strips the prefix",
			tool:       "Bash",
			input:      "Run command python3 - <<'PY'",
			wantSubstr: []string{"python3 - <<'PY'"},
			notEqual:   []string{"PY"},
		},
		{
			name:       "read tool still extracts a quoted path",
			tool:       "Read",
			input:      `Read file "/tmp/only-this.txt" please`,
			wantSubstr: []string{"/tmp/only-this.txt"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			display := buildToolDisplay(tt.tool, tt.input)
			if strings.TrimSpace(display.Detail) == "" {
				t.Fatalf("empty detail for %q", tt.input)
			}
			for _, want := range tt.wantSubstr {
				if !strings.Contains(display.Detail, want) {
					t.Fatalf("detail %q missing %q", display.Detail, want)
				}
			}
			for _, bad := range tt.notEqual {
				if display.Detail == bad {
					t.Fatalf("detail collapsed to %q", bad)
				}
			}
			for _, bad := range tt.notContain {
				if strings.Contains(display.Detail, bad) {
					t.Fatalf("detail leaked %q: %q", bad, display.Detail)
				}
			}

			if strings.EqualFold(tt.tool, "Bash") {
				card := formatProgressToolInput(tt.tool, display.Detail)
				if !strings.Contains(card, "```bash") {
					t.Fatalf("progress card missing bash fence: %q", card)
				}
				for _, want := range tt.wantSubstr {
					if !strings.Contains(card, want) {
						t.Fatalf("progress card missing %q: %q", want, card)
					}
				}
				for _, bad := range tt.notContain {
					if strings.Contains(card, bad) {
						t.Fatalf("progress card leaked %q: %q", bad, card)
					}
				}
			}
		})
	}
}

func TestBuildToolDisplay_MoneyMakerRolloutCommands(t *testing.T) {
	paths := moneyMakerRolloutPaths()
	if len(paths) == 0 {
		t.Skip("money-maker Codex rollouts not present on this machine")
	}

	var scripts []string
	seen := map[string]bool{}
	for _, path := range paths {
		got, err := loadRolloutBashScripts(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		for _, script := range got {
			if seen[script] {
				continue
			}
			seen[script] = true
			scripts = append(scripts, script)
		}
	}
	if len(scripts) == 0 {
		t.Fatal("rollouts exist but contained no CommandExecution scripts")
	}

	var collapsed, leakedEmail, missingToken int
	for _, script := range scripts {
		display := buildToolDisplay("Bash", script)
		first := firstNonEmptyLine(script)
		if first == "" {
			continue
		}
		quoted := extractFirstQuotedText(first)
		if quoted != "" && quoted != first && display.Detail == quoted {
			collapsed++
			if collapsed <= 5 {
				t.Errorf("collapsed to quoted fragment %q from %q", quoted, first)
			}
		}
		token := firstShellToken(first)
		if token != "" && !strings.Contains(display.Detail, token) {
			missingToken++
			if missingToken <= 5 {
				t.Errorf("detail dropped command token %q; first line %q detail %q", token, first, display.Detail)
			}
		}
		if emails := emailAddrRe.FindAllString(script, -1); len(emails) > 0 {
			for _, email := range emails {
				if strings.Contains(display.Detail, email) {
					leakedEmail++
					if leakedEmail <= 5 {
						t.Errorf("tool card leaked an email from %q", first)
					}
				}
			}
			if !strings.Contains(display.Detail, "[email]") {
				t.Errorf("expected [email] redaction in %q", first)
			}
		}

		elem := renderProgressEntryElement(core.ProgressCardEntry{
			Kind: core.ProgressEntryToolUse,
			Tool: "Bash",
			Text: display.Detail,
		}, "zh")
		text, _ := elem["text"].(map[string]any)
		content, _ := text["content"].(string)
		if quoted != "" && quoted != first && strings.TrimSpace(content) != "" {
			// Card title/body must still mention the real command token.
			if token != "" && !strings.Contains(content, token) {
				t.Errorf("progress element dropped %q from %q", token, first)
			}
		}
	}

	t.Logf("checked %d unique money-maker commands from %d rollouts", len(scripts), len(paths))
	if collapsed > 0 || leakedEmail > 0 || missingToken > 0 {
		t.Fatalf("money-maker display regressions: collapsed=%d leakedEmail=%d missingToken=%d", collapsed, leakedEmail, missingToken)
	}
}

func moneyMakerRolloutPaths() []string {
	parent := "/home/tiger/.codex/sessions/2026/09/08/rollout-2026-09-08T11-25-44-01a07f0c-dc92-7822-a584-ceb88d86fa35.jsonl"
	var paths []string
	if _, err := os.Stat(parent); err == nil {
		paths = append(paths, parent)
	}
	matches, _ := filepath.Glob("/home/tiger/.codex/sessions/2026/09/08/rollout-*01a07ffd*.jsonl")
	paths = append(paths, matches...)
	matches, _ = filepath.Glob("/home/tiger/.codex/sessions/2026/09/08/rollout-*01a08003*.jsonl")
	paths = append(paths, matches...)
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func loadRolloutBashScripts(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var scripts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec["type"] != "event_msg" {
			continue
		}
		payload, _ := rec["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		pt, _ := payload["type"].(string)
		if pt != "item_started" && pt != "item_completed" {
			continue
		}
		item, _ := payload["item"].(map[string]any)
		if item == nil {
			continue
		}
		it, _ := item["type"].(string)
		if it != "CommandExecution" && it != "commandExecution" {
			continue
		}
		if script := unwrapRolloutCommand(item["command"]); script != "" {
			scripts = append(scripts, script)
		}
	}
	return scripts, sc.Err()
}

func unwrapRolloutCommand(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			parts = append(parts, s)
		}
		if len(parts) >= 3 {
			base := filepath.Base(parts[0])
			if (base == "bash" || base == "sh") && (parts[1] == "-c" || parts[1] == "-lc") {
				return parts[2]
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func firstShellToken(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	for _, sep := range []string{" ", "\t", ";", "&", "|"} {
		if i := strings.IndexAny(command, sep); i > 0 {
			return command[:i]
		}
	}
	return command
}
