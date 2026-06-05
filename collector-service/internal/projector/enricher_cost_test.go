package projector

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// odsForCost builds a minimal ODS record on the vault-origin short-circuit path
// (personal: VK) so the control-event lookup is skipped — applyCost runs first
// and independently, which is exactly what we want to isolate here.
func odsForCost(model, provider string, in, out, cacheRead, cacheCreation int64) *ODSRecord {
	at := aikeytime.FromTime(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	return &ODSRecord{
		OrgID:                    "org1",
		EventTime:                at,
		OccurredAt:               at,
		VirtualKeyID:             sql.NullString{String: "personal:test", Valid: true},
		Model:                    sql.NullString{String: model, Valid: true},
		ProviderCode:             sql.NullString{String: provider, Valid: true},
		InputTokens:              sql.NullInt64{Int64: in, Valid: true},
		OutputTokens:             sql.NullInt64{Int64: out, Valid: true},
		CachedInputTokens:        sql.NullInt64{Int64: cacheRead, Valid: true},
		CacheCreationInputTokens: sql.NullInt64{Int64: cacheCreation, Valid: true},
	}
}

// fixedTestResolver builds a Resolver from a FIXED in-memory price table so the
// enricher cost tests assert against known rates, independent of the embedded
// LiteLLM file (which evolves on every upstream price sync — Stage 2.6 decision
// 2A). The single fixture model carries the canonical claude-3-5-sonnet rates
// the cost assertions below are computed from.
func fixedTestResolver(t *testing.T) *pricing.Resolver {
	t.Helper()
	litellm := []byte(`{"claude-3-5-sonnet-20241022":{` +
		`"input_cost_per_token":0.000003,` +
		`"output_cost_per_token":0.000015,` +
		`"cache_creation_input_token_cost":0.00000375,` +
		`"cache_read_input_token_cost":0.0000003,` +
		`"litellm_provider":"anthropic"}}`)
	empty := []byte(`{"schema_version":1,"entries":[]}`)
	r, err := pricing.LoadFrom(litellm, empty, empty)
	if err != nil {
		t.Fatalf("LoadFrom fixture: %v", err)
	}
	return r
}

// The enricher computes cost from the fixed test resolver and stamps the full
// audit trail. 方案 A: input_tokens=100 is the PURE (uncached) input; the 50
// cache_read + 20 cache_creation are SEPARATE, billed at their own rates
// (inputIncludesCache=false → no subtraction). claude-3-5-sonnet (anthropic):
// 100*3e-6 + 20*3.75e-6 + 50*3e-7 + 200*1.5e-5 = 0.00339000.
func TestEnrich_ComputesCostAndAuditTrail(t *testing.T) {
	e := NewEnricher(&mockControlReader{}, fixedTestResolver(t))

	fact, err := e.Enrich(context.Background(),
		odsForCost("claude-3-5-sonnet-20241022", "anthropic", 100, 200, 50, 20))
	if err != nil {
		t.Fatal(err)
	}

	if fact.BillableAmount == nil {
		t.Fatal("billable_amount must be computed, got nil")
	}
	if want := "0.00339000"; *fact.BillableAmount != want {
		t.Errorf("cost = %s, want %s", *fact.BillableAmount, want)
	}
	if fact.Currency != "USD" {
		t.Errorf("currency = %q, want USD", fact.Currency)
	}
	if fact.UnitPricesSnapshot == nil {
		t.Error("unit_prices_snapshot must be set on a priced event")
	}
	if fact.PricingSnapshotID == "" {
		t.Error("pricing_snapshot_id must be set")
	}
	if fact.BillingPeriod != "2026-06" {
		t.Errorf("billing_period = %q, want 2026-06", fact.BillingPeriod)
	}
}

// Unknown model: cost stays NULL (never zero), but pricing_snapshot_id is still
// recorded so the row pins which global price state had no entry.
func TestEnrich_UnknownModelLeavesNullCost(t *testing.T) {
	e := NewEnricher(&mockControlReader{}, fixedTestResolver(t))

	fact, err := e.Enrich(context.Background(),
		odsForCost("totally-unknown-model-x", "anthropic", 100, 50, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if fact.BillableAmount != nil {
		t.Errorf("unknown model must leave cost NULL, got %q", *fact.BillableAmount)
	}
	if fact.UnitPricesSnapshot != nil {
		t.Error("unknown model must leave unit_prices_snapshot NULL")
	}
	if fact.PricingSnapshotID == "" {
		t.Error("pricing_snapshot_id must still be recorded on a miss")
	}
}

type mockUnpricedSink struct{ calls []string }

func (m *mockUnpricedSink) Enqueue(provider, model string) {
	m.calls = append(m.calls, provider+"/"+model)
}

// Unknown model enqueues the (provider, model) for the pending-pricing queue.
func TestEnrich_UnknownModelEnqueuesUnpriced(t *testing.T) {
	e := NewEnricher(&mockControlReader{}, fixedTestResolver(t))
	sink := &mockUnpricedSink{}
	e.SetUnpricedSink(sink)

	if _, err := e.Enrich(context.Background(),
		odsForCost("unknown-model-xyz", "anthropic", 100, 50, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 1 || sink.calls[0] != "anthropic/unknown-model-xyz" {
		t.Errorf("expected enqueue [anthropic/unknown-model-xyz], got %v", sink.calls)
	}
}

// A priced (known) model must NOT enqueue anything.
func TestEnrich_KnownModelNoEnqueue(t *testing.T) {
	e := NewEnricher(&mockControlReader{}, fixedTestResolver(t))
	sink := &mockUnpricedSink{}
	e.SetUnpricedSink(sink)

	if _, err := e.Enrich(context.Background(),
		odsForCost("claude-3-5-sonnet-20241022", "anthropic", 100, 50, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 0 {
		t.Errorf("known model must not enqueue, got %v", sink.calls)
	}
}

// A nil resolver (tests / pricing disabled) leaves cost NULL but still sets the
// billing period — applyCost must not panic.
func TestEnrich_NilResolverNoCostNoPanic(t *testing.T) {
	e := NewEnricher(&mockControlReader{}, nil)
	fact, err := e.Enrich(context.Background(),
		odsForCost("claude-3-5-sonnet-20241022", "anthropic", 100, 200, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if fact.BillableAmount != nil {
		t.Errorf("nil resolver must leave cost NULL, got %q", *fact.BillableAmount)
	}
	if fact.BillingPeriod != "2026-06" {
		t.Errorf("billing_period still derived without resolver, got %q", fact.BillingPeriod)
	}
}

// TestInputIncludesCache_PureInputNoSubtract is the 方案 A regression guard:
// the proxy adapters now report PURE (uncached) input, so the collector flag is
// FALSE for ALL providers (no subtraction) and cache is billed only at its own
// rate via the separate buckets. Passing the TOTAL with this flag would over-charge
// — which is exactly why 方案 A moved the split to the source.
func TestInputIncludesCache_PureInputNoSubtract(t *testing.T) {
	for _, p := range []string{"anthropic", "openai", "kimi", "moonshot", "gemini", ""} {
		if inputIncludesCache(p) {
			t.Errorf("inputIncludesCache(%q) must be false (proxy reports PURE input post-方案-A)", p)
		}
	}

	// opus-4-8 rates: input 5e-6, cache_read 5e-7 (10x cheaper). Cache-heavy request:
	// pure input=2, cache_read=998. With pure input + flag=false (no subtract), the
	// cost charges 2 at input and 998 at cache_read.
	up := pricing.UnitPrices{InputPerToken: 5e-6, OutputPerToken: 2.5e-5,
		CacheReadInputPerToken: 5e-7, CacheCreationInputPerToken: 6.25e-6}
	pure := pricing.TokenCounts{Input: 2, CacheRead: 998}
	got := pricing.ComputeCost(up, pure, inputIncludesCache("anthropic"))
	want := 2*5e-6 + 998*5e-7 // pure 2 @ input + 998 @ cache_read
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("cache-heavy anthropic cost: want %v (cache @ cache rate) got %v", want, got)
	}
	// sanity: had the adapter still sent the TOTAL (1000) with this false flag (the
	// pre-方案-A bug shape), it would over-charge — 方案 A's pure input avoids it.
	totalShape := pricing.TokenCounts{Input: 1000, CacheRead: 998}
	if over := pricing.ComputeCost(up, totalShape, false); over <= got {
		t.Fatalf("sanity: total-input shape must over-charge (%v) vs correct (%v)", over, got)
	} else if over >= want*5 {
		t.Logf("over-charge factor ~%.1fx if total were sent (over=%v correct=%v)", over/want, over, want)
	}
}

// TestEnrich_LongContextTier_UsesTotalContext is the 方案 A guard for the 128k
// tier fix: the long-context price tier keys off TOTAL context (pure + cache), not
// the (now pure) input_tokens alone. A request with small pure input but large
// cache that crosses 128k must get the above-128k input rate — else cache-heavy
// long-context requests are under-charged.
func TestEnrich_LongContextTier_UsesTotalContext(t *testing.T) {
	litellm := []byte(`{"tiered-claude":{` +
		`"input_cost_per_token":0.000003,` +
		`"input_cost_per_token_above_128k_tokens":0.000006,` +
		`"output_cost_per_token":0.000015,` +
		`"cache_read_input_token_cost":0.0000003,` +
		`"litellm_provider":"anthropic"}}`)
	empty := []byte(`{"schema_version":1,"entries":[]}`)
	r, err := pricing.LoadFrom(litellm, empty, empty)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	e := NewEnricher(&mockControlReader{}, r)

	// pure input=100 (<128k), cache_read=200000 → total context 200100 (>128k).
	fact, err := e.Enrich(context.Background(),
		odsForCost("tiered-claude", "anthropic", 100, 0, 200000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if fact.BillableAmount == nil {
		t.Fatal("billable_amount nil")
	}
	// above-128k tier: pure 100 @ 6e-6 + cache 200000 @ 3e-7 = 0.0006 + 0.06 = 0.06060000.
	// (bug — tier keyed off pure input 100<128k — would give base 100@3e-6 = 0.06030000.)
	if want := "0.06060000"; *fact.BillableAmount != want {
		t.Errorf("long-context tier must key off TOTAL context: want %s, got %s", want, *fact.BillableAmount)
	}
}
