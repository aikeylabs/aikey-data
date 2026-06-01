package ingest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// newWatermarkTestDB bootstraps an in-memory SQLite with the REAL data-
// component schema (including rc.7 source_id/source_seq + usage_source_
// watermark) via the dbmigrate registry — never a hand-rolled CREATE TABLE
// (test-fixture-real-schema principle). This is what proves the rc.7
// migration and AdvanceWatermark's SQL agree on a real schema.
func newWatermarkTestDB(t *testing.T) *shared.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), db,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(db, shared.DialectSQLite)
}

// insertSeq inserts a minimal ODS row carrying just enough to exercise the
// watermark walk: org_id, source_id, source_seq + the NOT NULL columns.
func insertSeq(t *testing.T, repo ODSRepository, org, src string, seq int64) {
	t.Helper()
	now := aikeytime.Now()
	s := seq
	e := &UsageEvent{
		EventID:       org + "/" + src + "/" + itoa64(seq),
		OrgID:         org,
		SourceID:      src,
		SourceSeq:     &s,
		EventTime:     now,
		OccurredAt:    now,
		RequestStatus: "success",
		RequestCount:  1,
	}
	inserted, _, err := repo.InsertEvent(context.Background(), e, []byte("{}"), false)
	if err != nil {
		t.Fatalf("insert seq=%d: %v", seq, err)
	}
	if !inserted {
		t.Fatalf("insert seq=%d unexpectedly reported duplicate", seq)
	}
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestAdvanceWatermark_StopsAtGap is the core delivery-integrity invariant:
// the contiguous high-water mark walks consecutive seqs and STOPS at the first
// gap — it must not jump past a missing seq. This is what makes "contiguous"
// safe for the client to prune its WAL up to.
func TestAdvanceWatermark_StopsAtGap(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	// Store 1,2,3, (gap at 4), 5,6 — contiguous must stop at 3.
	for _, seq := range []int64{1, 2, 3, 5, 6} {
		insertSeq(t, repo, "org1", "srcA", seq)
	}
	got, err := repo.AdvanceWatermark(ctx, "org1", "srcA", 0)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got != 3 {
		t.Fatalf("contiguous = %d, want 3 (must stop at the gap before 4)", got)
	}

	// max_seen_seq must track the HIGHEST stored seq (6), above contiguous (3) —
	// the (3,6] span is the suspected-gap region B' detection inspects. This is
	// the column B' relies on; assert AdvanceWatermark maintains it.
	var contig, maxSeen int64
	var lastEventAt sql.NullInt64
	if err := db.QueryRowContext(ctx,
		"SELECT contiguous_seq, max_seen_seq, last_event_at FROM usage_source_watermark WHERE org_id=? AND source_id=?",
		"org1", "srcA",
	).Scan(&contig, &maxSeen, &lastEventAt); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if contig != 3 || maxSeen != 6 {
		t.Fatalf("watermark cols: contiguous=%d max_seen=%d, want 3/6 (max_seen above gap)", contig, maxSeen)
	}
	if !lastEventAt.Valid || lastEventAt.Int64 == 0 {
		t.Fatalf("last_event_at not set (source-silence signal must be maintained)")
	}

	// Backfill the gap (4 arrives late) → contiguous now zips up to 6.
	insertSeq(t, repo, "org1", "srcA", 4)
	got, err = repo.AdvanceWatermark(ctx, "org1", "srcA", 0)
	if err != nil {
		t.Fatalf("advance after backfill: %v", err)
	}
	if got != 6 {
		t.Fatalf("contiguous after backfill = %d, want 6 (gap filled, zip to end)", got)
	}
}

// TestAdvanceWatermark_PerSourceIsolation proves two sources on the same org
// keep independent sequences — a gap in one must not affect the other's
// contiguous mark (the multi-device / multi-vault invariant).
func TestAdvanceWatermark_PerSourceIsolation(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	for _, seq := range []int64{1, 2, 3} { // srcA: complete 1..3
		insertSeq(t, repo, "org1", "srcA", seq)
	}
	for _, seq := range []int64{1, 3} { // srcB: gap at 2
		insertSeq(t, repo, "org1", "srcB", seq)
	}

	a, _ := repo.AdvanceWatermark(ctx, "org1", "srcA", 0)
	b, _ := repo.AdvanceWatermark(ctx, "org1", "srcB", 0)
	if a != 3 {
		t.Errorf("srcA contiguous = %d, want 3", a)
	}
	if b != 1 {
		t.Errorf("srcB contiguous = %d, want 1 (gap at 2 isolated from srcA)", b)
	}
}

// TestAdvanceWatermark_Persists confirms the watermark is written to
// usage_source_watermark (so phase B' gap detection + the client-returned
// contiguous both read a real persisted value, not a recompute-only result).
func TestAdvanceWatermark_Persists(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	for _, seq := range []int64{1, 2} {
		insertSeq(t, repo, "org9", "srcZ", seq)
	}
	if _, err := repo.AdvanceWatermark(ctx, "org9", "srcZ", 0); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var stored int64
	err := db.QueryRowContext(ctx,
		"SELECT contiguous_seq FROM usage_source_watermark WHERE org_id = ? AND source_id = ?",
		"org9", "srcZ",
	).Scan(&stored)
	if err != nil {
		t.Fatalf("read persisted watermark: %v", err)
	}
	if stored != 2 {
		t.Fatalf("persisted contiguous_seq = %d, want 2", stored)
	}

	// Idempotent re-advance with no new rows keeps the value (not regress to 0).
	got, _ := repo.AdvanceWatermark(ctx, "org9", "srcZ", 0)
	if got != 2 {
		t.Fatalf("re-advance = %d, want 2 (stable, no regression)", got)
	}
}

// TestAdvanceWatermark_ClientAllocatedMonotonic covers the B' tail-gap input:
// client_allocated_seq is the client's allocator high-water carried on the
// batch. It can legitimately sit ABOVE max_seen (events allocated but not yet
// delivered / lost before WAL) — that (contiguous, client_allocated] span is
// the tail-gap region. It must also be monotonic: a later batch carrying a
// stale (lower) allocated value must NOT lower it, else a reordered upload
// would erase a real tail-gap signal.
func TestAdvanceWatermark_ClientAllocatedMonotonic(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	// Delivered 1..3, but client says it allocated up to 9 (4..9 in-flight/lost).
	for _, seq := range []int64{1, 2, 3} {
		insertSeq(t, repo, "orgT", "srcT", seq)
	}
	if _, err := repo.AdvanceWatermark(ctx, "orgT", "srcT", 9); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var contig, maxSeen, clientAlloc int64
	read := func() {
		if err := db.QueryRowContext(ctx,
			"SELECT contiguous_seq, max_seen_seq, client_allocated_seq FROM usage_source_watermark WHERE org_id=? AND source_id=?",
			"orgT", "srcT",
		).Scan(&contig, &maxSeen, &clientAlloc); err != nil {
			t.Fatalf("read watermark: %v", err)
		}
	}
	read()
	if contig != 3 || maxSeen != 3 || clientAlloc != 9 {
		t.Fatalf("cols: contiguous=%d max_seen=%d client_allocated=%d, want 3/3/9 (tail-gap region (3,9])", contig, maxSeen, clientAlloc)
	}

	// A reordered batch arrives carrying a STALE allocated=5 → must not regress 9.
	if _, err := repo.AdvanceWatermark(ctx, "orgT", "srcT", 5); err != nil {
		t.Fatalf("advance stale: %v", err)
	}
	read()
	if clientAlloc != 9 {
		t.Fatalf("client_allocated = %d after stale batch, want 9 (monotonic, must not regress)", clientAlloc)
	}

	// allocated=0 ("no info", e.g. a v1 batch) is a no-op, also must not regress.
	if _, err := repo.AdvanceWatermark(ctx, "orgT", "srcT", 0); err != nil {
		t.Fatalf("advance zero: %v", err)
	}
	read()
	if clientAlloc != 9 {
		t.Fatalf("client_allocated = %d after allocated=0, want 9 (0 is no-op)", clientAlloc)
	}
}
