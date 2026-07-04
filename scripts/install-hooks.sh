#!/usr/bin/env bash
# Registers the `hook` binary against Claude Code's hook events.
#
# This is NOT run automatically by anything in this repo — you run it
# yourself, explicitly, because it edits Claude Code's settings.json.
# It only ever *appends* a new hook group to each event's array; it never
# touches or removes any existing hook configuration (e.g. other tools like
# GitKraken CLI may already occupy every event).
#
# Usage:
#   scripts/install-hooks.sh --global            # ~/.claude/settings.json, observes every project
#   scripts/install-hooks.sh --project [dir]      # <dir>/.claude/settings.json, observes only that project
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK_BIN="$REPO_ROOT/bin/hook"

usage() {
  echo "Usage: $0 --global | --project [dir]" >&2
  exit 1
}

[ "$#" -ge 1 ] || usage

case "$1" in
  --global)
    SETTINGS_PATH="$HOME/.claude/settings.json"
    ;;
  --project)
    PROJECT_DIR="${2:-$PWD}"
    SETTINGS_PATH="$PROJECT_DIR/.claude/settings.json"
    ;;
  *)
    usage
    ;;
esac

command -v jq >/dev/null || { echo "jq is required (brew install jq)" >&2; exit 1; }

echo "Building $HOOK_BIN ..."
mkdir -p "$REPO_ROOT/bin"
(cd "$REPO_ROOT" && go build -o "$HOOK_BIN" ./cmd/hook)

mkdir -p "$(dirname "$SETTINGS_PATH")"
if [ ! -f "$SETTINGS_PATH" ]; then
  echo "{}" > "$SETTINGS_PATH"
fi

BACKUP="$SETTINGS_PATH.bak-$(date +%Y%m%dT%H%M%S)"
cp "$SETTINGS_PATH" "$BACKUP"
echo "Backed up existing settings to $BACKUP"

# Events worth capturing for session observability. Deliberately excludes:
#   - MessageDisplay: fires once per streamed chunk, far too high-frequency
#     to spawn a subprocess for.
#   - Elicitation / ElicitationResult: payload can carry raw user-entered
#     field values (see Claude Code hook docs), including things like
#     passwords for MCP auth flows — not something to persist to disk here.
#   - Setup, TeammateIdle: one-off / multi-agent-fleet specific, low value
#     for single-session observability.
EVENTS=(
  SessionStart SessionEnd
  UserPromptSubmit UserPromptExpansion Stop StopFailure
  PreToolUse PostToolUse PostToolUseFailure PostToolBatch
  SubagentStart SubagentStop
  PreCompact PostCompact
  Notification
  TaskCreated TaskCompleted
  ConfigChange CwdChanged InstructionsLoaded
  PermissionRequest PermissionDenied
  FileChanged WorktreeCreate WorktreeRemove
)

TMP="$(mktemp)"
cp "$SETTINGS_PATH" "$TMP"

added=0
skipped=0
for event in "${EVENTS[@]}"; do
  TMP2="$(mktemp)"
  before="$(jq -c --arg e "$event" '.hooks[$e] // []' "$TMP")"
  jq --arg event "$event" --arg cmd "$HOOK_BIN" '
    (.hooks[$event] // []) as $groups |
    ([$groups[].hooks[]?.command] | index($cmd)) as $idx |
    if $idx != null then .
    else .hooks[$event] = ($groups + [{"matcher": "", "hooks": [{"type": "command", "command": $cmd}]}])
    end
  ' "$TMP" > "$TMP2"
  after="$(jq -c --arg e "$event" '.hooks[$e] // []' "$TMP2")"
  if [ "$before" != "$after" ]; then
    added=$((added + 1))
  else
    skipped=$((skipped + 1))
  fi
  mv "$TMP2" "$TMP"
done

mv "$TMP" "$SETTINGS_PATH"

echo "Done: $added event(s) newly hooked, $skipped already present."
echo "Settings file: $SETTINGS_PATH"
echo "Hook binary:   $HOOK_BIN"
echo ""
echo "DB path defaults to ~/.observability-code/observability.db (override with OBS_DB_PATH)."
echo "Start a new Claude Code session, then run: go run ./cmd/server"
