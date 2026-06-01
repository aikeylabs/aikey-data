package integrity

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// newTestDB bootstraps an in-memory SQLite with the REAL data-component schema
// (rc.7 source columns + usage_source_watermark) via the dbmigrate registry —
// never a hand-rolled CREATE TABLE (test-fixture-real-schema principle), so the
// AgeMillis expression and the watermark/ODS columns are exactly production's.
func newTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

// seed inserts ODS rows for the given seqs and advances the watermark with the
// given client-allocated high-water — the exact ingest path, so the watermark
// columns the scanner reads are produced by real code, not test fixtures.
func seed(t *testing.T, db *shared.DB, org, src string, clientAllocated int64, seqs ...int64) {
	t.Helper()
	repo := ingest.NewSQLODSRepository(db)
	for _, seq := range seqs {
		s := seq
		now := aikeytime.Now()
		e := &ingest.UsageEvent{
			EventID: org + "/" + src + "/" + itoa(seq), OrgID: org,
			SourceID: src, SourceSeq: &s,
			EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		}
		if _, _, err := repo.InsertEvent(context.Background(), e, []byte("{}"), false); err != nil {
			t.Fatalf("insert seq=%d: %v", seq, err)
		}
	}
	if _, err := repo.AdvanceWatermark(context.Background(), org, src, clientAllocated); err != nil {
		t.Fatalf("advance: %v", err)
	}
}

// ageWatermark backdates updated_at and/or last_event_at by the given durations
// (SQLite INTEGER millis) so a test can simulate a gap that has persisted past
// the age threshold without sleeping. A zero duration leaves the column fresh.
func ageWatermark(t *testing.T, db *shared.DB, org, src string, staleUpdated, staleLastEvent time.Duration) {
	t.Helper()
	if staleUpdated > 0 {
		if _, err := db.ExecContext(context.Background(),
			"UPDATE usage_source_watermark SET updated_at = (CAST(strftime('%s','now') AS INTEGER) * 1000 - ?) WHERE org_id = ? AND source_id = ?",
			staleUpdated.Milliseconds(), org, src); err != nil {
			t.Fatalf("age updated_at: %v", err)
		}
	}
	if staleLastEvent > 0 {
		if _, err := db.ExecContext(context.Background(),
			"UPDATE usage_source_watermark SET last_event_at = (CAST(strftime('%s','now') AS INTEGER) * 1000 - ?) WHERE org_id = ? AND source_id = ?",
			staleLastEvent.Milliseconds(), org, src); err != nil {
			t.Fatalf("age last_event_at: %v", err)
		}
	}
}

func only(t *testing.T, findings []SourceCompleteness, src string) SourceCompleteness {
	t.Helper()
	for _, f := range findings {
		if f.SourceID == src {
			return f
		}
	}
	t.Fatalf("source %s not found in %d findings", src, len(findings))
	return SourceCompleteness{}
}

// TestScan_MiddleGap: a hole (seq 4 missing) below max_seen that has aged past
// MiddleGapAge is a middle gap, with an EXACT hole count from ODS.
func TestScan_MiddleGap(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcMid", 0, 1, 2, 3, 5) // gap at 4 → contiguous=3, max_seen=5
	ageWatermark(t, db, "org1", "srcMid", 11*time.Minute, 0)

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcMid")
	if f.Status != StatusMiddleGap {
		t.Fatalf("status=%s, want middle_gap", f.Status)
	}
	if f.Contiguous != 3 || f.MaxSeen != 5 {
		t.Fatalf("contiguous=%d max_seen=%d, want 3/5", f.Contiguous, f.MaxSeen)
	}
	// (3,5] span width 2, but seq 5 arrived → exactly 1 hole (seq 4).
	if f.GapCount != 1 {
		t.Fatalf("gap_count=%d, want 1 (only seq 4 missing, NOT span width 2)", f.GapCount)
	}
}

