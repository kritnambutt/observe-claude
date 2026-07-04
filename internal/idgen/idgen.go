// Package idgen derives deterministic OTel trace/span IDs from Claude Code
// identifiers (session_id, and a per-session sequence number). Determinism
// matters because a single Claude Code session is observed across many
// short-lived hook processes with no shared memory between them: the same
// session_id must always map to the same trace ID no matter which process
// computes it.
package idgen

import (
	"crypto/sha256"

	"go.opentelemetry.io/otel/trace"
)

// TraceID derives a stable OTel trace ID for a Claude Code session.
func TraceID(sessionID string) trace.TraceID {
	h := sha256.Sum256([]byte("observability-code/session:" + sessionID))
	var id trace.TraceID
	copy(id[:], h[:16])
	return id
}

// SpanID derives a stable OTel span ID from an arbitrary seed string.
// Callers pass "<session_id>:<seq>" where seq is a monotonically increasing
// counter scoped to the session (see store.Store.NextSeq), so repeated calls
// for the same logical span (e.g. computed once at start, once at end)
// yield the same span ID.
func SpanID(seed string) trace.SpanID {
	h := sha256.Sum256([]byte("observability-code/span:" + seed))
	var id trace.SpanID
	copy(id[:], h[:8])
	return id
}
