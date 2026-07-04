// Package web serves the local read-only UI over internal/store: a session
// list and a per-session trace waterfall, in the spirit of SigNoz/Jaeger but
// with zero external dependencies (no JS framework, no CDN).
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/papikayo/observability-code/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Server struct {
	st   *store.Store
	tmpl *template.Template
}

func NewServer(st *store.Store) (*Server, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{st: st, tmpl: tmpl}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /sessions", s.handleSessions)
	mux.HandleFunc("GET /sessions/{id}", s.handleSessionDetail)
	return mux
}

type sessionRow struct {
	ID         string
	Cwd        string
	Source     string
	Started    string
	StartedAgo string
	Duration   string
	SpanCount  int
	EndReason  string
	Live       bool
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.st.ListSessions(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows := make([]sessionRow, 0, len(sessions))
	for _, sm := range sessions {
		row := sessionRow{
			ID:         sm.SessionID,
			Cwd:        sm.Cwd,
			Source:     sm.Source,
			Started:    sm.StartedAt.Format("2006-01-02 15:04:05"),
			StartedAgo: humanizeAgo(sm.StartedAt),
			SpanCount:  sm.SpanCount,
			EndReason:  sm.EndReason,
			Live:       sm.EndedAt == nil,
		}
		if sm.EndedAt != nil {
			row.Duration = humanizeDuration(sm.EndedAt.Sub(sm.StartedAt))
		}
		rows = append(rows, row)
	}

	s.render(w, "sessions.html", map[string]any{
		"Sessions": rows,
		"Now":      time.Now().Format("2006-01-02 15:04:05"),
	})
}

type spanView struct {
	ID         string
	ParentID   string
	Name       string
	Kind       string
	Status     string
	Depth      int
	OffsetPct  float64
	WidthPct   float64
	DurationMs int64
	StartedAt  string
	Detail     string // one-line context (file path, command, message) for the row
	Added      int    // lines added (file-editing tools)
	Removed    int    // lines removed
	Diff       []diffHunk
	Attrs      []attrView
}

type attrView struct {
	Key   string
	Value string
}

type diffHunk struct {
	Header string
	Lines  []diffLine
}

type diffLine struct {
	Sign string // "+", "-", or " "
	Text string
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sm, err := s.st.GetSession(id)
	if err != nil {
		http.Error(w, "session not found: "+err.Error(), http.StatusNotFound)
		return
	}
	spans, err := s.st.ListSpans(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, minStart, maxEnd := buildWaterfall(spans)

	duration := "in progress"
	if !maxEnd.IsZero() && !minStart.IsZero() {
		duration = humanizeDuration(maxEnd.Sub(minStart))
	}

	turns, consumed := buildTurns(spans)
	rules := buildContext(spans)
	orphans := buildOrphans(spans, consumed)

	s.render(w, "trace.html", map[string]any{
		"Session":   sm,
		"Duration":  duration,
		"Turns":     turns,
		"MultiTurn": len(turns) > 1,
		"Rules":     rules,
		"Orphans":   orphans,
	})
}

// --- Per-turn summary: what each user prompt actually did ---
//
// The waterfall answers "what happened, when"; these summaries answer the
// higher-level question the UI leads with: for a given prompt, which model
// ran, how long it took, how many tokens it cost, and which tools / files /
// skills / subagents it touched. Each turn is a prompt span; we walk its
// subtree once and aggregate.

type turnView struct {
	Index         int
	SpanID        string
	Prompt        string
	Output        string
	Model         string
	Status        string
	StopReason    string
	Started       string
	Duration      string
	DurationMs    int64
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreate   int
	Tools         []toolStat
	ToolCalls     int
	Files         []string
	Skills        []string
	Agents        []string
	Events        []nameCount
	Compactions   int
	SpawnedAgents bool
	Spans         []spanView // this turn's own waterfall (its subtree)
}

type toolStat struct {
	Name       string
	Count      int
	Failed     int
	DurationMs int64
	Duration   string
}

type nameCount struct {
	Name  string
	Count int
}

type ruleView struct {
	Path   string
	Memory string
}

// buildTurns returns one summary+waterfall per user turn, in chronological
// order, plus the set of span IDs consumed by a turn (its prompt span and
// everything under it) so the caller can render whatever's left over — session
// start-up events, cross-turn notifications — as a separate section.
func buildTurns(spans []store.Span) ([]turnView, map[string]bool) {
	t := groupSpans(spans)
	consumed := map[string]bool{}

	prompts := make([]store.Span, 0)
	for _, sp := range spans {
		if sp.Kind == "prompt" {
			prompts = append(prompts, sp)
		}
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].StartTime.Before(prompts[j].StartTime) })

	out := make([]turnView, 0, len(prompts))
	for i, p := range prompts {
		consumed[p.SpanID] = true
		tv := turnView{
			Index:        i + 1,
			SpanID:       p.SpanID,
			Prompt:       attrString(p.Attributes, "prompt"),
			Output:       attrString(p.Attributes, "output"),
			Model:        shortModel(attrString(p.Attributes, "model")),
			Status:       p.Status,
			StopReason:   attrString(p.Attributes, "stop_reason"),
			Started:      p.StartTime.Format("15:04:05"),
			InputTokens:  attrInt(p.Attributes, "usage.input_tokens"),
			OutputTokens: attrInt(p.Attributes, "usage.output_tokens"),
			CacheRead:    attrInt(p.Attributes, "usage.cache_read_input_tokens"),
			CacheCreate:  attrInt(p.Attributes, "usage.cache_creation_input_tokens"),
		}
		if p.EndTime.After(p.StartTime) {
			tv.DurationMs = p.EndTime.Sub(p.StartTime).Milliseconds()
			tv.Duration = humanizeDuration(p.EndTime.Sub(p.StartTime))
		}

		acc := newTurnAcc()
		for _, c := range t.children[p.SpanID] {
			collectSubtree(t, c, acc)
		}
		acc.fill(&tv)
		tv.Spans = buildTurnWaterfall(t, p, consumed)
		out = append(out, tv)
	}
	return out, consumed
}

