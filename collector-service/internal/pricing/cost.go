package pricing

import "strconv"

// TokenCounts holds the per-type token counts for one event, matching the DWD
// token columns. Used by ComputeCost.
type TokenCounts struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	Reasoning     int64
}

// ComputeCost returns the USD cost of one event = Σ tokens_i × unit_price_i.
//
// inputIncludesCache controls how Input is interpreted, which differs by
// provider family:
//
//   - false (Anthropic-style): Input is ALREADY the uncached portion; cache
//     read/creation are reported on their own counts and billed separately.
//   - true (OpenAI-style): Input (prompt_tokens) INCLUDES cached tokens, so the
//     uncached portion = Input − CacheRead − CacheCreation (clamped at 0).
//
// The caller (projector) decides the flag from the event's provider. Passing a
// bool rather than a provider string keeps this package decoupled from provider
// naming (design §5.2 / 附录 A).
//
// up must be a resolved price set (the projector only calls this on a Lookup
// hit; an ErrUnknownModel event is written with NULL cost, never zero).
func ComputeCost(up UnitPrices, tk TokenCounts, inputIncludesCache bool) float64 {
	uncachedInput := tk.Input
	if inputIncludesCache {
		uncachedInput = tk.Input - tk.CacheRead - tk.CacheCreation
		if uncachedInput < 0 {
			uncachedInput = 0
		}
	}
	return float64(uncachedInput)*up.InputPerToken +
		float64(tk.CacheCreation)*up.CacheCreationInputPerToken +
		float64(tk.CacheRead)*up.CacheReadInputPerToken +
		float64(tk.Output)*up.OutputPerToken +
		float64(tk.Reasoning)*up.ReasoningPerToken
}

// FormatCostUSD renders a computed cost as the decimal string stored in the
// NUMERIC(20,8) billable_amount column. Fixed 8-decimal precision matches the
// column scale and keeps the stored value byte-stable for auditing.
func FormatCostUSD(cost float64) string {
	return strconv.FormatFloat(cost, 'f', 8, 64)
}
