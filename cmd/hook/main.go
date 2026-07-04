// Command hook is invoked once per Claude Code hook event (one OS process
// per invocation; see .claude/settings.json "command" hooks). It reads the
// event JSON from stdin, classifies it as opening, closing, or being a
// standalone (instant) span, and updates internal/store accordingly.
//
// Hooks must never block or fail the user's Claude Code session: every
// error is logged to stderr and swallowed, exiting 0.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/papikayo/observability-code/internal/config"
	"github.com/papikayo/observability-code/internal/hookevent"
	"github.com/papikayo/observability-code/internal/idgen"
	"github.com/papikayo/observability-code/internal/store"
	"github.com/papikayo/observability-code/internal/tracer"
	"github.com/papikayo/observability-code/internal/transcript"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "observability-code hook error:", err)
	}
	os.Exit(0)
}

func run() error {
	ev, err := hookevent.Parse(os.Stdin)
	if err != nil {
		return fmt.Errorf("parse stdin: %w", err)
	}

	sessionID := ev.SessionID()
	if sessionID == "" {
		return nil
	}

	path, err := config.DBPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	st, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	traceID := idgen.TraceID(sessionID)
	rootSpanID := idgen.SpanID(sessionID + ":root")

	if _, err := st.EnsureSession(sessionID, ev.Cwd(), ev.Source(), rootSpanID.String(), time.Now()); err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}

	tp := tracer.NewProvider(st, sessionID)
	defer tp.Shutdown(context.Background())

	h := &handler{
		ev:         ev,
		st:         st,
		tr:         tp.Tracer("observability-code/hook"),
		sessionID:  sessionID,
		traceID:    traceID,
		rootSpanID: rootSpanID,
	}
	return h.dispatch()
}

type handler struct {
	ev         *hookevent.Event
	st         *store.Store
	tr         trace.Tracer
	sessionID  string
	traceID    trace.TraceID
	rootSpanID trace.SpanID
}

func (h *handler) dispatch() error {
	switch h.ev.HookEventName() {
	case "SessionStart":
		return h.start("session", "session", "__session__", map[string]any{
			"cwd": h.ev.Cwd(), "source": h.ev.Source(),
		}, h.rootSpanID)

	case "SessionEnd":
		return h.end("__session__", map[string]any{"reason": h.ev.Reason()}, false, "")

	case "UserPromptSubmit":
		return h.start("prompt", "prompt", "__turn__", map[string]any{
			"prompt": h.ev.Prompt(), "permission_mode": h.ev.PermissionMode(),
		}, trace.SpanID{})

	case "Stop":
		return h.end("__turn__", map[string]any{"stop_reason": h.ev.StopReason()}, false, "")

	case "StopFailure":
		return h.end("__turn__", map[string]any{"error_type": h.ev.ErrorType()}, true, h.ev.ErrorType())

	case "PreToolUse":
		return h.start("tool", h.ev.ToolName(), "tool:"+h.ev.ToolName(), map[string]any{
			"tool_name": h.ev.ToolName(), "tool_input": h.ev.ToolInputJSON(), "permission_mode": h.ev.PermissionMode(),
		}, trace.SpanID{})

	case "PostToolUse":
		return h.end("tool:"+h.ev.ToolName(), map[string]any{
			"tool_response": h.ev.ToolResponseJSON(), "tool_use_id": h.ev.ToolUseID(), "succeeded": h.ev.ToolUseSucceeded(),
		}, false, "")

	case "PostToolUseFailure":
		return h.end("tool:"+h.ev.ToolName(), map[string]any{
			"tool_response": h.ev.ToolResponseJSON(), "tool_use_id": h.ev.ToolUseID(), "succeeded": false,
		}, true, "tool call failed")

	case "PostToolBatch":
		return h.endBatch()

	case "SubagentStart":
		return h.start("agent", agentName(h.ev.AgentType()), "agent:"+h.ev.AgentType(), map[string]any{
			"agent_type": h.ev.AgentType(), "agent_id": h.ev.AgentID(),
		}, trace.SpanID{})

	case "SubagentStop":
		return h.end("agent:"+h.ev.AgentType(), map[string]any{"stop_reason": h.ev.StopReason()}, false, "")

	case "PreCompact":
		return h.start("compact", "compact", "__compact__", map[string]any{"trigger": h.ev.Trigger()}, trace.SpanID{})

	case "PostCompact":
		return h.end("__compact__", map[string]any{"trigger": h.ev.Trigger()}, false, "")

	default:
		// Every other known-or-future event (Notification, InstructionsLoaded,
		// ConfigChange, CwdChanged, TaskCreated/Completed, PermissionRequest/
		// Denied, Elicitation*, FileChanged, ...) becomes a zero-duration span
		// carrying the full raw payload, so nothing is silently dropped even
		// if this switch hasn't been special-cased for it yet.
		name := h.ev.HookEventName()
		if name == "" {
			name = "unknown"
		}
		return h.instant("event", name, map[string]any{"raw": json.RawMessage(h.ev.AttributesJSON())})
	}
}