// buildTurnWaterfall lays out a single turn's subtree (the prompt span's
// descendants) as a waterfall whose offsets are relative to that turn's own
// time span — so each turn reads as a self-contained timeline. It also records
// every visited span in consumed.
func buildTurnWaterfall(t spanTree, prompt store.Span, consumed map[string]bool) []spanView {
	start := prompt.StartTime
	end := subtreeMaxEnd(t, prompt)
	total := max(end.Sub(start), time.Millisecond)

	var views []spanView
	var walk func(sp store.Span, depth int)
	walk = func(sp store.Span, depth int) {
		consumed[sp.SpanID] = true
		views = append(views, newSpanView(sp, depth, start, total))
		for _, c := range t.children[sp.SpanID] {
			walk(c, depth+1)
		}
	}
	for _, c := range t.children[prompt.SpanID] {
		walk(c, 0)
	}
	return views
}

func subtreeMaxEnd(t spanTree, sp store.Span) time.Time {
	maxEnd := sp.EndTime
	for _, c := range t.children[sp.SpanID] {
		if e := subtreeMaxEnd(t, c); e.After(maxEnd) {
			maxEnd = e
		}
	}
	return maxEnd
}

// buildOrphans lays out spans that belong to no turn — the session-start
// events (InstructionsLoaded, etc.) and any notifications that fired between
// turns — as one flat, session-relative waterfall. The session root span
// itself is skipped (it's just the container).
func buildOrphans(spans []store.Span, consumed map[string]bool) []spanView {
	var minStart, maxEnd time.Time
	for _, sp := range spans {
		if minStart.IsZero() || sp.StartTime.Before(minStart) {
			minStart = sp.StartTime
		}
		if sp.EndTime.After(maxEnd) {
			maxEnd = sp.EndTime
		}
	}
	total := max(maxEnd.Sub(minStart), time.Millisecond)

	var out []spanView
	for _, sp := range spans { // already start-time ordered from the store
		if consumed[sp.SpanID] || sp.Kind == "session" {
			continue
		}
		out = append(out, newSpanView(sp, 0, minStart, total))
	}
	return out
}

