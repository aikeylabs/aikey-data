package conversation

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	_ "modernc.org/sqlite"
)

// Fence tests for the paginated seat / session lists (2026-07-26).
//
// WHY these exist: the lists gained a TOTAL alongside the page so the console can
// render page numbers instead of guessing "there may be more" from
// len(page) >= pageSize (which offered a dead "next" on exact multiples of the
// page size). A total is only useful if it counts the SAME THING the page pages
// over — so the invariants worth fencing are:
//
//	(1) total counts GROUPS (seats / sessions), not rows;
//	(2) total uses the same grouping expression as the page — notably seatKeyExpr,
//	    where a row's seat is seat_id with an owner_account_id fallback. Counting
//	    by raw owner_account_id here is the exact regression that would put
//	    "共 3 条" above a list that bottoms out at 2;
//	(3) paging is a partition: pages are disjoint and their union is total;
//	(4) the conv_date filter moves total and items together.
//
// Schema comes from the REAL dbmigrate registry, never a hand-rolled CREATE TABLE
// (test-fixture-real-schema principle) — conversation_records is not in
// aikey-data/baseline, it ships as the v1.0.1-alpha.2 data migration. Mirrors
// collector-service/internal/conversation/conversation_test.go::newConvTestDB.
func newConvTestDB(t *testing.T) *shared.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), db,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate data component: %v", err)
	}
	return shared.NewDB(db, shared.DialectSQLite)
}

