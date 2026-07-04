// Package store persists Claude Code session/span data to a local SQLite
// database. It is the single source of truth shared between the short-lived
// `hook` process (one process per hook invocation, no shared memory) and the
// long-running `server` process that reads it back out for the web UI.
package store

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	session_id   TEXT PRIMARY KEY,
	cwd          TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER,
	end_reason   TEXT,
	next_seq     INTEGER NOT NULL DEFAULT 0,
	root_span_id TEXT NOT NULL
);

-- A span that has started but not yet closed. Popped and turned into a
-- finished row in spans once the matching end/instant event arrives.
-- This is the mechanism that lets a span's start (e.g. PreToolUse, in one
-- OS process) and end (PostToolUse, in a later, unrelated OS process) be
-- joined back together.
CREATE TABLE IF NOT EXISTS pending_spans (
	session_id      TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	span_id         TEXT NOT NULL,
	parent_span_id  TEXT NOT NULL,
	kind            TEXT NOT NULL,
	name            TEXT NOT NULL,
	match_key       TEXT NOT NULL DEFAULT '',
	start_time      INTEGER NOT NULL,
	start_attrs     TEXT NOT NULL DEFAULT '{}',
	PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_pending_session ON pending_spans(session_id);

-- Finished spans, written exactly once each by the OTel span exporter
-- (internal/tracer) once both halves of an event pair are known.
CREATE TABLE IF NOT EXISTS spans (
	span_id        TEXT PRIMARY KEY,
	trace_id       TEXT NOT NULL,
	session_id     TEXT NOT NULL,
	parent_span_id TEXT NOT NULL DEFAULT '',
	name           TEXT NOT NULL,
	kind           TEXT NOT NULL,
	status         TEXT NOT NULL DEFAULT 'ok',
	start_time     INTEGER NOT NULL,
	end_time       INTEGER NOT NULL,
	attributes     TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_spans_session ON spans(session_id);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id);
CREATE INDEX IF NOT EXISTS idx_spans_parent ON spans(parent_span_id);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single writer file is contended by concurrent hook processes
	// (e.g. PostToolBatch fan-out); WAL + a generous busy_timeout lets
	// SQLite itself serialize writes instead of failing with SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=10000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// EnsureSession creates the session row on first sight (typically
// SessionStart, but hooks can race or a session can be observed mid-stream)
// and returns whether it already existed.
func (s *Store) EnsureSession(sessionID, cwd, source, rootSpanID string, startedAt time.Time) (existed bool, err error) {
	row := s.db.QueryRow(`SELECT 1 FROM sessions WHERE session_id = ?`, sessionID)
	if row.Scan(new(int)) == nil {
		return true, nil
	}
	_, err = s.db.Exec(
		`INSERT INTO sessions (session_id, cwd, source, started_at, root_span_id) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO NOTHING`,
		sessionID, cwd, source, startedAt.UnixNano(), rootSpanID,
	)
	return false, err
}

func (s *Store) RootSpanID(sessionID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT root_span_id FROM sessions WHERE session_id = ?`, sessionID).Scan(&id)
	return id, err
}

func (s *Store) CloseSession(sessionID string, endedAt time.Time, reason string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE session_id = ?`,
		endedAt.UnixNano(), reason, sessionID,
	)
	return err
}