type turnAcc struct {
	tools     map[string]*toolStat
	toolOrder []string
	files     *orderedSet
	skills    *orderedSet
	agents    *orderedSet
	events    map[string]int
	evtOrder  []string
	compact   int
}

func newTurnAcc() *turnAcc {
	return &turnAcc{
		tools:  map[string]*toolStat{},
		files:  newOrderedSet(),
		skills: newOrderedSet(),
		agents: newOrderedSet(),
		events: map[string]int{},
	}
}

func collectSubtree(t spanTree, sp store.Span, acc *turnAcc) {
	durMs := int64(0)
	if sp.EndTime.After(sp.StartTime) {
		durMs = sp.EndTime.Sub(sp.StartTime).Milliseconds()
	}

	switch sp.Kind {
	case "tool":
		acc.addTool(sp, durMs)
	case "agent":
		at := attrString(sp.Attributes, "agent_type")
		if at == "" {
			at = sp.Name
		}
		acc.agents.add(at)
	case "compact":
		acc.compact++
	case "event":
		if _, seen := acc.events[sp.Name]; !seen {
			acc.evtOrder = append(acc.evtOrder, sp.Name)
		}
		acc.events[sp.Name]++
	}

	for _, c := range t.children[sp.SpanID] {
		collectSubtree(t, c, acc)
	}
}

// addTool records one tool span into the accumulator: bumps its per-name
// stats and mines its input for the files / skills / subagents it references.
func (a *turnAcc) addTool(sp store.Span, durMs int64) {
	name := attrString(sp.Attributes, "tool_name")
	if name == "" {
		name = sp.Name
	}
	ts := a.tools[name]
	if ts == nil {
		ts = &toolStat{Name: name}
		a.tools[name] = ts
		a.toolOrder = append(a.toolOrder, name)
	}
	ts.Count++
	ts.DurationMs += durMs
	if sp.Status == "error" {
		ts.Failed++
	}

	input := parseJSONObject(attrString(sp.Attributes, "tool_input"))
	if fp, ok := input["file_path"].(string); ok && fp != "" {
		a.files.add(fp)
	}
	if np, ok := input["notebook_path"].(string); ok && np != "" {
		a.files.add(np)
	}
	switch name {
	case "Skill":
		if sk, ok := input["skill"].(string); ok && sk != "" {
			a.skills.add(sk)
		}
	case "Task":
		if at, ok := input["subagent_type"].(string); ok && at != "" {
			a.agents.add(at)
		}
	}
}

func (a *turnAcc) fill(tv *turnView) {
	for _, name := range a.toolOrder {
		ts := a.tools[name]
		ts.Duration = humanizeDuration(time.Duration(ts.DurationMs) * time.Millisecond)
		tv.Tools = append(tv.Tools, *ts)
		tv.ToolCalls += ts.Count
	}
	tv.Files = a.files.items
	tv.Skills = a.skills.items
	tv.Agents = a.agents.items
	tv.SpawnedAgents = len(a.agents.items) > 0
	tv.Compactions = a.compact
	for _, name := range a.evtOrder {
		tv.Events = append(tv.Events, nameCount{Name: name, Count: a.events[name]})
	}
}

// buildContext surfaces the CLAUDE.md / rule files Claude Code loaded for the
// session (InstructionsLoaded events), which the raw waterfall buries.
func buildContext(spans []store.Span) []ruleView {
	seen := map[string]bool{}
	out := make([]ruleView, 0)
	for _, sp := range spans {
		if sp.Kind != "event" || sp.Name != "InstructionsLoaded" {
			continue
		}
		raw := parseJSONObject(attrString(sp.Attributes, "raw"))
		path, _ := raw["file_path"].(string)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		mem, _ := raw["memory_type"].(string)
		out = append(out, ruleView{Path: shortPath(path), Memory: mem})
	}
	return out
}