// TestScan_TailGap: client allocated up to 8 but the server only ever saw up to
// 3 — the (3,8] tail never arrived and has aged → tail gap, tail_pending=5.
func TestScan_TailGap(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcTail", 8, 1, 2, 3) // delivered 1..3, allocated 8
	ageWatermark(t, db, "org1", "srcTail", 11*time.Minute, 0)

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcTail")
	if f.Status != StatusTailGap {
		t.Fatalf("status=%s, want tail_gap", f.Status)
	}
	if f.TailPending != 5 {
		t.Fatalf("tail_pending=%d, want 5 (allocated 8 - max_seen 3)", f.TailPending)
	}
	if f.GapCount != 0 {
		t.Fatalf("gap_count=%d, want 0 (no middle hole, the gap is at the tail)", f.GapCount)
	}
}

// TestScan_Silent: a complete source whose last event is older than
// SilenceTimeout is silent (no middle/tail gap, just quiet).
func TestScan_Silent(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcQuiet", 0, 1, 2, 3) // complete 1..3
	ageWatermark(t, db, "org1", "srcQuiet", 0, 25*time.Hour)

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcQuiet")
	if f.Status != StatusSilent {
		t.Fatalf("status=%s, want silent", f.Status)
	}
}

// TestScan_OutOfOrderNotFalsePositive is the critical anti-false-positive case:
// the SAME hole as the middle-gap test, but FRESH (updated within MiddleGapAge).
// A seq arriving out of order within the grace window must NOT be flagged — it's
// in-flight reordering, not loss. This is what keeps the alarm trustworthy.
func TestScan_OutOfOrderNotFalsePositive(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcReorder", 0, 1, 2, 3, 5) // gap at 4, but JUST happened
	// no ageWatermark → updated_at is fresh (well within 10m)

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcReorder")
	if f.Status != StatusOK {
		t.Fatalf("status=%s, want ok (fresh gap = in-flight reorder, must not false-positive)", f.Status)
	}
	if f.GapCount != 0 {
		t.Fatalf("gap_count=%d, want 0 (not yet flagged, no ODS count run)", f.GapCount)
	}
}

// TestScan_HealthyComplete: a fully contiguous, recently-updated source is ok.
func TestScan_HealthyComplete(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcOK", 3, 1, 2, 3) // complete, allocated==delivered

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcOK")
	if f.Status != StatusOK {
		t.Fatalf("status=%s, want ok", f.Status)
	}
	if !f.Healthy() {
		t.Fatalf("Healthy()=false for a complete source")
	}
}

// TestScan_MiddleTakesPrecedenceOverTail: a source with BOTH a middle hole and
// an unsent tail reports middle_gap (the more actionable, confirmed loss).
func TestScan_MiddleTakesPrecedenceOverTail(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "org1", "srcBoth", 9, 1, 2, 3, 5) // hole at 4 (mid) + allocated 9 (tail)
	ageWatermark(t, db, "org1", "srcBoth", 11*time.Minute, 0)

	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := only(t, findings, "srcBoth")
	if f.Status != StatusMiddleGap {
		t.Fatalf("status=%s, want middle_gap (precedence over tail)", f.Status)
	}
	if f.GapCount != 1 || f.TailPending != 4 {
		t.Fatalf("gap_count=%d tail_pending=%d, want 1/4 (hole=4, tail=9-5)", f.GapCount, f.TailPending)
	}
}

