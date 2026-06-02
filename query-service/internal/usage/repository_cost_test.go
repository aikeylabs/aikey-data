package usage

import (
	"context"
	"math"
	"testing"
	"time"
)

// Cost aggregation tests for the cost-pricing Stage 3 fields
// (cost_usd / priced_request_count / unpriced_request_count). Each test
// exercises a DISTINCT code path edited in Stage 3:
//
//   - PersonalByProtocolTotal → scanProtocolTotal helper (shared w/ master)
//   - PersonalByKeyTotal       → d.-prefixed inline scan (aliased table)
//   - PersonalByModelTotal     → bare-column inline scan
//   - PersonalByAppTotal       → bare-column inline scan
//   - PersonalTimeline         → scanTimeline helper
//
// The trio semantics under test (design + decision recap):
//   - cost_usd            = Σ billable_amount over USD rows only (non-USD
//     priced rows are still "priced" but excluded from the USD sum).
//   - priced/unpriced     = SUM(request_count) split by billable_amount
//     IS NOT NULL / IS NULL, so priced+unpriced == request_count exactly.

func costPtr(v float64) *float64 { return &v }

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// noonMs is a fixed event_time inside the 2026-06-01 query window used by
// every cost test below.
var noonMs = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

// costParams builds a personal QueryParams scoped to the seed seat and the
// 2026-06-01 local-UTC day window.
func costParams(seat string) QueryParams {
	p := QueryParams{
		OrgID:     "org1",
		SeatID:    seat,
		StartDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	p.Defaults()
	return p
}

func TestPersonalByProtocolTotal_Cost(t *testing.T) {
	db := setupUsageTestDB(t)
	seat := "seatProto"
	// anthropic: 2 priced USD rows + 1 unpriced row carrying 2 requests.
	insertDWD(t, db, dwdRow{EventID: "p1", OrgID: "org1", SeatID: seat, ProviderCode: "anthropic", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.003)})
	insertDWD(t, db, dwdRow{EventID: "p2", OrgID: "org1", SeatID: seat, ProviderCode: "anthropic", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.002)})
	insertDWD(t, db, dwdRow{EventID: "p3", OrgID: "org1", SeatID: seat, ProviderCode: "anthropic", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 2 /* unpriced */})
	// openai: 1 priced-but-EUR row (priced, excluded from USD cost) + 1 unpriced.
	insertDWD(t, db, dwdRow{EventID: "p4", OrgID: "org1", SeatID: seat, ProviderCode: "openai", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.010), Currency: "EUR"})
	insertDWD(t, db, dwdRow{EventID: "p5", OrgID: "org1", SeatID: seat, ProviderCode: "openai", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1 /* unpriced */})

	rows, err := NewSQLRepository(db).PersonalByProtocolTotal(context.Background(), costParams(seat))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProtocolTotal{}
	for _, r := range rows {
		got[r.ProtocolType] = r
	}

	a := got["anthropic"]
	if !approxEq(a.CostUSD, 0.005) {
		t.Errorf("anthropic cost_usd = %v, want 0.005", a.CostUSD)
	}
	if a.PricedRequestCount != 2 || a.UnpricedRequestCount != 2 {
		t.Errorf("anthropic priced/unpriced = %d/%d, want 2/2", a.PricedRequestCount, a.UnpricedRequestCount)
	}
	if a.PricedRequestCount+a.UnpricedRequestCount != a.RequestCount {
		t.Errorf("anthropic priced+unpriced (%d) != request_count (%d)", a.PricedRequestCount+a.UnpricedRequestCount, a.RequestCount)
	}

	o := got["openai"]
	if !approxEq(o.CostUSD, 0) {
		t.Errorf("openai cost_usd = %v, want 0 (EUR row excluded)", o.CostUSD)
	}
	if o.PricedRequestCount != 1 || o.UnpricedRequestCount != 1 {
		t.Errorf("openai priced/unpriced = %d/%d, want 1/1 (EUR row is still priced)", o.PricedRequestCount, o.UnpricedRequestCount)
	}
}