// orderedSet is a de-duplicating string collection that preserves first-seen
// order, so lists (files touched, skills used) read chronologically.
type orderedSet struct {
	items []string
	seen  map[string]bool
}

func newOrderedSet() *orderedSet { return &orderedSet{seen: map[string]bool{}} }

func (o *orderedSet) add(s string) {
	if o.seen[s] {
		return
	}
	o.seen[s] = true
	o.items = append(o.items, s)
}

func attrString(attrs map[string]any, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// attrInt reads a numeric attribute. Numbers round-trip through JSON as
// float64 (map[string]any), so that's the common case.
func attrInt(attrs map[string]any, key string) int {
	switch n := attrs[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// parseJSONObject decodes a JSON-object-shaped string attribute (e.g.
// tool_input, an event's raw payload) into a map; returns an empty map on any
// failure so callers can index it freely.
func parseJSONObject(s string) map[string]any {
	if s == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return map[string]any{}
	}
	return m
}

// shortModel trims the vendor prefix off a model id for display
// ("claude-opus-4-8" -> "opus-4-8"), leaving unknown shapes untouched.
func shortModel(m string) string {
	return strings.TrimPrefix(m, "claude-")
}

// shortPath collapses an absolute rule path to the portion that identifies
// it, preferring what follows a ".claude/" segment.
func shortPath(p string) string {
	if i := strings.LastIndex(p, ".claude/"); i >= 0 {
		return p[i:]
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// spanTree groups a flat span list into a parent -> children adjacency map
// plus the ordered list of root spans (no parent, or an unresolvable one).
type spanTree struct {
	children map[string][]store.Span
	roots    []store.Span
	minStart time.Time
	maxEnd   time.Time
}

func groupSpans(spans []store.Span) spanTree {
	t := spanTree{children: map[string][]store.Span{}}
	byID := make(map[string]store.Span, len(spans))
	for _, sp := range spans {
		byID[sp.SpanID] = sp
		if t.minStart.IsZero() || sp.StartTime.Before(t.minStart) {
			t.minStart = sp.StartTime
		}
		if sp.EndTime.After(t.maxEnd) {
			t.maxEnd = sp.EndTime
		}
	}
	for _, sp := range spans {
		if sp.ParentSpanID == "" || byID[sp.ParentSpanID].SpanID == "" {
			t.roots = append(t.roots, sp)
		} else {
			t.children[sp.ParentSpanID] = append(t.children[sp.ParentSpanID], sp)
		}
	}
	sort.Slice(t.roots, func(i, j int) bool { return t.roots[i].StartTime.Before(t.roots[j].StartTime) })
	for _, kids := range t.children {
		sort.Slice(kids, func(i, j int) bool { return kids[i].StartTime.Before(kids[j].StartTime) })
	}
	return t
}

// buildWaterfall arranges the flat, start-time-ordered span list into a
// parent/child tree (DFS order, so children render directly under their
// parent) and computes each span's horizontal offset/width as a percentage
// of the overall session time range, for the waterfall bars.
func buildWaterfall(spans []store.Span) (views []spanView, minStart, maxEnd time.Time) {
	if len(spans) == 0 {
		return nil, time.Time{}, time.Time{}
	}

	t := groupSpans(spans)
	total := max(t.maxEnd.Sub(t.minStart), time.Millisecond)

	var walk func(sp store.Span, depth int)
	walk = func(sp store.Span, depth int) {
		views = append(views, newSpanView(sp, depth, t.minStart, total))
		for _, c := range t.children[sp.SpanID] {
			walk(c, depth+1)
		}
	}
	for _, r := range t.roots {
		walk(r, 0)
	}
	return views, t.minStart, t.maxEnd
}

func newSpanView(sp store.Span, depth int, minStart time.Time, total time.Duration) spanView {
	offsetPct := float64(sp.StartTime.Sub(minStart)) / float64(total) * 100
	widthPct := max(float64(sp.EndTime.Sub(sp.StartTime))/float64(total)*100, 0.3) // keep near-instant spans visible
	v := spanView{
		ID:         sp.SpanID,
		ParentID:   sp.ParentSpanID,
		Name:       sp.Name,
		Kind:       sp.Kind,
		Status:     sp.Status,
		Depth:      depth,
		OffsetPct:  offsetPct,
		WidthPct:   widthPct,
		DurationMs: sp.EndTime.Sub(sp.StartTime).Milliseconds(),
		StartedAt:  sp.StartTime.Format("15:04:05.000"),
		Detail:     spanDetail(sp),
		Attrs:      flattenAttrs(sp.Attributes),
	}
	v.Diff, v.Added, v.Removed = parseDiff(sp)
	return v
}

// parseDiff turns a file-editing tool's structuredPatch (unified-diff hunks
// Claude Code puts in the tool_response of Edit/Write/MultiEdit) into hunks the
// template can render red/green, plus added/removed line counts. A brand-new
// file (Write) has an empty patch, so its whole content counts as added lines.
func parseDiff(sp store.Span) (hunks []diffHunk, added, removed int) {
	if sp.Kind != "tool" {
		return nil, 0, 0
	}
	resp := parseJSONObject(attrString(sp.Attributes, "tool_response"))
	patch, _ := resp["structuredPatch"].([]any)
	for _, h := range patch {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		hunk := diffHunk{Header: fmt.Sprintf("@@ -%d,%d +%d,%d @@",
			intOf(hm["oldStart"]), intOf(hm["oldLines"]), intOf(hm["newStart"]), intOf(hm["newLines"]))}
		lines, _ := hm["lines"].([]any)
		for _, l := range lines {
			s, _ := l.(string)
			sign, text := " ", ""
			if len(s) > 0 {
				text = s[1:]
				switch s[0] {
				case '+':
					sign, added = "+", added+1
				case '-':
					sign, removed = "-", removed+1
				}
			}
			hunk.Lines = append(hunk.Lines, diffLine{Sign: sign, Text: text})
		}
		hunks = append(hunks, hunk)
	}
	if len(hunks) == 0 && added == 0 { // new-file Write: no patch, count content lines
		if content, ok := resp["content"].(string); ok && content != "" {
			added = strings.Count(content, "\n") + 1
		}
	}
	return hunks, added, removed
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// spanDetail pulls the single most useful piece of context for a span so the
// waterfall row is legible without expanding its attributes: the file a tool
// touched, the command it ran, the notification message, etc.
func spanDetail(sp store.Span) string {
	switch sp.Kind {
	case "tool", "agent":
		input := parseJSONObject(attrString(sp.Attributes, "tool_input"))
		for _, key := range []string{
			"file_path", "notebook_path", "path", // file ops
			"command", "pattern", "query", "url", // bash / search / fetch
			"subagent_type", "skill", "description", "prompt", // task / skill
		} {
			if v, ok := input[key].(string); ok && v != "" {
				if key == "file_path" || key == "notebook_path" || key == "path" {
					v = shortPath(v)
				}
				return truncate(oneLine(v), 90)
			}
		}
	case "event":
		raw := parseJSONObject(attrString(sp.Attributes, "raw"))
		for _, key := range []string{"message", "notification_type", "reason", "source"} {
			if v, ok := raw[key].(string); ok && v != "" {
				return truncate(oneLine(v), 90)
			}
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func flattenAttrs(attrs map[string]any) []attrView {
	out := make([]attrView, 0, len(attrs))
	for k, v := range attrs {
		val := fmt.Sprintf("%v", v)
		if len(val) > 400 {
			val = val[:400] + "…"
		}
		out = append(out, attrView{Key: k, Value: val})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func humanizeDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

func humanizeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	if err := s.tmpl.ExecuteTemplate(&b, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(b.String()))
}
