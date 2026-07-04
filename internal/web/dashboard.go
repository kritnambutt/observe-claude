package web

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/papikayo/observability-code/internal/pricing"
	"github.com/papikayo/observability-code/internal/store"
)

// The Overview dashboard: total usage, estimated API cost, an API-vs-plan
// comparison, a daily token chart, a by-model cost breakdown, top projects,
// and recent sessions — all derived from the per-turn (prompt span) usage
// snapshots. Everything is computed server-side (SVG charts, no JS/CDN) to
// keep the single-binary, zero-dependency shape of the rest of the app.

type statCard struct {
	Label  string
	Value  string
	Sub    string
	Accent bool
}

type modelRow struct {
	Model       string
	Turns       int
	Input       string
	Output      string
	CacheRead   string
	CacheCreate string
	Cost        string
	Color       string
	Pct         float64 // share of total cost, for the donut + legend
}

type donutSlice struct {
	Path  string
	Color string
}

type daySegment struct {
	Color     string
	HeightPct float64
}

type dayCol struct {
	Day      string
	Segments []daySegment
	Total    string
}

type projectRow struct {
	Name     string
	Tokens   string
	WidthPct float64
}

type dashSession struct {
	ID      string
	Short   string
	Project string
	Title   string
	Started string
	Model   string
	Turns   int
	Input   string
	Output  string
	Cost    string
}

// token-type colors, shared by the daily chart legend and stacks.
const (
	colInput  = "#38bdf8"
	colOutput = "#e2725b"
	colRead   = "#7fae7f"
	colWrite  = "#c9a54e"
)

// modelPalette colors the by-model donut/legend, in assignment order.
var modelPalette = []string{"#e2725b", "#c9a54e", "#7fae7f", "#38bdf8", "#a78bfa", "#8b93a7"}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	usages, err := s.st.PromptUsages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sessions, err := s.st.ListSessions(500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	d := buildDashboard(usages, sessions)
	d["Now"] = time.Now().Format("2006-01-02 15:04:05")
	s.render(w, "dashboard.html", d)
}