func agentName(agentType string) string {
	if agentType == "" {
		return "agent"
	}
	return agentType
}

// start opens a pending span: recorded now, exported later once its
// matching end event closes it.
func (h *handler) start(kind, name, matchKey string, attrs map[string]any, explicitSpanID trace.SpanID) error {
	var parentSpanID trace.SpanID
	if kind != "session" {
		parent, hasParent, err := h.st.PeekParent(h.sessionID)
		if err != nil {
			return err
		}
		if hasParent {
			parentSpanID = mustSpanID(parent)
		} else {
			parentSpanID = h.rootSpanID
		}
	}

	seq, err := h.st.NextSeq(h.sessionID)
	if err != nil {
		return err
	}

	spanID := explicitSpanID
	if !spanID.IsValid() {
		spanID = idgen.SpanID(fmt.Sprintf("%s:%d", h.sessionID, seq))
	}

	return h.st.PushPending(store.PendingSpan{
		SessionID:    h.sessionID,
		Seq:          seq,
		SpanID:       spanID.String(),
		ParentSpanID: spanIDOrEmpty(parentSpanID),
		Kind:         kind,
		Name:         name,
		MatchKey:     matchKey,
		StartTime:    time.Now(),
		StartAttrs:   attrs,
	})
}

// end pops the pending span matching matchKey and exports it as a finished
// OTel span with its real historical start/end timestamps.
func (h *handler) end(matchKey string, endAttrs map[string]any, isError bool, errReason string) error {
	p, ok, err := h.st.PopPending(h.sessionID, matchKey)
	if err != nil {
		return err
	}
	if !ok {
		return nil // no matching start was ever observed; nothing to close
	}

	var transcriptPath, outputText string
	if path := h.ev.TranscriptPath(); path != "" && (matchKey == "__turn__" || matchKey == "__session__") {
		transcriptPath = path
		if msg := latestTurnMessage(path); msg != nil {
			endAttrs["model"] = msg.Model
			endAttrs["usage.input_tokens"] = msg.Usage.InputTokens
			endAttrs["usage.output_tokens"] = msg.Usage.OutputTokens
			endAttrs["usage.cache_creation_input_tokens"] = msg.Usage.CacheCreationInputTokens
			endAttrs["usage.cache_read_input_tokens"] = msg.Usage.CacheReadInputTokens
			outputText = msg.Text
			if text := capString(msg.Text, 40000); text != "" {
				endAttrs["output"] = text
			}
		}
	}

	attrs := mergeAttrs(p.StartAttrs, endAttrs)

	ctx := tracer.WithSeed(context.Background(), h.traceID, mustSpanID(p.SpanID))
	ctx = tracer.WithParent(ctx, h.traceID, mustSpanID(p.ParentSpanID))

	kvs := append([]attribute.KeyValue{attribute.String(tracer.KindAttrKey, p.Kind)}, attrsToKVs(attrs)...)
	_, span := h.tr.Start(ctx, p.Name, trace.WithTimestamp(p.StartTime), trace.WithAttributes(kvs...))
	if isError {
		span.SetStatus(codes.Error, errReason)
	}
	span.End(trace.WithTimestamp(time.Now()))

	if matchKey == "__turn__" && transcriptPath != "" {
		h.emitMessageSpans(p, transcriptPath, outputText)
	}

	if matchKey == "__session__" {
		return h.st.CloseSession(h.sessionID, time.Now(), h.ev.Reason())
	}
	return nil
}

