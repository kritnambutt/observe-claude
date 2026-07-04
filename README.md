# observability-code

Local, self-hosted observability for Claude Code sessions — what got
prompted, which tools/agents/skills fired, what files were touched, what
model and how many tokens were used per turn — captured via Claude Code
hooks, modeled as OpenTelemetry traces, stored in SQLite, and browsed
through a small built-in web UI (a much lighter-weight cousin of SigNoz).

## Install

The whole tool is one cross-platform binary, `observe-claude`.

**macOS (Homebrew):**

```sh
brew install kritnambutt/tap/observe-claude
```

**Windows (Scoop):**

```powershell
scoop bucket add observe https://github.com/kritnambutt/scoop-bucket
scoop install observe-claude
```

**Any OS (prebuilt binary):** download the archive for your platform from the
[latest release](https://github.com/kritnambutt/observe-claude/releases) and
put `observe-claude` on your `PATH`.

**From source (needs Go):** `go build -o observe-claude ./cmd/observe-claude`

## Quick start

```sh
observe-claude init          # register the hook with Claude Code (all projects)
# ...start or restart a Claude Code session and do some work...
observe-claude serve --open  # browse your sessions (http://127.0.0.1:4790)
```

`observe-claude init` (use `--project [dir]` to scope to one project) only
*appends* a hook group to each event's array in Claude Code's `settings.json`
— it never removes or overwrites hooks other tools already registered there,
and it backs `settings.json` up first. It also drops a stable copy of the
binary in `~/.observability-code/bin/` and points the hook at that, so the
registration keeps working across upgrades.

<details>
<summary>Subcommands</summary>

| Command | What it does |
|---|---|
| `observe-claude init [--global \| --project [dir]]` | register the hook in Claude Code's `settings.json` (`--global` is the default) |
| `observe-claude serve [--addr host:port] [--db path] [--open]` | run the web UI |
| `observe-claude hook` | invoked by Claude Code per event; reads the payload on stdin (you don't run this) |
| `observe-claude version` | print the version |

</details>

## How it works

Claude Code invokes a fresh, short-lived OS process for every hook event
(`PreToolUse`, `PostToolUse`, `SessionStart`, ...), with no memory shared
between invocations. `observe-claude hook` (code in `internal/hookrun`) is
that process. Each invocation:

1. Parses the event JSON from stdin (`internal/hookevent`).
2. Derives deterministic OTel trace/span IDs from Claude's `session_id` and
   a per-session sequence counter (`internal/idgen`), so independent
   processes agree on IDs without coordinating.
3. Classifies the event as **start** (opens a span, e.g. `PreToolUse`),
   **end** (closes the matching open span, e.g. `PostToolUse`), or
   **instant** (a zero-duration span, e.g. `Notification`).
4. Persists open ("pending") spans to SQLite (`internal/store`) until their
   matching close event arrives — this is what lets a span survive across
   process boundaries.
5. Once a span has both a start and end, constructs it through the real
   OpenTelemetry Go SDK (`internal/tracer`) — `tracer.Start()` immediately
   followed by `span.End()`, but with both calls backfilled with the actual
   historical timestamps via `trace.WithTimestamp()`. This produces a
   genuine SDK-exported `ReadOnlySpan` even though the wall-clock call
   happens later than the span's real start.
6. A custom `sdktrace.SpanExporter` writes the finished span straight into
   the `spans` table — no collector, no OTLP network hop.

`observe-claude serve` just reads that SQLite database back out and renders a
session list + a per-session waterfall view (`internal/web`).

The single binary has three jobs (`internal/cli` multiplexes them):
`hook` (above), `serve` (the UI), and `init` (cross-platform registration into
Claude Code's `settings.json`, the Go port of `scripts/install-hooks.sh`).
`cmd/hook` and `cmd/server` remain as thin wrappers so existing scripts and
`go run ./cmd/server` keep working.

### Token/model enrichment

Hook payloads don't carry token usage or model name — those live in the
session transcript. Every hook payload does include `transcript_path`
though, so on turn/session-closing events (`Stop`, `StopFailure`,
`SessionEnd`) `internal/transcript` reverse-scans that file for the most
recent assistant message's `model` and `usage` fields and attaches them to
the span. This is a one-shot read at hook time, not a background tailer.

Claude Code fires the `Stop` hook and writes the turn's final reply to the
transcript nearly simultaneously, and the hook can win that race — so the
transcript momentarily ends on a `tool_use` message with the real reply not
yet flushed. The hook polls briefly for a terminal `stop_reason` (`end_turn`,
…) before reading; without it a turn's recorded output is the previous,
intermediate message rather than the actual final reply.

The same close-time transcript read also recovers the turn's **assistant
narration** — the text Claude emits between tool calls ("Let me look at
X…", "Now I'll edit Y…"). That text exists only in the transcript, never in
a hook event, so without this it's invisible. `transcript.AssistantMessages`
returns every assistant text block within the turn's time window, and each
becomes a `message` span (child of the `prompt` span) timestamped at the
moment Claude wrote it — so the waterfall reads as the real interleaving of
narration and tool calls. The block equal to the final reply is skipped
(it's already shown as the turn's output).

### Span kinds

`session` (whole session) → `prompt` (one user turn) → `tool` / `agent` /
`compact` (nested inside a turn) → `message` (Claude's between-tool
narration, reconstructed from the transcript) / `event` (everything else —
Notification, ConfigChange, TaskCreated, etc. — as zero-duration spans
carrying the full raw hook payload as an attribute, so nothing is silently
dropped even for event types this code hasn't special-cased).

### Web UI (`cmd/server` + `internal/web`)

The read side loads a session's spans, rebuilds the parent/child tree, and
renders it with `html/template` (no JS framework, no CDN) as:

- a **turns table** — one row per user turn with model, duration, tokens,
  tool/agent counts, and status; and
- a per-turn **waterfall** of that turn's spans, positioned on a shared time
  axis.

Each waterfall row is built to be readable without expanding anything:

- a **colored kind badge** (`TOOL`, `EVENT`, `AGENT`, `MESSAGE`, …) so the
  row type reads at a glance;
- an **inline detail** — the file a tool touched, the command Bash ran, the
  notification message — mined from the span's attributes;
- for file-editing tools (`Edit` / `Write` / `MultiEdit`), a **red/green
  diff**. Claude Code puts a `structuredPatch` (unified-diff hunks) in the
  tool response; `parseDiff` turns it into `+N −M` line counts on the row
  plus an expandable hunk view, so you can see exactly which lines changed
  without leaving the trace.

## Known limitations

- **Pairing is LIFO, not ID-based.** `PreToolUse` doesn't carry a
  `tool_use_id` (only the closing events do), so a same-named tool call
  that starts before a previous same-named call closes will pair
  incorrectly. Fine for the common case; not exact under heavy parallel
  tool use.
- **One trace per Claude session.** Subagent spans nest under whatever's
  currently on top of that session's stack; a subagent's own tool calls
  aren't distinguished from the parent's beyond the `agent` span wrapping
  them.
- **No real OTLP export.** Spans are OTel-*shaped* (trace/span IDs,
  attributes, resource) but go straight to SQLite, not through an OTel
  Collector — by design, per the "custom lightweight UI" scope. Feeding a
  real backend (SigNoz, Jaeger, ...) later just means adding an
  `otlptracehttp` exporter alongside the SQLite one in `internal/tracer`.

## Environment variables

| Variable       | Default                                   | Meaning                          |
|----------------|--------------------------------------------|-----------------------------------|
| `OBS_DB_PATH`  | `~/.observability-code/observability.db`  | SQLite database path (shared by `hook` and `server`) |
| `OBS_ADDR`     | `127.0.0.1:4790`                          | `cmd/server` listen address      |
