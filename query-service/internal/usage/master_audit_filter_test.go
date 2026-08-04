package usage

import (
	"context"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

// 20260729 用量审计页自由筛选 — fence tests for the master audit filter
// push-down (masterAuditWhere dimension registry).
//
// Four rows, one org, one day, on the REAL baseline schema, differing in every
// filter dimension so each dimension provably narrows on its own column:
//
//	ev-a  seat1 credA anthropic claude-x  exact    priced   vk1 anthropic_native
//	ev-b  seat2 credB openai    gpt-5.6   invalid  unpriced vk2 openai_compatible (anomaly token_spike)
//	ev-c  seat1 credB anthropic claude-x  partial  unpriced vk3 anthropic_native
//	ev-p  probe row (non_generation)      — must NEVER appear, filtered or not
//
// Pinned rules:
//   - no filters → all three audit rows (backward compat: an old FE that sends
//     no params sees exactly the pre-filter behaviour)
//   - each dimension narrows independently; dimensions AND-combine
//   - detail and export share masterAuditWhere → identical row sets for the
//     same params (the "screen scope == CSV scope" invariant)
func TestMasterAuditFilters(t *testing.T) {
	db := setupUsageTestDB(t)
	seedOrgSeatsFixture(t, db)

	day := "2026-07-20"
	ts := msAt("2026-07-20T10:00:00Z")
	priced := 0.5
	insertDWD(t, db, dwdRow{
		EventID: "ev-a", OrgID: "org1", SeatID: "seat1", CredentialID: "credA",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk1", QualityStatus: "exact",
		OAuthIdentity:  "alice@pool.example",
		BillableAmount: &priced,
		EventTimeMs:    ts, UsageDate: day, TotalTokens: 100,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-b", OrgID: "org1", SeatID: "seat2", CredentialID: "credB",
		ProviderCode: "openai", ProtocolType: "openai_compatible",
		Model: "gpt-5.6", VirtualKeyID: "vk2", QualityStatus: "invalid",
		AnomalyType: "token_spike",
		EventTimeMs: ts, UsageDate: day, TotalTokens: 200,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-c", OrgID: "org1", SeatID: "seat1", CredentialID: "credB",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk3", QualityStatus: "partial",
		EventTimeMs: ts, UsageDate: day, TotalTokens: 300,
	})
	insertDWD(t, db, dwdRow{
		EventID: "ev-p", OrgID: "org1", SeatID: "seat1", CredentialID: "credA",
		ProviderCode: "anthropic", ProtocolType: "anthropic_native",
		Model: "claude-x", VirtualKeyID: "vk1",
		UserUsageScope: "non_generation",
		EventTimeMs:    ts, UsageDate: day, TotalTokens: 0,
	})

	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", day)
	base := QueryParams{OrgID: "org1", StartDate: start, EndDate: start, Limit: 100}

	cases := []struct {
		name string
		mut  func(*QueryParams)
		want []string
	}{
		{"no filters (backward compat)", func(p *QueryParams) {}, []string{"ev-a", "ev-b", "ev-c"}},
		{"seat", func(p *QueryParams) { p.SeatID = "seat1" }, []string{"ev-a", "ev-c"}},
		// oauth_identity narrows to the one row carrying that pool-account
		// email; rows with NULL identity (ev-b/ev-c) must not match.
		{"oauth identity", func(p *QueryParams) { p.OAuthIdentity = "alice@pool.example" }, []string{"ev-a"}},
		{"credential", func(p *QueryParams) { p.CredentialID = "credB" }, []string{"ev-b", "ev-c"}},
		{"provider", func(p *QueryParams) { p.ProviderCode = "openai" }, []string{"ev-b"}},
		{"model", func(p *QueryParams) { p.Model = "claude-x" }, []string{"ev-a", "ev-c"}},
		{"quality", func(p *QueryParams) { p.QualityStatus = "invalid" }, []string{"ev-b"}},
		{"virtual key", func(p *QueryParams) { p.VirtualKeyID = "vk1" }, []string{"ev-a"}},
		{"protocol", func(p *QueryParams) { p.Protocol = "openai_compatible" }, []string{"ev-b"}},
		{"anomaly", func(p *QueryParams) { p.AnomalyType = "token_spike" }, []string{"ev-b"}},
		{"billing priced", func(p *QueryParams) { p.Billing = "priced" }, []string{"ev-a"}},
		{"billing unpriced", func(p *QueryParams) { p.Billing = "unpriced" }, []string{"ev-b", "ev-c"}},
		{"AND combine seat+unpriced", func(p *QueryParams) { p.SeatID = "seat1"; p.Billing = "unpriced" }, []string{"ev-c"}},
		{"AND combine provider+model+quality", func(p *QueryParams) {
			p.ProviderCode = "anthropic"
			p.Model = "claude-x"
			p.QualityStatus = "exact"
		}, []string{"ev-a"}},
		{"filter matching nothing", func(p *QueryParams) { p.SeatID = "seat-none" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mut(&p)
			detail, err := repo.MasterUsageDetail(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			assertEventIDs(t, "detail", detail, tc.want)

			// Same params through export MUST yield the same row set —
			// detail and export share masterAuditWhere by construction, and
			// this fence keeps a future refactor from splitting them.
			var exported []MasterUsageAuditRow
			if err := repo.StreamMasterUsageExport(context.Background(), p, func(a *MasterUsageAuditRow) error {
				exported = append(exported, *a)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertEventIDs(t, "export", exported, tc.want)
		})
	}
}

// 20260729 查询分页 — fence for TRUE server pagination + keyword + facets.
// Same 4-row fixture as TestMasterAuditFilters (3 audit rows + 1 probe).
func TestMasterAuditPagination(t *testing.T) {
	db := setupUsageTestDB(t)
	seedOrgSeatsFixture(t, db)
	day := "2026-07-20"
	ts := msAt("2026-07-20T10:00:00Z")
	for i, ev := range []string{"ev-1", "ev-2", "ev-3", "ev-4", "ev-5"} {
		insertDWD(t, db, dwdRow{
			EventID: ev, OrgID: "org1", SeatID: "seat1", ProviderCode: "anthropic",
			ProtocolType: "anthropic_native", Model: "claude-x", VirtualKeyID: "vk1",
			OAuthIdentity: "alice@pool.example",
			EventTimeMs:   ts + int64(i)*1000, UsageDate: day, TotalTokens: 100,
		})
	}
	repo := NewSQLRepository(db)
	start, _ := time.Parse("2006-01-02", day)
	base := QueryParams{OrgID: "org1", StartDate: start, EndDate: start}

	// Page walk: limit 2 → pages of 2/2/1, newest first, no overlap, no gap.
	seen := map[string]bool{}
	for _, off := range []int{0, 2, 4} {
		p := base
		p.Limit, p.Offset = 2, off
		rows, err := repo.MasterUsageDetail(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		wantLen := 2
		if off == 4 {
			wantLen = 1
		}
		if len(rows) != wantLen {
			t.Fatalf("offset %d: got %d rows, want %d", off, len(rows), wantLen)
		}
		for _, r := range rows {
			if seen[r.EventID] {
				t.Fatalf("offset %d: duplicate row %s across pages", off, r.EventID)
			}
			seen[r.EventID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("page walk covered %d rows, want 5", len(seen))
	}

	// Total reflects the FULL scope regardless of limit/offset.
	p := base
	p.Limit, p.Offset = 2, 2
	total, err := repo.MasterUsageDetailTotal(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}

	// Keyword narrows all three entry points identically (shared WHERE):
	// "alice" hits oauth_identity; "zzz" hits nothing.
	kw := base
	kw.Limit = 100
	kw.Keyword = "ALICE" // case-insensitive
	if rows, err := repo.MasterUsageDetail(context.Background(), kw); err != nil || len(rows) != 5 {
		t.Fatalf("keyword alice: rows=%d err=%v, want 5", len(rows), err)
	}
	kw.Keyword = "zzz-no-match"
	if n, err := repo.MasterUsageDetailTotal(context.Background(), kw); err != nil || n != 0 {
		t.Fatalf("keyword no-match total=%d err=%v, want 0", n, err)
	}
	// LIKE metacharacters are literals, not wildcards.
	kw.Keyword = "%"
	if n, err := repo.MasterUsageDetailTotal(context.Background(), kw); err != nil || n != 0 {
		t.Fatalf("keyword %% must match literally: total=%d err=%v, want 0", n, err)
	}

	// Server-side sort (20260801): whitelist keys order the FULL scope, ties
	// break on the stable event_time/event_id tail so pages never overlap.
	srt := base
	srt.Limit, srt.SortBy, srt.SortDir = 100, "tokens", "asc"
	rowsAsc, err := repo.MasterUsageDetail(context.Background(), srt)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rowsAsc); i++ {
		if rowsAsc[i-1].TotalTokens > rowsAsc[i].TotalTokens {
			t.Fatalf("tokens asc out of order: %d before %d", rowsAsc[i-1].TotalTokens, rowsAsc[i].TotalTokens)
		}
	}
	srt.SortDir = "desc"
	rowsDesc, err := repo.MasterUsageDetail(context.Background(), srt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsDesc) == 0 || rowsDesc[0].TotalTokens < rowsDesc[len(rowsDesc)-1].TotalTokens {
		t.Fatalf("tokens desc not descending")
	}
	// Sorted pagination stays gap/overlap-free: walk pages of 2 under a sort
	// with ALL-TIED keys (every row has identical tokens=100 in this fixture's
	// first 5 events → the stable tiebreaker is doing the work).
	srt2 := base
	srt2.SortBy, srt2.SortDir = "tokens", "asc"
	seenSorted := map[string]bool{}
	for off := 0; off < 6; off += 2 {
		p := srt2
		p.Limit, p.Offset = 2, off
		pg, err := repo.MasterUsageDetail(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pg {
			if seenSorted[r.EventID] {
				t.Fatalf("sorted page walk: duplicate %s at offset %d", r.EventID, off)
			}
			seenSorted[r.EventID] = true
		}
	}
	if len(seenSorted) != 5 {
		t.Fatalf("sorted page walk covered %d rows, want 5", len(seenSorted))
	}
	if !ValidMasterAuditSortKey("model") || ValidMasterAuditSortKey("evil; DROP") {
		t.Fatal("sort-key whitelist broken")
	}

	// Facets: distinct row-derived dimension values under the same scope.
	facets, err := repo.MasterUsageDetailFacets(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(facets["identity"]) != 1 || facets["identity"][0] != "alice@pool.example" {
		t.Fatalf("identity facet = %v", facets["identity"])
	}
	if len(facets["model"]) != 1 || facets["model"][0] != "claude-x" {
		t.Fatalf("model facet = %v", facets["model"])
	}

	// Self-exclusion (user report 2026-07-29: chip-click showed only the
	// selected value): with an identity filter APPLIED, the identity facet
	// must still list the alternatives, while other facets keep narrowing.
	insertDWD(t, db, dwdRow{
		EventID: "ev-bob", OrgID: "org1", SeatID: "seat2", ProviderCode: "openai",
		ProtocolType: "openai_compatible", Model: "gpt-x", VirtualKeyID: "vk2",
		OAuthIdentity: "bob@pool.example",
		EventTimeMs:   ts + 9000, UsageDate: day, TotalTokens: 100,
	})
	sel := base
	sel.OAuthIdentity = "alice@pool.example"
	facets2, err := repo.MasterUsageDetailFacets(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(facets2["identity"]) != 2 {
		t.Fatalf("identity facet with own filter applied must show alternatives, got %v", facets2["identity"])
	}
	if len(facets2["model"]) != 1 || facets2["model"][0] != "claude-x" {
		t.Fatalf("model facet must stay narrowed by the identity filter, got %v", facets2["model"])
	}
}

func assertEventIDs(t *testing.T, label string, rows []MasterUsageAuditRow, want []string) {
	t.Helper()
	got := map[string]bool{}
	for _, r := range rows {
		if r.EventID == "ev-p" {
			t.Errorf("%s: non_generation probe row leaked into audit result", label)
		}
		got[r.EventID] = true
	}
	if len(rows) != len(want) {
		t.Fatalf("%s: got %d rows (%v), want %d (%v)", label, len(rows), got, len(want), want)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("%s: missing expected row %s (got %v)", label, id, got)
		}
	}
}

// seedOrgSeatsFixture mirrors the org_seats control-plane DDL the audit JOIN
// needs — same convention as scope_filter_test.go (see the DDL provenance
// comment there):
//
//	aikey-control-master/service/migrations/001_baseline_schema.sql (org_seats)
//	aikey-config-tool/pkg/dbmigrate/versions_master/v1_0_1_alpha1_digital_employees.go (alias)
func seedOrgSeatsFixture(t *testing.T, db *shared.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS org_seats (
			seat_id       TEXT NOT NULL PRIMARY KEY,
			org_id        TEXT NOT NULL,
			account_id    TEXT,
			invited_email TEXT NOT NULL,
			seat_status   TEXT NOT NULL DEFAULT 'pending_claim',
			claimed_at    TIMESTAMPTZ,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT (datetime('now')),
			CONSTRAINT uq_org_seats_org_email UNIQUE (org_id, invited_email)
		)`,
		`ALTER TABLE org_seats ADD COLUMN seat_type TEXT NOT NULL DEFAULT 'human'`,
		`ALTER TABLE org_seats ADD COLUMN alias TEXT`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("org_seats fixture DDL: %v", err)
		}
	}
	if _, err := db.DB.Exec(
		`INSERT INTO org_seats (seat_id, org_id, invited_email, alias) VALUES
			('seat1','org1','a@corp','Alice'), ('seat2','org1','b@corp','Bob')`,
	); err != nil {
		t.Fatalf("seed org_seats: %v", err)
	}
}

// TestMasterAuditSortKeys_MatchTheColumnsTheTableOffers pins the whitelist as a
// SET, not by spot-checking one member.
//
// The console's table header carries the comment "keys mirror the backend sort
// whitelist" and nothing enforces it across the two repos. The failure is
// asymmetric and worth naming: a key the FE offers but the backend rejects is a
// loud 400 someone fixes that afternoon, while a key the backend accepts and the
// FE never sends is dead code that reads as coverage. Listing the set here means
// adding a sortable column is a decision recorded in both places, and dropping
// one cannot happen silently on either side.
func TestMasterAuditSortKeys_MatchTheColumnsTheTableOffers(t *testing.T) {
	want := map[string]bool{
		"time": true, "seat": true, "identity": true, "provider": true,
		"model": true, "tokens": true, "cost": true, "quality": true,
		// 2026-08-04: the upstream-failover column. An admin sorts this to pull
		// every stepped-around request together; without it the column is the
		// only unsortable header on the page.
		"failover": true,
	}
	for k := range want {
		if !ValidMasterAuditSortKey(k) {
			t.Errorf("the table offers a %q header but the backend rejects that sort key — "+
				"the request 400s the moment a user clicks it", k)
		}
	}
	for k := range masterAuditSortColumns {
		if !want[k] {
			t.Errorf("the backend accepts sort key %q that no table header offers — "+
				"either add the column or drop the entry; an unreachable branch reads as coverage", k)
		}
	}
	if ValidMasterAuditSortKey("evil; DROP") {
		t.Fatal("the whitelist must reject anything not listed — it is the injection boundary")
	}
}

// NULL and 0 may be collapsed for ORDERING and nowhere else.
//
// `failover` maps to COALESCE(d.fallback_attempt, 0) so "no chain" ranks below
// "the primary served it" and a DESC sort surfaces the deepest fallbacks first.
// That is a statement about row order. Every rendering path — the drawer, the
// CSV, the table cell — must still tell the two apart, because one means "no hop
// number was ever measured" and the other is a measurement.
func TestMasterAuditSort_FailoverExpressionOrdersNullBelowPrimary(t *testing.T) {
	expr, ok := masterAuditSortColumns["failover"]
	if !ok {
		t.Fatal("failover is not a sortable column")
	}
	if expr != "COALESCE(d.fallback_attempt, 0)" {
		t.Fatalf("failover sorts on %q.\n"+
			"Sorting on the bare column pushes NULLs to one end in a dialect-dependent way "+
			"(Postgres puts them last on ASC, SQLite first), so the page would order "+
			"differently on Trial and Production for the same data", expr)
	}
}
