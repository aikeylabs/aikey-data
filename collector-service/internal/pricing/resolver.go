package pricing

// longContextThreshold is the input-token count above which tiered "above_128k"
// rates apply, for models that price long context differently (e.g. Gemini).
const longContextThreshold = 128_000

// modelKey identifies a priced model. The same model name under a different
// provider (direct vs bedrock vs vertex_ai) is a distinct entry with its own
// price — provider is part of the key, never dropped (design §3.4).
type modelKey struct {
	provider string
	model    string
}

// overrideKey scopes an enterprise/org price override. Phase 1 keys on org;
// future scopes (project/tenant) arrive via the mapping layer, not here.
type overrideKey struct {
	orgID    string
	provider string
	model    string
}

// historyEntry is one past price for a (provider, model), effective from a
// point in time. The resolver expects a model's entries pre-sorted by
// effectiveFrom DESC so the first entry with effectiveFrom <= request_ts is the
// price that was in force at request time.
type historyEntry struct {
	effectiveFrom int64 // unix ms
	prices        UnitPrices
}

// Resolver answers per-(provider, model) price lookups over three layers, in
// priority order: overrides -> history -> litellm. It is immutable after
// construction (built once at collector startup) and concurrency-safe for
// reads. Lookup performs NO logging or side effects — the caller (projector)
// WARN-logs and enqueues an ErrUnknownModel miss, keeping this a pure function
// (design §2.3 / §5.2).
type Resolver struct {
	overrides map[overrideKey]UnitPrices
	history   map[modelKey][]historyEntry // each slice sorted by effectiveFrom DESC
	litellm   map[modelKey]UnitPrices
	snapshot  Snapshot
}

// newResolver builds a Resolver from already-parsed in-memory layers. The loader
// (which reads the embedded files) calls this; tests call it directly with
// hand-built maps so the lookup logic is testable without any file I/O.
// History slices MUST already be sorted by effectiveFrom DESC.
func newResolver(
	litellm map[modelKey]UnitPrices,
	history map[modelKey][]historyEntry,
	overrides map[overrideKey]UnitPrices,
	snap Snapshot,
) *Resolver {
	return &Resolver{
		overrides: litellmNilSafe(overrides),
		history:   history,
		litellm:   litellm,
		snapshot:  snap,
	}
}

// Snapshot returns the pricing-source fingerprint this resolver was built from.
// The projector stamps it onto every event as pricing_snapshot_id.
func (r *Resolver) Snapshot() Snapshot { return r.snapshot }

// Lookup resolves the unit prices for one request. It returns the prices that
// were in force for (provider, model) at request_ts, scoped to orgID, with
// long-context tiering already applied based on inputTokens.
//
// Resolution order (first hit wins): overrides -> history -> litellm. On no
// match it returns (nil, ErrUnknownModel); the caller must treat that as
// "unpriced" (billable_amount=NULL + enqueue unpriced_models), never as free.
//
// The returned UnitPrices carries the effective (already-tiered) per-token
// rates and a Source tag; the projector serializes it verbatim into
// unit_prices_snapshot, so the audit record shows the exact rates that were
// multiplied — not a rate table the auditor would have to re-interpret.
func (r *Resolver) Lookup(provider, model string, requestTs, inputTokens int64, orgID string) (*UnitPrices, error) {
	key := modelKey{provider: provider, model: model}

	// 1. overrides (highest priority): enterprise/contract discounts.
	if orgID != "" {
		if up, ok := r.overrides[overrideKey{orgID: orgID, provider: provider, model: model}]; ok {
			up.Source = SourceOverrides
			return ptr(r.applyTier(up, inputTokens)), nil
		}
	}

	// 2. history: the price in force at request_ts (entries sorted DESC, so the
	// first one effective at-or-before request_ts is the most recent applicable).
	if entries, ok := r.history[key]; ok {
		for _, e := range entries {
			if requestTs >= e.effectiveFrom {
				up := e.prices
				up.Source = SourceHistory
				up.EffectiveFrom = e.effectiveFrom
				return ptr(r.applyTier(up, inputTokens)), nil
			}
		}
	}

	// 3. litellm: community current list price.
	if up, ok := r.litellm[key]; ok {
		up.Source = SourceLiteLLM
		return ptr(r.applyTier(up, inputTokens)), nil
	}

	return nil, ErrUnknownModel
}

// applyTier substitutes the above_128k rates when the request crosses the
// long-context threshold, then clears the tier fields so the returned prices
// represent the single effective rate that was actually applied. Keeping the
// tiering inside the resolver means the projector only asks "what does this
// request cost" and never has to know about context-length pricing (design §5.2).
func (r *Resolver) applyTier(up UnitPrices, inputTokens int64) UnitPrices {
	if inputTokens > longContextThreshold {
		if up.InputPerTokenAbove128k > 0 {
			up.InputPerToken = up.InputPerTokenAbove128k
		}
		if up.OutputPerTokenAbove128k > 0 {
			up.OutputPerToken = up.OutputPerTokenAbove128k
		}
	}
	up.InputPerTokenAbove128k = 0
	up.OutputPerTokenAbove128k = 0
	return up
}

func ptr(up UnitPrices) *UnitPrices { return &up }

// litellmNilSafe avoids nil-map reads in Lookup when a layer is empty.
func litellmNilSafe(m map[overrideKey]UnitPrices) map[overrideKey]UnitPrices {
	if m == nil {
		return map[overrideKey]UnitPrices{}
	}
	return m
}
