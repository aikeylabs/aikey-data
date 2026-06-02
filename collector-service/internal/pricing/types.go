// Package pricing resolves per-token unit prices for usage cost computation.
//
// It is a pure in-memory lookup over three layered sources, checked in order:
// overrides (enterprise discounts) -> history (past price changes, matched by
// request timestamp) -> litellm (community current prices). The three sources
// are embedded into the binary at build time, so a privatized deployment never
// reaches out to the network at runtime.
//
// This package does NOT touch the database. Cost is computed by the projector
// using the UnitPrices returned here; the projector serializes the result into
// the event's unit_prices_snapshot column and stamps the pricing_snapshot_id so
// every billed amount stays auditable after prices change. See the design doc
// "20260526-计价报表-数据流与价目体系.md" §3.1 / §3.6 / §5.2.
package pricing

import "errors"

// ErrUnknownModel is returned by the resolver when no pricing layer matches the
// (provider, model) pair. The caller (projector) MUST treat this as "unpriced":
// write billable_amount=NULL and enqueue the pair into unpriced_models. It must
// never silently fall back to a zero price — a $0 row reads as "free model",
// which is a different (and wrong) statement than "we have no price for this".
// (CLAUDE.md: 禁止偷懒默认 / no-lazy-defaults.)
var ErrUnknownModel = errors.New("pricing: unknown model")

// Source identifies which layer produced a UnitPrices result. It is serialized
// into the per-event unit_prices_snapshot JSON so an auditor can later see which
// layer won the lookup (overrides vs history vs litellm) without replaying the
// resolver state.
type Source string

const (
	SourceOverrides Source = "overrides"
	SourceHistory   Source = "history"
	SourceLiteLLM   Source = "litellm"
)

// UnitPrices is the per-token price set for one (provider, model) resolved at
// one point in time. Every rate is USD per single token.
//
// A zero rate means "this token type is free for this model" (e.g. some models
// bill cache creation at zero) — which is a real, priced answer. That is
// deliberately distinct from ErrUnknownModel ("not priced at all"): the former
// produces billable_amount that includes a 0 term, the latter produces NULL.
type UnitPrices struct {
	InputPerToken              float64 `json:"input"`
	OutputPerToken             float64 `json:"output"`
	CacheCreationInputPerToken float64 `json:"cache_creation"`
	CacheReadInputPerToken     float64 `json:"cache_read"`
	ReasoningPerToken          float64 `json:"reasoning"`

	// Long-context tiered rates. Zero means "no tiering" (the flat rate above
	// applies regardless of context length). When non-zero, the projector picks
	// the *_Above128k rate once the request's input token count crosses the
	// threshold (Gemini and other long-context models price this way).
	InputPerTokenAbove128k  float64 `json:"input_above_128k,omitempty"`
	OutputPerTokenAbove128k float64 `json:"output_above_128k,omitempty"`

	// Provenance recorded into the audit snapshot.
	Source Source `json:"source"`
	// EffectiveFrom is the unix-ms timestamp at which this price began to apply,
	// set only for history-layer hits. Zero for litellm/overrides ("current").
	EffectiveFrom int64 `json:"effective_from,omitempty"`
}