func TestPersonalByKeyTotal_Cost(t *testing.T) {
	db := setupUsageTestDB(t)
	seat := "seatKey"
	insertDWD(t, db, dwdRow{EventID: "k1", OrgID: "org1", SeatID: seat, VirtualKeyID: "vk-1", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.004)})
	insertDWD(t, db, dwdRow{EventID: "k2", OrgID: "org1", SeatID: seat, VirtualKeyID: "vk-1", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.001)})
	insertDWD(t, db, dwdRow{EventID: "k3", OrgID: "org1", SeatID: seat, VirtualKeyID: "vk-1", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1 /* unpriced */})

	rows, err := NewSQLRepository(db).PersonalByKeyTotal(context.Background(), costParams(seat))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 key row, got %d", len(rows))
	}
	k := rows[0]
	if !approxEq(k.CostUSD, 0.005) {
		t.Errorf("key cost_usd = %v, want 0.005", k.CostUSD)
	}
	if k.PricedRequestCount != 2 || k.UnpricedRequestCount != 1 {
		t.Errorf("key priced/unpriced = %d/%d, want 2/1", k.PricedRequestCount, k.UnpricedRequestCount)
	}
}

func TestPersonalByModelTotal_Cost(t *testing.T) {
	db := setupUsageTestDB(t)
	seat := "seatModel"
	insertDWD(t, db, dwdRow{EventID: "m1", OrgID: "org1", SeatID: seat, Model: "claude-opus-4", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.006)})
	insertDWD(t, db, dwdRow{EventID: "m2", OrgID: "org1", SeatID: seat, Model: "gpt-4o", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 3 /* unpriced */})

	rows, err := NewSQLRepository(db).PersonalByModelTotal(context.Background(), costParams(seat))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ModelTotal{}
	for _, r := range rows {
		got[r.Model] = r
	}
	if c := got["claude-opus-4"]; !approxEq(c.CostUSD, 0.006) || c.PricedRequestCount != 1 || c.UnpricedRequestCount != 0 {
		t.Errorf("claude-opus-4 = cost %v priced %d unpriced %d, want 0.006/1/0", c.CostUSD, c.PricedRequestCount, c.UnpricedRequestCount)
	}
	if g := got["gpt-4o"]; !approxEq(g.CostUSD, 0) || g.PricedRequestCount != 0 || g.UnpricedRequestCount != 3 {
		t.Errorf("gpt-4o = cost %v priced %d unpriced %d, want 0/0/3", g.CostUSD, g.PricedRequestCount, g.UnpricedRequestCount)
	}
}

func TestPersonalByAppTotal_Cost(t *testing.T) {
	db := setupUsageTestDB(t)
	seat := "seatApp"
	insertDWD(t, db, dwdRow{EventID: "a1", OrgID: "org1", SeatID: seat, AppSlug: "degrade-detector", ProviderCode: "anthropic", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.007)})
	insertDWD(t, db, dwdRow{EventID: "a2", OrgID: "org1", SeatID: seat, AppSlug: "degrade-detector", ProviderCode: "anthropic", EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1 /* unpriced */})

	rows, err := NewSQLRepository(db).PersonalByAppTotal(context.Background(), costParams(seat))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 app row, got %d", len(rows))
	}
	if r := rows[0]; !approxEq(r.CostUSD, 0.007) || r.PricedRequestCount != 1 || r.UnpricedRequestCount != 1 {
		t.Errorf("app = cost %v priced %d unpriced %d, want 0.007/1/1", r.CostUSD, r.PricedRequestCount, r.UnpricedRequestCount)
	}
}

func TestPersonalTimeline_Cost(t *testing.T) {
	db := setupUsageTestDB(t)
	seat := "seatTL"
	insertDWD(t, db, dwdRow{EventID: "t1", OrgID: "org1", SeatID: seat, EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.008)})
	insertDWD(t, db, dwdRow{EventID: "t2", OrgID: "org1", SeatID: seat, EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1, BillableAmount: costPtr(0.002)})
	insertDWD(t, db, dwdRow{EventID: "t3", OrgID: "org1", SeatID: seat, EventTimeMs: noonMs, UsageDate: "2026-06-01", TotalTokens: 10, RequestCount: 1 /* unpriced contributes 0 cost */})

	rows, err := NewSQLRepository(db).PersonalTimeline(context.Background(), costParams(seat))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 timeline bucket, got %d", len(rows))
	}
	if !approxEq(rows[0].CostUSD, 0.010) {
		t.Errorf("timeline cost_usd = %v, want 0.010", rows[0].CostUSD)
	}
}
