# Cursor progress display — sim doc incident

The Feishu topic `cursor gemini，sim doc` exposed two separate display problems. The captured 41-reply snapshot contained 26 tool cards and 10 thinking cards. Cursor had inherited `progress_style=legacy`; the existing Codex and Claude bots use `card`. Additionally, Cursor's protobuf JSON supplies Glob arguments as `globPattern` / `targetDirectory`, while the adapter only checked `pattern`, so the fallback printed the entire JSON object.

The underlying Cursor records contain structured tool calls. The observed English thinking text comes from the model; this incident did not show tool-call syntax being generated as ordinary assistant prose.

## Change

The adapter recognizes camelCase, snake_case and legacy Glob fields and renders a readable search path. Grep includes its target directory when supplied. Read/Edit/Write path aliases, List, Search and WebFetch inputs are summarized consistently. Shell commands retain their original quoting and newlines.

The private candidate config adds only the following setting to the existing Cursor bot's Feishu options:

```toml
[projects.platforms.options]
progress_style = "card"
```

It reuses the established progress-card implementation. Existing configuration and the running conversation have not been changed; deployment awaits an explicit user command. When deploying, compare the saved source config hash and reapply only this field to the latest config if another setting has changed meanwhile.

## Actual-message validation

- Seven actual Glob argument objects were extracted from the affected Feishu messages into `agent/cursor/testdata/sim_doc_glob.json`. Their regression test fails on d7213b07 and passes with this fix.
- The native executable was `/home/tiger/.local/bin/agent-us`, using Cursor CLI 2026.10.22-27dcf5f and model `gemini-4-argon-high`. No standalone Google Gemini CLI was used.
- An isolated read-only run requested Glob for `*.md` and Grep for `RoboDojo`, then returned `CURSOR_DISPLAY_OK`. It completed successfully in 32.27 seconds.
- `tests/release_local/turn_contract/testdata/cursor_gemini_tools.jsonl` retains its actual stream event shapes, with workspace path/session identity normalized and unrelated startup metadata removed. The replay uses the real Cursor adapter and core engine; only the CLI subprocess and messaging transport are replaced.
- Under legacy style the replay produces separate thinking/tool messages. Under card style it produces exactly one progress card and one final reply; Glob/Grep summaries are readable and the progress card reaches the completed state.
- The candidate configuration was parsed by cc-connect and instantiated by the actual Feishu factory; its progress style is `card`, with the original three projects preserved.
- Full Go tests, Cursor/turn-contract/Feishu race tests, go vet and full build pass. Raw chat history and credentials remain outside the commit.
