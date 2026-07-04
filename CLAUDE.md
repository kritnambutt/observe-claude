# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Local, self-hosted observability for Claude Code itself. It captures what a
Claude Code session actually did — prompts, tool calls, subagent
invocations, files touched, model + token usage per turn, notifications,
compactions — via Claude Code's hook system, models it as OpenTelemetry
traces, stores it in SQLite, and renders it through a small built-in web UI
(a much lighter-weight, single-binary cousin of SigNoz/Jaeger).

There are two binaries:
- `cmd/hook` — invoked once per Claude Code hook event (see Architecture).
- `cmd/server` — long-running local web UI reading the same SQLite DB.

## Commands

```sh
go build ./...              # build everything
go vet ./...                # static checks
gofmt -l .                  # formatting check (should print nothing)
go mod tidy                 # after adding/removing an import

go build -o bin/hook ./cmd/hook
go build -o bin/server ./cmd/server

OBS_DB_PATH=/tmp/x.db go run ./cmd/server   # run the UI against a specific db
```

There is no automated test suite yet. To manually exercise `cmd/hook`
end-to-end without a live Claude Code session, pipe a hook JSON payload into
the built binary (see `scripts/install-hooks.sh`'s header comment for the
full event list, and `internal/hookevent/event.go` for field names):

```sh
export OBS_DB_PATH=/tmp/smoke.db
echo '{"session_id":"s1","hook_event_name":"SessionStart","source":"startup","cwd":"/tmp"}' | ./bin/hook
echo '{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hi"}' | ./bin/hook
echo '{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}' | ./bin/hook
echo '{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"ok","tool_use_succeeded":true}' | ./bin/hook
echo '{"session_id":"s1","hook_event_name":"Stop","stop_reason":"end_turn"}' | ./bin/hook
echo '{"session_id":"s1","hook_event_name":"SessionEnd","reason":"user_exit"}' | ./bin/hook
sqlite3 /tmp/smoke.db "SELECT name, kind, status, parent_span_id FROM spans;"
```

Note: use `printf '%s' "$json" | ./bin/hook`, not `echo`, if the payload
comes from a shell variable containing escaped characters — some shells'
`echo` interprets `\n` etc. before it reaches stdin, which breaks JSON
parsing (this is a shell footgun, not a hook bug).

To register the hooks against a real Claude Code install:
`scripts/install-hooks.sh --global` (all projects) or `--project [dir]`
(one project). This only appends to `settings.json`'s hook arrays — it
never touches existing entries from other tools — and it backs up
`settings.json` first. It is never run automatically; it's a deliberate,
explicit step because it edits shared Claude Code config.

## Architecture

The core design problem: Claude Code invokes a **fresh, short-lived OS
process for every hook event** (`PreToolUse`, `PostToolUse`, `SessionStart`,
...), with no memory shared between invocations, yet a "tool call" or
"session" as a concept spans many of these events over time. Everything in
`cmd/hook` and its dependencies exists to solve that.

The pipeline, per hook invocation (`cmd/hook/main.go`):

1. **Parse** — `internal/hookevent` decodes the event JSON from stdin into
   a raw map with typed accessors (kept as a map, not a strict struct,
   because the hook schema varies per event and gains fields across Claude
   Code versions).
2. **Derive deterministic IDs** — `internal/idgen` hashes Claude's
   `session_id` (+ a per-session sequence counter from `store.NextSeq`)
   into OTel trace/span IDs. This is what lets independent processes agree
   on the same span ID for the same logical span without talking to each
   other.
3. **Classify** — `cmd/hook/main.go`'s `dispatch()` maps each
   `hook_event_name` to one of three actions:
   - **start** (`PreToolUse`, `UserPromptSubmit`, `SubagentStart`,
     `SessionStart`, `PreCompact`) — opens a span.
   - **end** (`PostToolUse[Failure]`, `Stop[Failure]`, `SubagentStop`,
     `SessionEnd`, `PostCompact`) — closes the matching open span.
   - **instant** (everything else — `Notification`, `ConfigChange`,
     `TaskCreated`, ...) — a zero-duration span, attributes = the full raw
     payload, so nothing recognized by Claude Code but not yet
     special-cased here is silently dropped.
4. **Persist the open span** (`internal/store`, table `pending_spans`) —
   an open span is just a SQLite row (parent, kind, name, start time, start
   attributes) pushed onto a stack keyed by `session_id`. The process then
   exits. Nesting (tool calls inside a subagent inside a turn) falls out
   naturally from treating this as a real stack: `PeekParent` returns
   whatever's currently on top.
5. **On the closing event**, `PopPending` finds the matching pending span
   (by a `match_key` such as `"tool:Bash"`, falling back to LIFO if no
   exact match), and **only then** is a real span constructed through the
   OpenTelemetry Go SDK (`internal/tracer`): `tracer.Start()` immediately
   followed by `span.End()`, but both calls pass `trace.WithTimestamp()`
   with the actual historical start/end times pulled from the pending row.
   This produces a genuine SDK `ReadOnlySpan`, just assembled later than it
   "started" — the only way to get correct SDK-produced spans out of a
   lifecycle that spans multiple OS processes.
6. A custom `sdktrace.SpanExporter` (also in `internal/tracer`) writes that
   finished span straight into SQLite's `spans` table. No collector, no
   OTLP network hop — `cmd/server` reads this table directly.

**Token/model enrichment** (`internal/transcript`): hook payloads don't
carry token usage or model name, but every payload includes
`transcript_path` (the session's JSONL transcript, which Claude Code
already writes). On turn/session-closing events, `transcript.LatestUsage`
reverse-scans that file in growing chunks (starting at 64KB, capped at 4MB)
for the most recent `type:"assistant"` line's `message.model` /
`message.usage`, and attaches it to the span. This is a one-shot read at
hook-close time, not a background tailer.

**Span kind hierarchy**: `session` (root, one per Claude Code session) →
`prompt` (one per user turn) → `tool` / `agent` / `compact` (nested inside
a turn, or inside an `agent` span if a subagent is currently active) →
`event` (instant spans for everything else).

`cmd/server` + `internal/web` are the read side: `internal/web/handlers.go`
loads a session's spans, rebuilds the parent/child tree
(`groupSpans`/`buildWaterfall`), and renders it as a waterfall via
`html/template` (`internal/web/templates/*.html`, embedded with
`go:embed`) — no JS framework, no CDN, styled to look like a minimal
SigNoz/Jaeger.

`internal/config` is the single source of truth for `OBS_DB_PATH` so
`cmd/hook` and `cmd/server` can't drift on where the database lives.

### Known simplifications (see README.md for the full list)

- Pairing `PreToolUse`→`PostToolUse` is by `match_key` + LIFO fallback, not
  a stable ID, because `PreToolUse` payloads don't carry `tool_use_id`
  (only the closing events do). Breaks under heavy same-tool parallelism.
- One OTel trace per Claude Code `session_id`; subagents nest as a span,
  not a separate trace.
- Spans are OTel-*shaped* but exported straight to SQLite, not through a
  real OTel Collector/OTLP — intentional per this project's scope.
