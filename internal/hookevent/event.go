// Package hookevent parses the JSON payload Claude Code sends on the stdin
// of every hook command invocation.
package hookevent

import (
	"encoding/json"
	"io"
)

// Event wraps the raw decoded JSON payload for one hook invocation.
// Claude Code's hook schema varies by event and gains fields over versions,
// so we keep the raw map and expose typed accessors for the fields we use
// rather than defining a brittle struct per event.
type Event struct {
	Raw map[string]any
}

func Parse(r io.Reader) (*Event, error) {
	var raw map[string]any
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}
	return &Event{Raw: raw}, nil
}

func (e *Event) str(key string) string {
	v, _ := e.Raw[key].(string)
	return v
}

func (e *Event) boolean(key string) bool {
	v, _ := e.Raw[key].(bool)
	return v
}

// Common fields present on (most) hook events.
func (e *Event) SessionID() string      { return e.str("session_id") }
func (e *Event) PromptID() string       { return e.str("prompt_id") }
func (e *Event) TranscriptPath() string { return e.str("transcript_path") }
func (e *Event) Cwd() string            { return e.str("cwd") }
func (e *Event) PermissionMode() string { return e.str("permission_mode") }
func (e *Event) HookEventName() string  { return e.str("hook_event_name") }
func (e *Event) AgentID() string        { return e.str("agent_id") }
func (e *Event) AgentType() string      { return e.str("agent_type") }

// Tool events: PreToolUse, PostToolUse, PostToolUseFailure, PermissionRequest, PermissionDenied.
func (e *Event) ToolName() string  { return e.str("tool_name") }
func (e *Event) ToolUseID() string { return e.str("tool_use_id") }

func (e *Event) ToolInputJSON() string {
	b, _ := json.Marshal(e.Raw["tool_input"])
	return string(b)
}

func (e *Event) ToolResponseJSON() string {
	b, _ := json.Marshal(e.Raw["tool_response"])
	return string(b)
}

func (e *Event) ToolUseSucceeded() bool { return e.boolean("tool_use_succeeded") }

// PostToolBatch: parallel tool call results.
func (e *Event) ToolUses() []map[string]any {
	raw, ok := e.Raw["tool_uses"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// UserPromptSubmit.
func (e *Event) Prompt() string { return e.str("prompt") }

// SessionStart, ConfigChange.
func (e *Event) Source() string { return e.str("source") }

// SessionEnd, PreToolUse deny reasons, etc.
func (e *Event) Reason() string { return e.str("reason") }

// Stop, SubagentStop.
func (e *Event) StopReason() string { return e.str("stop_reason") }

// StopFailure.
func (e *Event) ErrorType() string { return e.str("error_type") }

// PreCompact, PostCompact, Setup.
func (e *Event) Trigger() string { return e.str("trigger") }

// Notification.
func (e *Event) Message() string          { return e.str("message") }
func (e *Event) NotificationType() string { return e.str("notification_type") }

// InstructionsLoaded.
func (e *Event) FilePath() string { return e.str("file_path") }

// TaskCreated, TaskCompleted.
func (e *Event) TaskName() string        { return e.str("task_name") }
func (e *Event) TaskDescription() string { return e.str("task_description") }

// AttributesJSON marshals the full raw payload, for storage as a catch-all
// attribute blob on spans/events so no field is silently dropped.
func (e *Event) AttributesJSON() string {
	b, _ := json.Marshal(e.Raw)
	return string(b)
}
