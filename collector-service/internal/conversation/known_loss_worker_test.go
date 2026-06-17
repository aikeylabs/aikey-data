package conversation

import (
	"context"
	"strconv"
	"testing"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

func ledgerSeqs(t *testing.T, db *shared.DB, org, src string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT seq FROM conversation_known_loss_ledger WHERE org_id=? AND source_id=? ORDER BY seq`, org, src)
	if err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("ledger scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// The known-loss worker must NOT promote a fresh gap (still in-flight) but MUST
// promote one aged past the timeout, ledgering the lost seq and re-advancing the
// watermark past it — otherwise contiguous_seq (and the proxy's content-WAL
// pruning) would stall on the gap forever.
func TestKnownLossWorker_PromotesOnlyStaleGap(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	svc := NewService(repo)
	ctx := context.Background()
	org, src := "org1", "srcA"

	// arrive 1,2,3,5 — seq 4 lost.
	for _, seq := range []int64{1, 2, 3, 5} {
		ingest(ctx, svc, rec("ev"+strconv.FormatInt(seq, 10), org, "s", "seatX", src, seq, ""))
	}
	if c, _ := repo.AdvanceWatermark(ctx, org, src, 5); c != 3 {
		t.Fatalf("precondition contiguous=%d want 3 (stops before gap at 4)", c)
	}

	w := NewKnownLossWorker(repo, nil) // default 30min timeout

	// Fresh gap (last_event_at just stamped = now) → NOT promotable yet.
	w.PromoteOnce(ctx)
	if got := ledgerSeqs(t, db, org, src); len(got) != 0 {
		t.Fatalf("ledger=%v want empty (gap not yet stale)", got)
	}
	if c, _ := repo.AdvanceWatermark(ctx, org, src, 5); c != 3 {
		t.Fatalf("contiguous=%d want still 3 (no promotion)", c)
	}

	// Age the watermark well past the timeout.
	if _, err := db.ExecContext(ctx,
		`UPDATE conversation_source_watermark SET last_event_at = 1000 WHERE org_id=? AND source_id=?`, org, src); err != nil {
		t.Fatalf("age watermark: %v", err)
	}
	w.PromoteOnce(ctx)

	// seq 4 ledgered, and contiguous zips to 5 (ledgered seq counts as present).
	if got := ledgerSeqs(t, db, org, src); len(got) != 1 || got[0] != 4 {
		t.Fatalf("ledger=%v want [4]", got)
	}
	if c, _ := repo.AdvanceWatermark(ctx, org, src, 5); c != 5 {
		t.Fatalf("contiguous=%d want 5 (advanced past promoted gap)", c)
	}

	// Idempotent: a second promote pass adds nothing.
	w.PromoteOnce(ctx)
	if got := ledgerSeqs(t, db, org, src); len(got) != 1 {
		t.Fatalf("ledger=%v want still [4] (idempotent)", got)
	}
}