// NextSeq returns a fresh, session-scoped, monotonically increasing counter
// used to build unique deterministic span-ID seeds ("<session_id>:<seq>").
func (s *Store) NextSeq(sessionID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var seq int
	err = tx.QueryRow(`SELECT next_seq FROM sessions WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET next_seq = ? WHERE session_id = ?`, seq+1, sessionID); err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// PendingSpan is a span that has been opened (start event seen) but not yet
// closed (end event not yet seen).
type PendingSpan struct {
	SessionID    string
	Seq          int
	SpanID       string
	ParentSpanID string
	Kind         string
	Name         string
	MatchKey     string
	StartTime    time.Time
	StartAttrs   map[string]any
}

// PushPending records a newly-opened span and pushes it onto the session's
// stack. MatchKey is used by PopPending to prefer popping the span that
// actually corresponds to the closing event (e.g. tool_name) over whatever
// happens to be on top, since tool calls are not always perfectly nested.
func (s *Store) PushPending(p PendingSpan) error {
	b, err := json.Marshal(p.StartAttrs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO pending_spans (session_id, seq, span_id, parent_span_id, kind, name, match_key, start_time, start_attrs)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.SessionID, p.Seq, p.SpanID, p.ParentSpanID, p.Kind, p.Name, p.MatchKey, p.StartTime.UnixNano(), string(b),
	)
	return err
}

// PopPending pops the pending span matching matchKey if one exists,
// otherwise falls back to the most recently pushed pending span in the
// session (LIFO), which handles events whose payload doesn't carry enough
// information to match exactly.
func (s *Store) PopPending(sessionID, matchKey string) (*PendingSpan, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	query := `SELECT seq, span_id, parent_span_id, kind, name, start_time, start_attrs FROM pending_spans WHERE session_id = ?`
	args := []any{sessionID}
	if matchKey != "" {
		query += ` AND match_key = ? ORDER BY seq DESC LIMIT 1`
		args = append(args, matchKey)
	} else {
		query += ` ORDER BY seq DESC LIMIT 1`
	}

	var p PendingSpan
	var startAttrs string
	var startNanos int64
	row := tx.QueryRow(query, args...)
	err = row.Scan(&p.Seq, &p.SpanID, &p.ParentSpanID, &p.Kind, &p.Name, &startNanos, &startAttrs)
	if err == sql.ErrNoRows {
		if matchKey == "" {
			return nil, false, nil
		}
		// No exact match on tool name; fall back to LIFO.
		row = tx.QueryRow(
			`SELECT seq, span_id, parent_span_id, kind, name, start_time, start_attrs FROM pending_spans WHERE session_id = ? ORDER BY seq DESC LIMIT 1`,
			sessionID,
		)
		err = row.Scan(&p.Seq, &p.SpanID, &p.ParentSpanID, &p.Kind, &p.Name, &startNanos, &startAttrs)
	}
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p.StartTime = time.Unix(0, startNanos)
	if err := json.Unmarshal([]byte(startAttrs), &p.StartAttrs); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_spans WHERE session_id = ? AND seq = ?`, sessionID, p.Seq); err != nil {
		return nil, false, err
	}
	return &p, true, tx.Commit()
}

// PeekParent returns the span ID currently on top of the session's pending
// stack, i.e. the span that should be the parent of a newly-started span or
// an instant (zero-duration) span. If the stack is empty, callers should use
// the session's root span instead.
func (s *Store) PeekParent(sessionID string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT span_id FROM pending_spans WHERE session_id = ? ORDER BY seq DESC LIMIT 1`, sessionID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

// FinishedSpan is a complete span record, ready to be written by the
// OTel exporter (internal/tracer) once a pending span's matching end event
// has arrived.
type FinishedSpan struct {
	SpanID       string
	TraceID      string
	SessionID    string
	ParentSpanID string
	Name         string
	Kind         string
	Status       string
	StartTime    time.Time
	EndTime      time.Time
	Attributes   map[string]any
}

func (s *Store) InsertSpan(sp FinishedSpan) error {
	b, err := json.Marshal(sp.Attributes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO spans (span_id, trace_id, session_id, parent_span_id, name, kind, status, start_time, end_time, attributes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(span_id) DO UPDATE SET end_time=excluded.end_time, status=excluded.status, attributes=excluded.attributes`,
		sp.SpanID, sp.TraceID, sp.SessionID, sp.ParentSpanID, sp.Name, sp.Kind, sp.Status,
		sp.StartTime.UnixNano(), sp.EndTime.UnixNano(), string(b),
	)
	return err
}

// --- Read side, used by cmd/server ---

type SessionSummary struct {
	SessionID string
	Cwd       string
	Source    string
	StartedAt time.Time
	EndedAt   *time.Time
	EndReason string
	SpanCount int
}

func (s *Store) ListSessions(limit int) ([]SessionSummary, error) {
	rows, err := s.db.Query(`
		SELECT sessions.session_id, sessions.cwd, sessions.source, sessions.started_at, sessions.ended_at, sessions.end_reason,
		       (SELECT COUNT(*) FROM spans WHERE spans.session_id = sessions.session_id)
		FROM sessions
		ORDER BY sessions.started_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionSummary
	for rows.Next() {
		var sm SessionSummary
		var startedAt int64
		var endedAt sql.NullInt64
		var endReason sql.NullString
		if err := rows.Scan(&sm.SessionID, &sm.Cwd, &sm.Source, &startedAt, &endedAt, &endReason, &sm.SpanCount); err != nil {
			return nil, err
		}
		sm.StartedAt = time.Unix(0, startedAt)
		if endedAt.Valid {
			t := time.Unix(0, endedAt.Int64)
			sm.EndedAt = &t
		}
		sm.EndReason = endReason.String
		out = append(out, sm)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(sessionID string) (*SessionSummary, error) {
	var sm SessionSummary
	sm.SessionID = sessionID
	var startedAt int64
	var endedAt sql.NullInt64
	var endReason sql.NullString
	err := s.db.QueryRow(`
		SELECT cwd, source, started_at, ended_at, end_reason,
		       (SELECT COUNT(*) FROM spans WHERE spans.session_id = ?)
		FROM sessions WHERE session_id = ?`, sessionID, sessionID,
	).Scan(&sm.Cwd, &sm.Source, &startedAt, &endedAt, &endReason, &sm.SpanCount)
	if err != nil {
		return nil, err
	}
	sm.StartedAt = time.Unix(0, startedAt)
	if endedAt.Valid {
		t := time.Unix(0, endedAt.Int64)
		sm.EndedAt = &t
	}
	sm.EndReason = endReason.String
	return &sm, nil
}

// PromptUsage is one user turn's token/model snapshot (a prompt span), joined
// with its session's cwd, for the usage dashboard. Token counts come from the
// transcript enrichment attached at turn close; they are the most-recent
// assistant usage for that turn, so aggregates are approximate.
type PromptUsage struct {
	SessionID   string
	Cwd         string
	Start       time.Time
	Model       string
	Prompt      string
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
}

func attrInt64(attrs map[string]any, key string) int64 {
	switch n := attrs[key].(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// PromptUsages returns every closed prompt span across all sessions, with the
// token usage and model parsed out of its attributes. This is the raw input
// the web layer aggregates into the usage dashboard.
func (s *Store) PromptUsages() ([]PromptUsage, error) {
	rows, err := s.db.Query(`
		SELECT spans.session_id, sessions.cwd, spans.start_time, spans.attributes
		FROM spans
		JOIN sessions ON sessions.session_id = spans.session_id
		WHERE spans.kind = 'prompt'
		ORDER BY spans.start_time ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PromptUsage
	for rows.Next() {
		var pu PromptUsage
		var start int64
		var attrs string
		if err := rows.Scan(&pu.SessionID, &pu.Cwd, &start, &attrs); err != nil {
			return nil, err
		}
		pu.Start = time.Unix(0, start)
		var m map[string]any
		if err := json.Unmarshal([]byte(attrs), &m); err != nil {
			return nil, err
		}
		pu.Model, _ = m["model"].(string)
		pu.Prompt, _ = m["prompt"].(string)
		pu.Input = attrInt64(m, "usage.input_tokens")
		pu.Output = attrInt64(m, "usage.output_tokens")
		pu.CacheRead = attrInt64(m, "usage.cache_read_input_tokens")
		pu.CacheCreate = attrInt64(m, "usage.cache_creation_input_tokens")
		out = append(out, pu)
	}
	return out, rows.Err()
}

type Span struct {
	SpanID       string
	TraceID      string
	ParentSpanID string
	Name         string
	Kind         string
	Status       string
	StartTime    time.Time
	EndTime      time.Time
	Attributes   map[string]any
}

func (s *Store) ListSpans(sessionID string) ([]Span, error) {
	rows, err := s.db.Query(`
		SELECT span_id, trace_id, parent_span_id, name, kind, status, start_time, end_time, attributes
		FROM spans WHERE session_id = ? ORDER BY start_time ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Span
	for rows.Next() {
		var sp Span
		var start, end int64
		var attrs string
		if err := rows.Scan(&sp.SpanID, &sp.TraceID, &sp.ParentSpanID, &sp.Name, &sp.Kind, &sp.Status, &start, &end, &attrs); err != nil {
			return nil, err
		}
		sp.StartTime = time.Unix(0, start)
		sp.EndTime = time.Unix(0, end)
		if err := json.Unmarshal([]byte(attrs), &sp.Attributes); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
