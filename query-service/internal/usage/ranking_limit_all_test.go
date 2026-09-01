package usage

import (
	"context"
	"testing"
	"time"
)

// A ranking is a TOP-N list; the cost ledger's per-department table is a
// CONSERVING TOTAL. Both are served by MasterUserRanking, so the query has to
// be able to answer the second question too — Σ(rows) must equal what the
// organisation actually spent.
//
// 🔴 Why this fence exists: the by-department table shipped reading the
// ranking chart's own top-20 result. Every department figure was then a
// fraction of the truth, the conservation row could not balance for ANY N, and
// the shortfall reads on screen as "those departments spent less" rather than
// as a truncated list. Caught on staging: the table added to $7.2230 against
// an organisation total of $11.51.
//
// bugfix: workflow/CI/bugfix/2026-08-27-ledger-by-department-top-n-truncation.md
func TestMasterUserRanking_LimitAllReturnsEveryRow(t *testing.T) {
	db := setupUsageTestDB(t)
	start, _ := time.Parse("2006-01-02", "2026-07-01")
	end, _ := time.Parse("2006-01-02", "2026-07-31")

	const seats = 7
	for i := 0; i < seats; i++ {
		insertDWD(t, db, dwdRow{
			EventID: "ev-" + string(rune('a'+i)), OrgID: "org1",
			SeatID:       "seat-" + string(rune('a'+i)),
			ProviderCode: "openai", ProtocolType: "openai_compatible",
			Model: "gpt-5.6", EventTimeMs: msAt("2026-07-14T10:00:00Z"),
			UsageDate: "2026-07-14", TotalTokens: int64(100 * (i + 1)), RequestCount: 1,
			BillingScope: "org_and_user", UserUsageScope: "normal",
		})
	}
	repo := NewSQLRepository(db)
	params := func(limit int) QueryParams {
		return QueryParams{OrgID: "org1", StartDate: start, EndDate: end, Limit: limit}
	}

	// The top-N behaviour the ranking chart depends on is unchanged.
	top, err := repo.MasterUserRanking(context.Background(), params(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Fatalf("limit=3: want 3 rows, got %d", len(top))
	}

	all, err := repo.MasterUserRanking(context.Background(), params(LimitAll))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != seats {
		t.Fatalf("LimitAll: want all %d rows, got %d — a truncated list cannot "+
			"produce a conserving per-department total", seats, len(all))
	}

	// 🔴 The point of the whole thing: the rows add up to the real total.
	// Asserted on the sum rather than on the row count, because "we got N
	// rows" is satisfied by any N large enough on this fixture and would stop
	// failing the day somebody reintroduced a generous-but-finite cap.
	var wantTokens int64
	for i := 0; i < seats; i++ {
		wantTokens += int64(100 * (i + 1))
	}
	var gotTokens int64
	for _, r := range all {
		gotTokens += r.TotalTokens
	}
	if gotTokens != wantTokens {
		t.Errorf("LimitAll: Σtokens = %d, organisation actually spent %d", gotTokens, wantTokens)
	}
}

// Defaults() must not swallow LimitAll. It is a deliberate value, not "unset";
// the old `if q.Limit <= 0 { q.Limit = 50 }` would have turned "give me
// everything" into "give me fifty" with nothing to show for it.
func TestQueryParamsDefaults_PreservesLimitAll(t *testing.T) {
	q := QueryParams{Limit: LimitAll}
	q.Defaults()
	if q.Limit != LimitAll {
		t.Errorf("Defaults() rewrote LimitAll to %d — asking for every row would silently cap", q.Limit)
	}
	var unset QueryParams
	unset.Defaults()
	if unset.Limit != 50 {
		t.Errorf("an unset Limit must still default to 50, got %d", unset.Limit)
	}
}

// 🔴 The ledger's population must be the SAME one the organisation total is
// computed from. Two ways to lose money were found on staging on the same day:
// a truncated list (LimitAll fixes it) and the wrong population (this fixes
// it). Fixing one alone still leaves the conservation row unbalanced — the
// stats rule drops abnormal/org_only rows, $0.7427 of $11.5059 there.
//
// This pins the two WHERE fragments together: if MasterTimeline's filter is
// edited and the ranking's org-billing filter is not, this goes red rather
// than the console quietly under-reporting again.
func TestLedgerPopulationMatchesTimeline(t *testing.T) {
	db := setupUsageTestDB(t)
	start, _ := time.Parse("2006-01-02", "2026-07-01")
	end, _ := time.Parse("2006-01-02", "2026-07-31")

	rows := []struct {
		id, seat, scope, billing string
		tokens                   int64
	}{
		{"ev-normal", "seat-a", "normal", "org_and_user", 1000},
		// The row the stats rule drops and the organisation total keeps.
		{"ev-abnormal", "seat-b", "abnormal", "org_only", 700},
		// Kept by neither: not org-billed.
		{"ev-held", "seat-c", "excluded", "hold_for_review", 40},
		// Kept by neither: not generation traffic.
		{"ev-probe", "seat-d", "non_generation", "org_and_user", 5},
	}
	for _, r := range rows {
		insertDWD(t, db, dwdRow{
			EventID: r.id, OrgID: "org1", SeatID: r.seat,
			ProviderCode: "openai", ProtocolType: "openai_compatible",
			Model: "gpt-5.6", EventTimeMs: msAt("2026-07-14T10:00:00Z"),
			UsageDate: "2026-07-14", TotalTokens: r.tokens, RequestCount: 1,
			BillingScope: r.billing, UserUsageScope: r.scope,
		})
	}
	repo := NewSQLRepository(db)
	base := QueryParams{OrgID: "org1", StartDate: start, EndDate: end, Limit: LimitAll}

	tl, err := repo.MasterTimeline(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	var orgTotal int64
	for _, p := range tl {
		orgTotal += p.TotalTokens
	}

	billing := base
	billing.RankingScope = RankingScopeOrgBilling
	perSeat, err := repo.MasterUserRanking(context.Background(), billing)
	if err != nil {
		t.Fatal(err)
	}
	var summed int64
	for _, r := range perSeat {
		summed += r.TotalTokens
	}
	if summed != orgTotal {
		t.Errorf("Σ per-seat (org_billing) = %d but the organisation total is %d — "+
			"the cost-by-department table cannot balance against a total computed "+
			"from a different population", summed, orgTotal)
	}
	if orgTotal != 1700 {
		t.Errorf("fixture drifted: organisation total = %d, want 1700 (normal + abnormal/org_only)", orgTotal)
	}

	// And the default population is still the chart's, unchanged.
	stats, err := repo.MasterUserRanking(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	var statsTotal int64
	for _, r := range stats {
		statsTotal += r.TotalTokens
	}
	if statsTotal != 1000 {
		t.Errorf("the default (stats) population changed: %d, want 1000 — the "+
			"existing top-users chart must not move", statsTotal)
	}
}
