# Cursor /ps over ACP

The Cursor print adapter starts a new CLI process for each turn. `/ps` and `/btw`
previously called Send again during an active turn, producing `cursor: previous
turn is still running`. The process guard remains necessary to protect chat data.

## Implementation

Cursor now accepts `backend = "acp"` in its existing agent options. It keeps
`type = "cursor"`, the custom CLI command, provider environment, model picker,
workspace settings, skills, and Cursor error classification. The default is
still `print`. The shared protocol implementation and its existing tests moved
from `agent/acp` into `internal/acpclient`; the generic ACP plugin registers a
thin wrapper, so neither agent imports another agent package.

An optional core MessageAppender interface separates `/ps` from starting another
turn. Cursor cancels the old ACP request and waits for its response before
submitting the added instruction in the same session. The new prompt explicitly
keeps the original task. The superseded request emits no terminal task event;
the engine resets its unfinished answer and records the added user instruction.
Obsolete permission requests are invalidated even if their events were queued.
Normal messages still use the existing queue; `/ps` is not silently queued.

ACP model configuration uses the advertised parameterized picker. Existing
Gemini 4 Argon and Grok 4.7 effort/fast aliases translate into their corresponding
config options. Images are sent as ACP image blocks; file references retain the
per-message attachment paths. Consecutive thinking chunks form one summary.

On first resume through ACP, an existing print database is copied using SQLite's
consistent snapshot operation into the ACP store with the same native ID. The
original is retained. Publication is atomic after integrity validation. The Go
SQLite dependency already used by the repository avoids a sqlite3 executable
requirement. ACP history replay is suppressed during load so it cannot fill the
event buffer. Resume failures do not fall back to an empty session. Once a
session has ACP storage, changing the default backend does not route it back to
a stale print copy.

## Verification

- Regression `TestCmdPs_UsesAppenderAndPreservesAttachments` fails against the
  previous engine with the observed send-failed error, and passes with the fix.
- Request cancellation, single completion, long history replay, obsolete
  permissions, attachment forwarding, and idempotent close have regression tests.
- CUJ A9 includes append-and-continue, rejection while idle, and suppression of
  the cancelled draft and a late permission request.
- Real `agent-us` 2026.10.22-27dcf5f with Gemini 4 Argon High: create a print chat,
  migrate and recall a marker, append while streaming, restart, and recall the
  original marker again. All steps passed; native ID and original database bytes
  were preserved. Tests used an isolated CURSOR_CONFIG_DIR.
- Native Grok 4.7 High and XHigh Fast configuration was accepted through ACP.

Logs and reproducible probes are in `.agent-tmp/cursor-ps-20261025/` and
`/opt/tiger/yesen/tmp/cc-connect/cursor-acp-20261025/`. Private test credentials are
outside the repository. Production deployment is a separate step; no bot was
reconfigured or restarted during this change.
