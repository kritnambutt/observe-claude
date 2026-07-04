// Package pricing turns Claude token counts into a dollar estimate using
// published Anthropic API list prices (USD per million tokens). It is the
// single source of truth for the cost math the web dashboard shows.
//
// Prices are matched to a model id by family substring (opus/sonnet/haiku/
// fable) so new dated snapshots within a family price correctly without a
// table update. Cache-write is 1.25x base input (5-minute TTL), cache-read is
// 0.1x base input — the standard Anthropic prompt-caching multipliers.
package pricing

import "strings"

// Rate is the per-million-token price for one model family, in USD.
type Rate struct {
	Input      float64
	Output     float64
	CacheWrite float64
	CacheRead  float64
}

// family rates as of mid-2026 (see the claude-api model table). Cache columns
// are derived from Input (1.25x write, 0.1x read) but stored explicitly so a
// family with non-standard cache pricing can override.
var rates = map[string]Rate{
	"fable":  {Input: 10, Output: 50, CacheWrite: 12.5, CacheRead: 1.0},
	"opus":   {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5},
	"sonnet": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3},
	"haiku":  {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1},
}

// defaultRate is used when a model id matches no known family, so an unknown
// or future model still produces a plausible (Sonnet-tier) estimate rather
// than $0.
var defaultRate = rates["sonnet"]

// For returns the rate for a model id, matched by family substring.
func For(model string) Rate {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		return rates["fable"]
	case strings.Contains(m, "opus"):
		return rates["opus"]
	case strings.Contains(m, "haiku"):
		return rates["haiku"]
	case strings.Contains(m, "sonnet"):
		return rates["sonnet"]
	default:
		return defaultRate
	}
}

// Cost estimates the USD cost of a token bundle for a given model.
func Cost(model string, input, output, cacheRead, cacheWrite int64) float64 {
	r := For(model)
	const perM = 1_000_000.0
	return float64(input)/perM*r.Input +
		float64(output)/perM*r.Output +
		float64(cacheRead)/perM*r.CacheRead +
		float64(cacheWrite)/perM*r.CacheWrite
}

// Subscription plan prices (USD/month) for the API-vs-plan comparison.
const (
	ProMonthly  = 20.0
	Max5Monthly = 100.0
	MaxMonthly  = 200.0
)