// TestScan_QuarantineCount: a source with a quarantined (content_hash-failed)
// event surfaces quarantine_count>0 even though its delivery is gap-free (the
// seq still arrived). Quarantine is orthogonal to the gap Status.
func TestScan_QuarantineCount(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	// seq 1 accepted, seq 2 quarantined — both stored, both advance contiguity.
	s1, s2 := int64(1), int64(2)
	now := aikeytime.Now()
	mk := func(id string, seq *int64) *ingest.UsageEvent {
		return &ingest.UsageEvent{
			EventID: id, OrgID: "orgQ", SourceID: "srcQ", SourceSeq: seq,
			EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		}
	}
	if _, _, err := repo.InsertEvent(ctx, mk("q1", &s1), []byte("{}"), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.InsertEvent(ctx, mk("q2", &s2), []byte("{}"), true); err != nil { // quarantined
		t.Fatal(err)
	}
	if _, err := repo.AdvanceWatermark(ctx, "orgQ", "srcQ", 0); err != nil {
		t.Fatal(err)
	}

	f := only(t, mustScan(t, db), "srcQ")
	if f.QuarantineCount != 1 {
		t.Fatalf("quarantine_count=%d, want 1", f.QuarantineCount)
	}
	// Delivery is gap-free (both seqs present) → Status stays ok; quarantine is
	// surfaced separately, NOT as a gap.
	if f.Status != StatusOK {
		t.Fatalf("status=%s, want ok (no delivery gap; quarantine is orthogonal)", f.Status)
	}
}

func mustScan(t *testing.T, db *shared.DB) []SourceCompleteness {
	t.Helper()
	findings, err := NewScanner(db, DefaultCriteria()).Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return findings
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestPromoteFlow_MiddleGap is the end-to-end D1 loop: a stale middle gap is
// enumerated, recorded in the known-loss ledger, and contiguous advances PAST it
// so the source converges to "complete with documented losses" (§6.3). Mirrors
// the projector's promoteKnownLoss orchestration.
func TestPromoteFlow_MiddleGap(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	seed(t, db, "orgP", "srcP", 0, 1, 2, 3, 5) // gap at 4 → contiguous=3, max_seen=5
	ageWatermark(t, db, "orgP", "srcP", 25*time.Hour, 0)

	f := only(t, mustScan(t, db), "srcP")
	if !f.Promotable {
		t.Fatalf("Promotable=false, want true (gap aged 25h > KnownLossTimeout 24h)")
	}
	if f.Status != StatusMiddleGap {
		t.Fatalf("status=%s, want middle_gap", f.Status)
	}

	hi := f.MaxSeen
	if f.ClientAllocated > hi {
		hi = f.ClientAllocated
	}
	missing, err := repo.EnumerateMissingSeqs(ctx, "orgP", "srcP", f.Contiguous, hi)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != 4 {
		t.Fatalf("missing=%v, want [4]", missing)
	}
	contiguous, err := repo.RecordKnownLoss(ctx, "orgP", "srcP", missing, "stale_middle_gap")
	if err != nil {
		t.Fatal(err)
	}
	if contiguous != 5 {
		t.Fatalf("contiguous=%d after promote, want 5 (zipped PAST ledgered seq 4)", contiguous)
	}

	// Re-scan: converged — ok, known_loss=1, gap_count=0.
	f2 := only(t, mustScan(t, db), "srcP")
	if f2.Status != StatusOK {
		t.Fatalf("status=%s after promote, want ok (gap now accounted)", f2.Status)
	}
	if f2.KnownLoss != 1 {
		t.Fatalf("known_loss_count=%d, want 1", f2.KnownLoss)
	}
	if f2.GapCount != 0 {
		t.Fatalf("gap_count=%d, want 0 (seq 4 ledgered, not a gap)", f2.GapCount)
	}
}

// TestPromoteFlow_TailGap: a stale tail (allocated 8, only 1..3 delivered)
// promotes the never-seen tail seqs and converges contiguous to client_allocated.
func TestPromoteFlow_TailGap(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	seed(t, db, "orgT2", "srcT2", 8, 1, 2, 3) // delivered 1..3, allocated 8
	ageWatermark(t, db, "orgT2", "srcT2", 25*time.Hour, 0)

	f := only(t, mustScan(t, db), "srcT2")
	if !f.Promotable || f.Status != StatusTailGap {
		t.Fatalf("Promotable=%v status=%s, want true/tail_gap", f.Promotable, f.Status)
	}
	hi := f.MaxSeen
	if f.ClientAllocated > hi {
		hi = f.ClientAllocated
	}
	missing, _ := repo.EnumerateMissingSeqs(ctx, "orgT2", "srcT2", f.Contiguous, hi)
	if len(missing) != 5 { // (3,8] = 4,5,6,7,8
		t.Fatalf("missing=%v, want 5 seqs (4..8)", missing)
	}
	contiguous, _ := repo.RecordKnownLoss(ctx, "orgT2", "srcT2", missing, "stale_tail_gap")
	if contiguous != 8 {
		t.Fatalf("contiguous=%d, want 8 (converged to client_allocated)", contiguous)
	}
	f2 := only(t, mustScan(t, db), "srcT2")
	if f2.Status != StatusOK || f2.KnownLoss != 5 || f2.TailPending != 0 {
		t.Fatalf("after promote: status=%s known_loss=%d tail_pending=%d, want ok/5/0", f2.Status, f2.KnownLoss, f2.TailPending)
	}
}

// TestRecordKnownLoss_Idempotent: re-promoting the same seqs doesn't duplicate
// ledger rows or regress contiguous (the projector may re-tick before a source
// gets new events).
func TestRecordKnownLoss_Idempotent(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	seed(t, db, "orgI", "srcI", 0, 1, 2, 3, 5) // gap at 4
	if _, err := repo.RecordKnownLoss(ctx, "orgI", "srcI", []int64{4}, "stale_middle_gap"); err != nil {
		t.Fatal(err)
	}
	c2, err := repo.RecordKnownLoss(ctx, "orgI", "srcI", []int64{4}, "stale_middle_gap") // again
	if err != nil {
		t.Fatal(err)
	}
	if c2 != 5 {
		t.Fatalf("contiguous=%d on re-record, want 5 (stable)", c2)
	}
	if f := only(t, mustScan(t, db), "srcI"); f.KnownLoss != 1 {
		t.Fatalf("known_loss_count=%d, want 1 (idempotent — no duplicate ledger row)", f.KnownLoss)
	}
}

// TestEnumerateMissingSeqs_ExcludesLedgered: once a seq is ledgered it's no
// longer "missing" (accounted), so a second promotion pass is a no-op.
func TestEnumerateMissingSeqs_ExcludesLedgered(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()

	seed(t, db, "orgE", "srcE", 0, 1, 2, 3, 5, 7) // gaps at 4 and 6 → contiguous=3, max_seen=7
	// Record only seq 4 as lost.
	if _, err := repo.RecordKnownLoss(ctx, "orgE", "srcE", []int64{4}, "x"); err != nil {
		t.Fatal(err)
	}
	// contiguous zips 3→5 (4 ledgered, 5 in ODS), stops at 6 (still missing).
	missing, err := repo.EnumerateMissingSeqs(ctx, "orgE", "srcE", 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != 6 {
		t.Fatalf("missing=%v, want [6] (4 excluded — ledgered; 5,7 in ODS)", missing)
	}
}

// TestGapSeqs_EnumeratesAndExcludesLedgered: GapSeqs returns the unaccounted
// seqs; once ledgered they drop out (D3 client checks these against its WAL).
func TestGapSeqs_EnumeratesAndExcludesLedgered(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()
	seed(t, db, "orgG", "srcG", 0, 1, 2, 3, 5, 7) // gaps at 4 and 6
	seqs, truncated, err := repo.GapSeqs(ctx, "orgG", "srcG", 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(seqs) != 2 || seqs[0] != 4 || seqs[1] != 6 {
		t.Fatalf("GapSeqs=%v truncated=%v, want [4 6] false", seqs, truncated)
	}
	// Ledger seq 4 → GapSeqs now returns only [6].
	if _, err := repo.RecordKnownLoss(ctx, "orgG", "srcG", []int64{4}, "x"); err != nil {
		t.Fatal(err)
	}
	seqs, _, _ = repo.GapSeqs(ctx, "orgG", "srcG", 0)
	if len(seqs) != 1 || seqs[0] != 6 {
		t.Fatalf("GapSeqs after ledger=%v, want [6]", seqs)
	}
}

// TestConfirmLost_GuardsReceivedSeqs is the safety invariant: a client must not
// be able to mark a RECEIVED seq as lost. ConfirmLost only ledgers genuinely
// absent seqs; a received seq passed in is ignored.
func TestConfirmLost_GuardsReceivedSeqs(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()
	seed(t, db, "orgC", "srcC", 0, 1, 2, 3, 5) // gap at 4; 5 is RECEIVED

	// Client (wrongly or racily) declares both 4 AND 5 lost. Only 4 is absent.
	promoted, contiguous, err := repo.ConfirmLost(ctx, "orgC", "srcC", []int64{4, 5}, "client_confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 1 {
		t.Fatalf("promoted=%d, want 1 (only the genuinely-absent seq 4; seq 5 is received)", promoted)
	}
	if contiguous != 5 {
		t.Fatalf("contiguous=%d, want 5 (4 ledgered, 5 received → zip to 5)", contiguous)
	}
	// Assert seq 5 is NOT in the ledger (would corrupt the audit / double-count).
	var n int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_known_loss_ledger WHERE org_id='orgC' AND source_id='srcC' AND seq=5").Scan(&n)
	if n != 0 {
		t.Fatalf("seq 5 (received) was marked lost — confirm-lost guard failed")
	}
	f := only(t, mustScan(t, db), "srcC")
	if f.KnownLoss != 1 || f.Status != StatusOK {
		t.Fatalf("known_loss=%d status=%s, want 1/ok", f.KnownLoss, f.Status)
	}
}

// TestConfirmLost_RejectsAboveAllocated: a client (buggy or malicious) cannot
// inflate known_loss by confirming seqs that were never allocated/seen. Only
// seqs within (0, max(max_seen, client_allocated)] are ledgerable.
func TestConfirmLost_RejectsAboveAllocated(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()
	seed(t, db, "orgA", "srcA", 0, 1, 2, 3, 5) // gap at 4; max_seen=5, client_allocated=0 → ceiling=5

	// Confirm 4 (absent, in range) + 99 (above ceiling, never allocated).
	promoted, _, err := repo.ConfirmLost(ctx, "orgA", "srcA", []int64{4, 99}, "client_confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 1 {
		t.Fatalf("promoted=%d, want 1 (only seq 4; seq 99 is above the allocated ceiling 5)", promoted)
	}
	var n int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_known_loss_ledger WHERE org_id='orgA' AND source_id='srcA' AND seq=99").Scan(&n)
	if n != 0 {
		t.Fatalf("seq 99 (above ceiling) was ledgered — above-allocated guard failed")
	}
}

// TestKnownLoss_ResurrectionNotDoubleCounted: if a seq is confirmed-lost and
// THEN its real event arrives (resurrection / direct-abuse double-write), ODS
// presence is authoritative — known_loss must not count it (it's received), and
// gap_count must not be corrupted by the dual presence.
func TestKnownLoss_ResurrectionNotDoubleCounted(t *testing.T) {
	db := newTestDB(t)
	repo := ingest.NewSQLODSRepository(db)
	ctx := context.Background()
	seed(t, db, "orgR", "srcR", 0, 1, 2, 3, 5) // gap at 4, max_seen=5

	// Confirm seq 4 lost → known_loss=1.
	if _, _, err := repo.ConfirmLost(ctx, "orgR", "srcR", []int64{4}, "client_confirmed"); err != nil {
		t.Fatal(err)
	}
	if f := only(t, mustScan(t, db), "srcR"); f.KnownLoss != 1 {
		t.Fatalf("known_loss=%d, want 1 before resurrection", f.KnownLoss)
	}

	// Now seq 4's real event arrives after all (resurrection).
	s4 := int64(4)
	now := aikeytime.Now()
	e := &ingest.UsageEvent{
		EventID: "orgR/srcR/4-late", OrgID: "orgR", SourceID: "srcR", SourceSeq: &s4,
		EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
	}
	if _, _, err := repo.InsertEvent(ctx, e, []byte("{}"), false); err != nil {
		t.Fatal(err)
	}

	// known_loss must drop to 0 (seq 4 is now in ODS — received, not lost).
	f := only(t, mustScan(t, db), "srcR")
	if f.KnownLoss != 0 {
		t.Fatalf("known_loss=%d after resurrection, want 0 (ODS presence is authoritative)", f.KnownLoss)
	}
}
