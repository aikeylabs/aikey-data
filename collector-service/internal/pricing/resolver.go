package pricing

import (
	"regexp"
	"sort"
	"strings"
)

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
	// byCanonical maps a normalized bare model name (provider/region/version
	// prefixes stripped) → list price, derived from litellm at construction. It
	// is the cross-provider fallback: many models exist in litellm ONLY under a
	// prefixed key (e.g. claude-3-5-haiku-20241022 only as the bedrock entry
	// anthropic.claude-3-5-haiku-20241022-v1:0), so an exact (anthropic, model)
	// lookup misses; the canonical name matches the prefixed entry's price.
	byCanonical map[string]UnitPrices
	snapshot    Snapshot
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
		overrides:   litellmNilSafe(overrides),
		history:     history,
		litellm:     litellm,
		byCanonical: buildCanonicalIndex(litellm),
		snapshot:    snap,
	}
}

// Model-name normalization regexes (industry pattern, mirrors LiteLLM's
// _strip_model_name + cross-region handling; see deep-research 2026-06-10).
// Applied to litellm KEYS at index build (they carry prefixes) and to the
// looked-up model (usually already bare, so a near no-op).
var (
	canonRegionPrefix = regexp.MustCompile(`^(global|us|eu|apac|jp|au|us-gov)\.`)
	canonVendorPrefix = regexp.MustCompile(`^(anthropic|amazon|meta|cohere|mistral|ai21|deepseek|writer|stability|nova|qwen|titan)\.`)
	canonAtDate       = regexp.MustCompile(`@(\d{8})$`)
	canonVersionSfx   = regexp.MustCompile(`-v\d+(:\d+)?$`)
)

// canonicalModel reduces a model string to a provider/region/version-agnostic
// bare name so the same model under different key conventions collapses to one
// lookup key. Order matters: slash provider prefix → region dot → vendor dot →
// @date→-date → trailing -vN[:M]. Examples:
//
//	bedrock/us.anthropic.claude-3-5-haiku-20241022-v1:0 → claude-3-5-haiku-20241022
//	anthropic.claude-3-5-haiku-20241022-v1:0            → claude-3-5-haiku-20241022
//	vertex_ai/claude-3-sonnet@20240229                  → claude-3-sonnet-20240229
//	claude-3-5-haiku-20241022 (already bare)            → claude-3-5-haiku-20241022
func canonicalModel(s string) string {
	m := strings.ToLower(strings.TrimSpace(s))
	// Only strip the AWS "bedrock/" path prefix — it re-keys the SAME dot-notation
	// model at the SAME price. Do NOT strip other "<host>/" prefixes
	// (together_ai/, fireworks/, deepseek-ai/, vertex_ai/, openrouter/, …): the
	// same model hosted by different providers has DIFFERENT prices, so merging
	// them across hosts would mis-price (verified: an unrestricted slash strip
	// produced 132 cross-host price collisions over the snapshot).
	m = strings.TrimPrefix(m, "bedrock/")
	m = canonRegionPrefix.ReplaceAllString(m, "")
	m = canonVendorPrefix.ReplaceAllString(m, "")
	m = canonAtDate.ReplaceAllString(m, "-$1")
	m = canonVersionSfx.ReplaceAllString(m, "")
	if m == "" { // never return empty — degrade to the original
		return strings.ToLower(strings.TrimSpace(s))
	}
	return m
}

// buildCanonicalIndex derives the canonical-name → price map from litellm.
// Two passes for deterministic, native-wins behavior: a native plain entry
// (its key already equals its canonical form, e.g. claude-3-haiku-20240307)
// is authoritative; prefixed/versioned entries only FILL gaps and never
// override a native price. Among competing prefixed entries the first by sorted
// key wins (region variants of one model share a price, so order is immaterial
// for them; sorting just makes the build deterministic).
func buildCanonicalIndex(litellm map[modelKey]UnitPrices) map[string]UnitPrices {
	keys := make([]modelKey, 0, len(litellm))
	for k := range litellm {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].provider < keys[j].provider
	})
	out := make(map[string]UnitPrices, len(litellm))
	for _, k := range keys { // pass 1: native plain entries are authoritative
		if canonicalModel(k.model) == k.model {
			out[k.model] = litellm[k]
		}
	}
	for _, k := range keys { // pass 2: prefixed entries fill gaps only
		canon := canonicalModel(k.model)
		if canon == k.model {
			continue
		}
		if _, exists := out[canon]; !exists {
			out[canon] = litellm[k]
		}
	}
	return out
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

	// 3. litellm: community current list price (exact provider+model). Native
	// plain keys hit here — fast path, no normalization, zero behavior change.
	if up, ok := r.litellm[key]; ok {
		up.Source = SourceLiteLLM
		return ptr(r.applyTier(up, inputTokens)), nil
	}

	// 4. normalized cross-provider fallback: the model has no exact key but a
	// region/version/provider-prefixed litellm entry canonicalizes to the same
	// bare name (e.g. claude-3-5-haiku-20241022 → anthropic.claude-...-v1:0).
	// Tagged SourceLiteLLMNormalized so the projector can WARN-log the match.
	if up, ok := r.byCanonical[canonicalModel(model)]; ok {
		up.Source = SourceLiteLLMNormalized
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