func buildDashboard(usages []store.PromptUsage, sessions []store.SessionSummary) map[string]any {
	var totIn, totOut, totRead, totWrite int64
	var totCost float64

	type modelAgg struct {
		turns                int
		in, out, read, write int64
		cost                 float64
	}
	byModel := map[string]*modelAgg{}
	var modelOrder []string

	type dayAgg struct{ in, out, read, write int64 }
	byDay := map[string]*dayAgg{}

	byProject := map[string]int64{}

	type sessAgg struct {
		in, out, read, write int64
		cost                 float64
		turns                int
		model                string
		title                string
	}
	bySession := map[string]*sessAgg{}
	seenSessions := map[string]bool{}

	for _, u := range usages {
		cost := pricing.Cost(u.Model, u.Input, u.Output, u.CacheRead, u.CacheCreate)
		totIn += u.Input
		totOut += u.Output
		totRead += u.CacheRead
		totWrite += u.CacheCreate
		totCost += cost

		model := u.Model
		if model == "" {
			model = "unknown"
		}
		ma := byModel[model]
		if ma == nil {
			ma = &modelAgg{}
			byModel[model] = ma
			modelOrder = append(modelOrder, model)
		}
		ma.turns++
		ma.in += u.Input
		ma.out += u.Output
		ma.read += u.CacheRead
		ma.write += u.CacheCreate
		ma.cost += cost

		day := u.Start.Format("2006-01-02")
		da := byDay[day]
		if da == nil {
			da = &dayAgg{}
			byDay[day] = da
		}
		da.in += u.Input
		da.out += u.Output
		da.read += u.CacheRead
		da.write += u.CacheCreate

		byProject[shortProject(u.Cwd)] += u.Input + u.Output + u.CacheRead + u.CacheCreate

		sa := bySession[u.SessionID]
		if sa == nil {
			sa = &sessAgg{}
			bySession[u.SessionID] = sa
		}
		sa.turns++
		sa.in += u.Input
		sa.out += u.Output
		sa.read += u.CacheRead
		sa.write += u.CacheCreate
		sa.cost += cost
		sa.model = model // last model seen wins
		if sa.title == "" && u.Prompt != "" {
			sa.title = u.Prompt
		}
		seenSessions[u.SessionID] = true
	}

	turns := len(usages)

	// --- stat cards ---
	cards := []statCard{
		{Label: "Sessions", Value: fmt.Sprint(len(seenSessions)), Sub: "with usage"},
		{Label: "Turns", Value: humanCount(int64(turns)), Sub: "user prompts"},
		{Label: "Input tokens", Value: humanCount(totIn), Sub: "uncached"},
		{Label: "Output tokens", Value: humanCount(totOut), Sub: "generated"},
		{Label: "Cache read", Value: humanCount(totRead), Sub: "from prompt cache"},
		{Label: "Cache creation", Value: humanCount(totWrite), Sub: "writes to cache"},
		{Label: "Est. cost", Value: "$" + fmtUSD(totCost), Sub: "API list pricing", Accent: true},
	}

	// --- by-model rows + donut (share of cost) ---
	sort.Slice(modelOrder, func(i, j int) bool {
		return byModel[modelOrder[i]].cost > byModel[modelOrder[j]].cost
	})
	var models []modelRow
	var donutVals []float64
	for i, name := range modelOrder {
		ma := byModel[name]
		color := modelPalette[i%len(modelPalette)]
		pct := 0.0
		if totCost > 0 {
			pct = ma.cost / totCost * 100
		}
		models = append(models, modelRow{
			Model:       name,
			Turns:       ma.turns,
			Input:       humanCount(ma.in),
			Output:      humanCount(ma.out),
			CacheRead:   humanCount(ma.read),
			CacheCreate: humanCount(ma.write),
			Cost:        "$" + fmtUSD(ma.cost),
			Color:       color,
			Pct:         pct,
		})
		donutVals = append(donutVals, ma.cost)
	}
	donut := buildDonut(donutVals, modelPalette)

	// --- daily chart (stacked, scaled to the busiest day) ---
	dayKeys := make([]string, 0, len(byDay))
	for k := range byDay {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	if len(dayKeys) > 30 {
		dayKeys = dayKeys[len(dayKeys)-30:]
	}
	var dayMax int64
	for _, k := range dayKeys {
		da := byDay[k]
		if t := da.in + da.out + da.read + da.write; t > dayMax {
			dayMax = t
		}
	}
	var days []dayCol
	for _, k := range dayKeys {
		da := byDay[k]
		total := da.in + da.out + da.read + da.write
		days = append(days, dayCol{
			Day:   k[5:], // MM-DD
			Total: humanCount(total),
			Segments: []daySegment{
				{Color: colInput, HeightPct: pctOf(da.in, dayMax)},
				{Color: colOutput, HeightPct: pctOf(da.out, dayMax)},
				{Color: colRead, HeightPct: pctOf(da.read, dayMax)},
				{Color: colWrite, HeightPct: pctOf(da.write, dayMax)},
			},
		})
	}

	// --- top projects by tokens ---
	type pv struct {
		name   string
		tokens int64
	}
	var projList []pv
	var projMax int64
	for name, tok := range byProject {
		projList = append(projList, pv{name, tok})
		if tok > projMax {
			projMax = tok
		}
	}
	sort.Slice(projList, func(i, j int) bool { return projList[i].tokens > projList[j].tokens })
	if len(projList) > 10 {
		projList = projList[:10]
	}
	var projects []projectRow
	for _, p := range projList {
		projects = append(projects, projectRow{
			Name:     p.name,
			Tokens:   humanCount(p.tokens),
			WidthPct: pctOf(p.tokens, projMax),
		})
	}

	// --- recent sessions (most recent first, enriched with usage) ---
	var recent []dashSession
	for _, sm := range sessions {
		sa := bySession[sm.SessionID]
		row := dashSession{
			ID:      sm.SessionID,
			Short:   shortID(sm.SessionID),
			Project: shortProject(sm.Cwd),
			Started: sm.StartedAt.Format("2006-01-02 15:04"),
		}
		if sa != nil {
			row.Title = truncate(sa.title, 60)
			row.Model = shortModel(sa.model)
			row.Turns = sa.turns
			row.Input = humanCount(sa.in)
			row.Output = humanCount(sa.out)
			row.Cost = "$" + fmtUSD(sa.cost)
		}
		recent = append(recent, row)
		if len(recent) >= 25 {
			break
		}
	}

	return map[string]any{
		"Cards":     cards,
		"Models":    models,
		"Donut":     donut,
		"Days":      days,
		"HasDays":   len(days) > 0,
		"Projects":  projects,
		"Recent":    recent,
		"ColInput":  colInput,
		"ColOutput": colOutput,
		"ColRead":   colRead,
		"ColWrite":  colWrite,
		// API-vs-plan comparison
		"Cost":       fmtUSD(totCost),
		"Pro":        fmtUSD(pricing.ProMonthly),
		"Max5":       fmtUSD(pricing.Max5Monthly),
		"Max":        fmtUSD(pricing.MaxMonthly),
		"ProSaving":  savingLabel(totCost, pricing.ProMonthly),
		"Max5Saving": savingLabel(totCost, pricing.Max5Monthly),
		"MaxSaving":  savingLabel(totCost, pricing.MaxMonthly),
	}
}

// buildDonut converts a set of values into SVG arc paths in a 0..100 viewbox
// (outer r=45, inner r=27, centered at 50,50), starting at 12 o'clock.
func buildDonut(values []float64, palette []string) []donutSlice {
	var total float64
	for _, v := range values {
		total += v
	}
	if total <= 0 {
		return nil
	}
	const cx, cy, rOut, rIn = 50.0, 50.0, 45.0, 27.0
	var out []donutSlice
	angle := -math.Pi / 2
	for i, v := range values {
		frac := v / total
		next := angle + frac*2*math.Pi
		// A full single slice can't be drawn as one arc; nudge to 359.99°.
		a1 := next
		if frac >= 0.9999 {
			a1 = angle + 2*math.Pi - 1e-4
		}
		large := 0
		if a1-angle > math.Pi {
			large = 1
		}
		x0o, y0o := cx+rOut*math.Cos(angle), cy+rOut*math.Sin(angle)
		x1o, y1o := cx+rOut*math.Cos(a1), cy+rOut*math.Sin(a1)
		x1i, y1i := cx+rIn*math.Cos(a1), cy+rIn*math.Sin(a1)
		x0i, y0i := cx+rIn*math.Cos(angle), cy+rIn*math.Sin(angle)
		path := fmt.Sprintf("M%.3f %.3f A%.1f %.1f 0 %d 1 %.3f %.3f L%.3f %.3f A%.1f %.1f 0 %d 0 %.3f %.3f Z",
			x0o, y0o, rOut, rOut, large, x1o, y1o, x1i, y1i, rIn, rIn, large, x0i, y0i)
		out = append(out, donutSlice{Path: path, Color: palette[i%len(palette)]})
		angle = next
	}
	return out
}

func pctOf(v, max int64) float64 {
	if max <= 0 {
		return 0
	}
	return float64(v) / float64(max) * 100
}

// savingLabel describes how the flat plan compares to metered API cost.
func savingLabel(apiCost, plan float64) string {
	if apiCost <= 0 {
		return "—"
	}
	if apiCost <= plan {
		return fmt.Sprintf("API is $%s cheaper", fmtUSD(plan-apiCost))
	}
	pct := (apiCost - plan) / apiCost * 100
	return fmt.Sprintf("saves $%s (%.0f%%)", fmtUSD(apiCost-plan), pct)
}

func humanCount(n int64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", f/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", f/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", f/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func fmtUSD(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	if v >= 1 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.4f", v)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// shortProject reduces a cwd to a "parent/leaf" project label.
func shortProject(cwd string) string {
	if cwd == "" {
		return "—"
	}
	parts := splitPath(cwd)
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return cwd
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
