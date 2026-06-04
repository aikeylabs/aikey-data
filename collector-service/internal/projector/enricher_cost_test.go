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
// audit trail. The proxy reports the TOTAL input (input_tokens=100 INCLUDES the
// 50 cache_read + 20 cache_creation), so the billable input is the uncached
// portion 100−50−20=30 (2026-06-04 fix B — cache must not also be billed at the
// input rate). claude-3-5-sonnet (anthropic):
// 30*3e-6 + 20*3.75e-6 + 50*3e-7 + 200*1.5e-5 = 0.00318000.
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
	if want := "0.00318000"; *fact.BillableAmount != want {
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

// TestInputIncludesCache_NoCacheOvercharge is the regression guard for the
// 2026-06-04 fix B (bugfix/2026-06-04-quota-token-metric-cache-semantics.md):
// every proxy adapter reports the TOTAL input (cache is a subset), so the
// uncached-subtract flag must be true for ALL providers — otherwise cache is
// billed at BOTH the input rate and the cache rate (~10x over-charge).
func TestInputIncludesCache_NoCacheOvercharge(t *testing.T) {
	for _, p := range []string{"anthropic", "openai", "kimi", "moonshot", "gemini", ""} {
		if !inputIncludesCache(p) {
			t.Errorf("inputIncludesCache(%q) must be true (proxy reports TOTAL input for all providers)", p)
		}
	}

	// Cache-heavy anthropic request priced via the enricher's flag must charge the
	// cache bucket at the CACHE rate, not the input rate. opus-4-8 rates: input
	// 5e-6, cache_read 5e-7 (10x cheaper). Total input=1000 (998 cached, 2 pure).
	up := pricing.UnitPrices{InputPerToken: 5e-6, OutputPerToken: 2.5e-5,
		CacheReadInputPerToken: 5e-7, CacheCreationInputPerToken: 6.25e-6}
	tk := pricing.TokenCounts{Input: 1000, CacheRead: 998}
	got := pricing.ComputeCost(up, tk, inputIncludesCache("anthropic"))
	want := 2*5e-6 + 998*5e-7 // pure 2 @ input + 998 @ cache_read
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("cache-heavy anthropic cost: want %v (cache @ cache rate) got %v", want, got)
	}
	// the old false-flag would have over-charged (cache @ input rate too).
	if over := pricing.ComputeCost(up, tk, false); over <= got {
		t.Fatalf("sanity: false flag must over-charge (%v) vs correct (%v)", over, got)
	} else if over < want*5 {
		t.Logf("over-charge factor ~%.1fx (over=%v correct=%v)", over/want, over, want)
	}
}