// insertTurn writes one conversation_records row. seatID "" stores NULL-ish empty
// (the legacy shape from pre-alpha.4 proxies) so seatKeyExpr must fall back to
// ownerAccountID — the mixed-shape case invariant (2) is about.
func insertTurn(t *testing.T, db *shared.DB, eventID, convDate, org, sessionID, seatID, owner string, tokens int64, createdAt int64) {
	t.Helper()
	var sess any
	if sessionID != "" {
		sess = sessionID
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO conversation_records
			(event_id, conv_date, org_id, session_id, seat_id, owner_account_id,
			 user_text, assistant_text, total_tokens, content_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, convDate, org, sess, seatID, owner,
		"Q-"+eventID, "A-"+eventID, tokens, len(eventID)*2, createdAt)
	if err != nil {
		t.Fatalf("insert %s: %v", eventID, err)
	}
}

const testOrg = "org-fence"

func TestSeatSummariesTotalCountsSeatsNotRows(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	// 3 distinct SEATS across 6 rows. seat-b arrives with seat_id set (>=alpha.4
	// proxy); seat-a and seat-c only carry owner_account_id (legacy rows). A total
	// computed from raw owner_account_id would see 3 too — so seat-b deliberately
	// carries a DIFFERENT owner_account_id than its seat_id, which is what a
	// shared OAuth-pool VK looks like: counting by owner would fold it into
	// "pool-creator" and report 2.
	insertTurn(t, db, "e1", "2026-07-20", testOrg, "s1", "", "seat-a", 10, 1000)
	insertTurn(t, db, "e2", "2026-07-20", testOrg, "s1", "", "seat-a", 20, 2000)
	insertTurn(t, db, "e3", "2026-07-20", testOrg, "s2", "seat-b", "pool-creator", 30, 3000)
	insertTurn(t, db, "e4", "2026-07-20", testOrg, "s2", "seat-b", "pool-creator", 40, 4000)
	insertTurn(t, db, "e5", "2026-07-20", testOrg, "s3", "", "seat-c", 50, 5000)
	insertTurn(t, db, "e6", "2026-07-20", testOrg, "s3", "", "pool-creator", 60, 6000)

	// Seats: seat-a, seat-b (via seat_id), pool-creator (e6 has no seat_id), seat-c = 4
	items, total, err := repo.SeatSummaries(ctx, QueryParams{OrgID: testOrg, Limit: 100})
	if err != nil {
		t.Fatalf("SeatSummaries: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (distinct seats, not 6 rows)", total)
	}
	if int64(len(items)) != total {
		t.Errorf("unpaged len(items) = %d but total = %d — count and page disagree "+
			"about what a seat is (check countGroups uses seatKeyExpr)", len(items), total)
	}
}

func TestSeatSummariesPagingIsAPartition(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	const seats = 7
	for i := 0; i < seats; i++ {
		id := fmt.Sprintf("seat-%02d", i)
		insertTurn(t, db, "ev-"+id, "2026-07-20", testOrg, "s-"+id, "", id, int64(i+1)*10, int64(i+1)*1000)
	}

	// Walk every page of 3 and prove: total is stable, pages are disjoint, and the
	// union is exactly total. This is the property a page-number UI depends on —
	// "第 3/3 页" must not hide a row or show a phantom one.
	seen := map[string]bool{}
	var firstTotal int64 = -1
	for offset := 0; offset < seats+3; offset += 3 {
		items, total, err := repo.SeatSummaries(ctx, QueryParams{OrgID: testOrg, Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("SeatSummaries offset=%d: %v", offset, err)
		}
		if firstTotal == -1 {
			firstTotal = total
		} else if total != firstTotal {
			t.Errorf("offset=%d: total = %d, want stable %d", offset, total, firstTotal)
		}
		for _, it := range items {
			if seen[it.OwnerAccountID] {
				t.Errorf("offset=%d: seat %q appeared on more than one page", offset, it.OwnerAccountID)
			}
			seen[it.OwnerAccountID] = true
		}
	}
	if firstTotal != seats {
		t.Errorf("total = %d, want %d", firstTotal, seats)
	}
	if int64(len(seen)) != firstTotal {
		t.Errorf("paged through %d distinct seats but total says %d", len(seen), firstTotal)
	}

	// The empty-page guard: at offset == total the page is empty while total is
	// unchanged. This is what lets the console disable "next" instead of offering
	// a click that lands on nothing.
	items, total, err := repo.SeatSummaries(ctx, QueryParams{OrgID: testOrg, Limit: 3, Offset: seats})
	if err != nil {
		t.Fatalf("SeatSummaries past end: %v", err)
	}
	if len(items) != 0 || total != seats {
		t.Errorf("past-end page: len=%d total=%d, want 0/%d", len(items), total, seats)
	}
}

func TestSessionSummariesTotalCountsSessionsIncludingSessionless(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	// One seat: 2 real multi-turn sessions + 2 sessionless turns. Sessionless rows
	// group by their own event_id (decision B, 2026-06-17), so they are 2 separate
	// pseudo-sessions — the total must agree with that, not collapse them into one.
	insertTurn(t, db, "a1", "2026-07-20", testOrg, "sess-1", "", "seat-x", 10, 1000)
	insertTurn(t, db, "a2", "2026-07-20", testOrg, "sess-1", "", "seat-x", 10, 2000)
	insertTurn(t, db, "a3", "2026-07-20", testOrg, "sess-2", "", "seat-x", 10, 3000)
	insertTurn(t, db, "a4", "2026-07-20", testOrg, "", "", "seat-x", 10, 4000)
	insertTurn(t, db, "a5", "2026-07-20", testOrg, "", "", "seat-x", 10, 5000)

	items, total, err := repo.SessionSummaries(ctx, QueryParams{
		OrgID: testOrg, OwnerAccountID: "seat-x", Limit: 100,
	})
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (sess-1, sess-2, a4, a5)", total)
	}
	if int64(len(items)) != total {
		t.Errorf("unpaged len(items) = %d but total = %d", len(items), total)
	}
}

func TestListTotalsHonorDateFilter(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	insertTurn(t, db, "d1", "2026-07-01", testOrg, "s1", "", "seat-old", 10, 1000)
	insertTurn(t, db, "d2", "2026-07-20", testOrg, "s2", "", "seat-new", 10, 2000)
	insertTurn(t, db, "d3", "2026-07-21", testOrg, "s3", "", "seat-new", 10, 3000)

	// A total that ignored the conv_date bounds would read "共 2 条" while the list
	// shows 1 — the filter must move both together.
	items, total, err := repo.SeatSummaries(ctx, QueryParams{
		OrgID: testOrg, StartDate: "2026-07-15", EndDate: "2026-07-31", Limit: 100,
	})
	if err != nil {
		t.Fatalf("SeatSummaries filtered: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Errorf("date-filtered seats: len=%d total=%d, want 1/1", len(items), total)
	}

	sItems, sTotal, err := repo.SessionSummaries(ctx, QueryParams{
		OrgID: testOrg, OwnerAccountID: "seat-new", StartDate: "2026-07-21", EndDate: "2026-07-31", Limit: 100,
	})
	if err != nil {
		t.Fatalf("SessionSummaries filtered: %v", err)
	}
	if sTotal != 1 || len(sItems) != 1 {
		t.Errorf("date-filtered sessions: len=%d total=%d, want 1/1", len(sItems), sTotal)
	}
}

func TestListTotalsAreTenantScoped(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	insertTurn(t, db, "t1", "2026-07-20", testOrg, "s1", "", "seat-mine", 10, 1000)
	insertTurn(t, db, "t2", "2026-07-20", "org-other", "s2", "", "seat-theirs", 10, 2000)
	insertTurn(t, db, "t3", "2026-07-20", "org-other", "s3", "", "seat-theirs2", 10, 3000)

	// Tenant isolation is already fenced for the page; the total must not leak a
	// count across orgs either (a "共 3 条" over a 1-row list is an info leak, not
	// just a cosmetic bug).
	items, total, err := repo.SeatSummaries(ctx, QueryParams{OrgID: testOrg, Limit: 100})
	if err != nil {
		t.Fatalf("SeatSummaries: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Errorf("tenant-scoped: len=%d total=%d, want 1/1", len(items), total)
	}
}

// Fences for the seat-list search filter (2026-07-31). The facade resolves
// ?q=<name> against its org_seats directory and sends BOTH the resolved keys and
// the RAW term, OR'd here. Invariants:
//
//	(1) a resolved key matches via seatKeyExpr — seat_id rows and legacy
//	    owner-only rows alike;
//	(2) the raw term matches the seat KEY as a case-insensitive substring, which
//	    is what makes a seat with NO directory entry searchable by the very label
//	    the console shows for it (regression: "demo-alice" visible in the list but
//	    unfindable, reported 2026-07-31);
//	(3) total moves WITH the filter (count and page share the where clause);
//	(4) searched-but-nothing-to-match-on is an EMPTY page, not the unfiltered list;
//	(5) LIKE metacharacters in the term are literal text, not wildcards.
func TestSeatSummariesSeatSearchFilter(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	ctx := context.Background()

	// seat-b is a >=alpha.4 shape (seat_id set, owner = pool creator); the rest
	// are legacy owner-only rows. demo-alice stands for a key with NO org_seats
	// row — the console renders it by its raw key.
	insertTurn(t, db, "k1", "2026-07-20", testOrg, "s1", "", "seat-a", 10, 1000)
	insertTurn(t, db, "k2", "2026-07-20", testOrg, "s2", "seat-b", "pool-creator", 20, 2000)
	insertTurn(t, db, "k3", "2026-07-20", testOrg, "s3", "", "demo-alice", 30, 3000)
	insertTurn(t, db, "k4", "2026-07-20", testOrg, "s4", "", "demo-bob", 40, 4000)

	search := func(t *testing.T, p QueryParams) ([]SeatSummary, int64) {
		t.Helper()
		p.OrgID, p.Limit = testOrg, 100
		items, total, err := repo.SeatSummaries(ctx, p)
		if err != nil {
			t.Fatalf("SeatSummaries: %v", err)
		}
		if int64(len(items)) != total {
			t.Errorf("len(items)=%d but total=%d — count and page disagree", len(items), total)
		}
		return items, total
	}

	t.Run("resolved keys match both row generations", func(t *testing.T) {
		items, total := search(t, QueryParams{SeatKeys: []string{"seat-a", "seat-b"}, SeatFilterSet: true})
		if total != 2 {
			t.Fatalf("total = %d, want 2", total)
		}
		for _, it := range items {
			if it.OwnerAccountID != "seat-a" && it.OwnerAccountID != "seat-b" {
				t.Errorf("unexpected seat %q", it.OwnerAccountID)
			}
		}
	})

	t.Run("owner id of a seat_id row does not match", func(t *testing.T) {
		// seatKeyExpr prefers seat_id, so the pool creator's id owns no seat here.
		if _, total := search(t, QueryParams{SeatKeys: []string{"pool-creator"}, SeatFilterSet: true}); total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
	})

	t.Run("raw term finds a directory-less seat by its displayed key", func(t *testing.T) {
		// THE REGRESSION: no org_seats row → facade resolves zero keys → only the
		// raw term can match. Case-insensitive, substring.
		items, total := search(t, QueryParams{SeatKeyLike: "DEMO-ALICE", SeatFilterSet: true})
		if total != 1 || items[0].OwnerAccountID != "demo-alice" {
			t.Fatalf("total=%d items=%v, want exactly demo-alice", total, items)
		}
		// A shared prefix matches every such seat — substring, not equality.
		if _, total := search(t, QueryParams{SeatKeyLike: "demo-", SeatFilterSet: true}); total != 2 {
			t.Errorf("prefix search total = %d, want 2 (demo-alice + demo-bob)", total)
		}
	})

	t.Run("resolved keys OR raw term", func(t *testing.T) {
		// An alias hit (seat-a) and a raw-key hit (demo-alice) surface together —
		// the admin searched one word and both label origins answered.
		if _, total := search(t, QueryParams{
			SeatKeys: []string{"seat-a"}, SeatKeyLike: "demo-alice", SeatFilterSet: true,
		}); total != 2 {
			t.Errorf("union total = %d, want 2", total)
		}
	})

	t.Run("LIKE metacharacters are literal", func(t *testing.T) {
		if _, total := search(t, QueryParams{SeatKeyLike: "%", SeatFilterSet: true}); total != 0 {
			t.Errorf("%% matched %d seats, want 0 (escaped, not a wildcard)", total)
		}
		if _, total := search(t, QueryParams{SeatKeyLike: "demo_alice", SeatFilterSet: true}); total != 0 {
			t.Errorf("_ matched %d seats, want 0 (escaped, not any-char)", total)
		}
	})

	t.Run("searched with nothing to match on is empty, not everyone", func(t *testing.T) {
		if _, total := search(t, QueryParams{SeatFilterSet: true}); total != 0 {
			t.Errorf("total = %d, want 0 (NOT the unfiltered list)", total)
		}
	})

	t.Run("no search stays the full list", func(t *testing.T) {
		if _, total := search(t, QueryParams{}); total != 4 {
			t.Errorf("unfiltered total = %d, want 4", total)
		}
	})
}

func TestEffectiveListLimit(t *testing.T) {
	// The HTTP envelope echoes this as the page size in effect, so the console and
	// the server must agree on what an unset/absurd ?limit= resolves to.
	for _, tc := range []struct{ in, want int }{
		{0, defaultListLimit},
		{-5, defaultListLimit},
		{20, 20},
		{maxListLimit + 1, maxListLimit},
	} {
		if got := EffectiveListLimit(tc.in); got != tc.want {
			t.Errorf("EffectiveListLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