// latestTurnMessage reads the turn's final assistant reply (its text + usage)
// from the transcript. Claude Code fires the Stop hook and writes that final
// message nearly simultaneously, and the hook often wins the race — so the
// transcript can momentarily end on a "tool_use" message with the real reply
// not yet flushed. We poll briefly for a terminal stop_reason (end_turn, …),
// which marks the final reply as present; without this the turn's recorded
// output is the previous, intermediate message instead of the actual reply.
func latestTurnMessage(path string) *transcript.LatestAssistantMessage {
	const tries = 12
	var msg *transcript.LatestAssistantMessage
	for i := range tries {
		if m, err := transcript.LatestUsage(path); err == nil {
			msg = m
			if m.StopReason != "tool_use" { // final reply is flushed
				break
			}
		}
		if i < tries-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return msg
}

// emitMessageSpans recovers a just-closed turn's assistant narration from the
// transcript and exports one instant child span per text block, so the turn's
// waterfall shows what Claude said between tool calls. That text lives only in
// the transcript, never in a hook event, so it is invisible without this. The
// block equal to the turn's final output is skipped — it's already surfaced as
// the turn's output. Best-effort: any error just means no message spans.
func (h *handler) emitMessageSpans(prompt *store.PendingSpan, path, outputText string) {
	msgs, err := transcript.AssistantMessages(path, prompt.StartTime, time.Now())
	if err != nil {
		return
	}
	skip := strings.TrimSpace(outputText)
	for i, m := range msgs {
		if skip != "" && strings.TrimSpace(m.Text) == skip {
			continue
		}
		spanID := idgen.SpanID(fmt.Sprintf("%s:msg:%s:%d", h.sessionID, prompt.SpanID, i))
		ctx := tracer.WithSeed(context.Background(), h.traceID, spanID)
		ctx = tracer.WithParent(ctx, h.traceID, mustSpanID(prompt.SpanID))
		kvs := []attribute.KeyValue{
			attribute.String(tracer.KindAttrKey, "message"),
			attribute.String("text", capString(m.Text, 40000)),
		}
		_, span := h.tr.Start(ctx, messageSnippet(m.Text),
			trace.WithTimestamp(m.Time), trace.WithAttributes(kvs...))
		span.End(trace.WithTimestamp(m.Time))
	}
}

// messageSnippet condenses an assistant text block to a single-line label for
// the waterfall row (the full text is kept in the span's "text" attribute).
func messageSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const n = 100
	if len(s) <= n {
		return s
	}
	for n2 := n; n2 > 0; n2-- {
		if utf8.RuneStart(s[n2]) {
			return s[:n2] + "…"
		}
	}
	return s[:n] + "…"
}

// endBatch closes every tool span reported in a PostToolBatch event
// (parallel tool calls resolving together).
func (h *handler) endBatch() error {
	for _, tu := range h.ev.ToolUses() {
		toolName, _ := tu["tool_name"].(string)
		toolUseID, _ := tu["tool_use_id"].(string)
		succeeded, _ := tu["tool_use_succeeded"].(bool)
		respJSON, _ := json.Marshal(tu["tool_response"])

		if err := h.end("tool:"+toolName, map[string]any{
			"tool_response": string(respJSON),
			"tool_use_id":   toolUseID,
			"succeeded":     succeeded,
		}, !succeeded, ""); err != nil {
			return err
		}
	}
	return nil
}

// instant records a zero-duration span for one-shot events that have no
// separate start/end pair.
func (h *handler) instant(kind, name string, attrs map[string]any) error {
	parent, hasParent, err := h.st.PeekParent(h.sessionID)
	if err != nil {
		return err
	}
	parentSpanID := h.rootSpanID
	if hasParent {
		parentSpanID = mustSpanID(parent)
	}

	seq, err := h.st.NextSeq(h.sessionID)
	if err != nil {
		return err
	}
	spanID := idgen.SpanID(fmt.Sprintf("%s:%d", h.sessionID, seq))

	ctx := tracer.WithSeed(context.Background(), h.traceID, spanID)
	ctx = tracer.WithParent(ctx, h.traceID, parentSpanID)

	kvs := append([]attribute.KeyValue{attribute.String(tracer.KindAttrKey, kind)}, attrsToKVs(attrs)...)
	now := time.Now()
	_, span := h.tr.Start(ctx, name, trace.WithTimestamp(now), trace.WithAttributes(kvs...))
	span.End(trace.WithTimestamp(now))
	return nil
}

func mustSpanID(s string) trace.SpanID {
	var id trace.SpanID
	if s == "" {
		return id
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id
	}
	copy(id[:], b)
	return id
}

func spanIDOrEmpty(id trace.SpanID) string {
	if !id.IsValid() {
		return ""
	}
	return id.String()
}

// capString truncates s to at most n bytes (on a rune boundary) so a very
// long assistant reply doesn't bloat the span row; the UI notes truncation.
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n…[truncated]"
}

func mergeAttrs(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

func attrsToKVs(attrs map[string]any) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		switch val := v.(type) {
		case string:
			kvs = append(kvs, attribute.String(k, val))
		case bool:
			kvs = append(kvs, attribute.Bool(k, val))
		case int:
			kvs = append(kvs, attribute.Int(k, val))
		case int64:
			kvs = append(kvs, attribute.Int64(k, val))
		case float64:
			kvs = append(kvs, attribute.Float64(k, val))
		case json.RawMessage:
			kvs = append(kvs, attribute.String(k, string(val)))
		default:
			b, _ := json.Marshal(v)
			kvs = append(kvs, attribute.String(k, string(b)))
		}
	}
	return kvs
}
