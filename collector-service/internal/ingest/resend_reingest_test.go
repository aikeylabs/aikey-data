package ingest

import (
	"context"
	"database/sql"
	"testing"
)

// odsRowSnapshot is the metering + identity surface that defines a usage event.
// Comparing it before-delete vs after-reingest is the deterministic stand-in for
// the live E2E's "byte-identical WAL replay" assertion: if the collector stored
// the re-sent event with any field altered, this differs.
type odsRowSnapshot struct {
	eventID                        string
	contentHash                    sql.NullString
	model, provider                string
	reqStatus, ingest, dwd         string
	total, in, out, cacheR, cacheC sql.NullInt64
}

func snapshotODS(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, eventID string) odsRowSnapshot {
	t.Helper()
	var s odsRowSnapshot
	s.eventID = eventID
	err := db.QueryRowContext(context.Background(),
		`SELECT content_hash, model, provider_code, request_status, ingest_status, dwd_status,
		        total_tokens, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens
		   FROM usage_event_ods WHERE event_id = ?`, eventID).
		Scan(&s.contentHash, &s.model, &s.provider, &s.reqStatus, &s.ingest, &s.dwd,
			&s.total, &s.in, &s.out, &s.cacheR, &s.cacheC)
	if err != nil {
		t.Fatalf("snapshot ODS row %s: %v", eventID, err)
	}
	return s
}

// TestResend_ByteIdenticalReingestAfterDelete is the collector-side, deterministic
// proof of the D3 re-send round-trip — the mirror of the live E2E
// (workflow/CI/e2e/cases/2026-06-01-reconcile-resend-recovers-wal-present-gap.md)
// without real LLM traffic. It exercises the REAL rc.7 schema:
//
//  1. ingest seqs 1,2,3 → contiguous advances to 3.
//  2. capture seq 2's full row, then DELETE it (collector now missing 2) and roll
//     the watermark back to 1 — the consistent state "seq 2 was allocated but
//     never delivered" (contiguous can't be 3 if 2 is gone).
//  3. /gaps must report exactly [2].
//  4. RE-INGEST the SAME seq-2 event (what reconcile's resendWALSeqs replays from
//     the WAL) → it is ACCEPTED (a fresh insert, not "duplicated"), stored
//     BYTE-IDENTICAL to the capture, and the watermark zips back to 3.
//  5. /gaps empty again and the known-loss ledger stays EMPTY — recovered via
//     re-send, NOT confirm-lost.
func TestResend_ByteIdenticalReingestAfterDelete(t *testing.T) {
	db := newContentHashTestDB(t)
	repo := NewSQLODSRepository(db)
	svc := NewService(repo)
	ctx := context.Background()
	const org, src = "orgH", "srcH" // hashEvent hardcodes these

	// 1. ingest 1,2,3 (distinct realistic metering so the hash is non-trivial).
	events := map[int64]*UsageEvent{}
	for _, seq := range []int64{1, 2, 3} {
		e, h := hashEvent("rs-"+itoa64(seq), seq, 100+seq, 50+seq, 10, 20, "claude-haiku-4-5", "anthropic")
		e.ContentHash = h
		if r := ingestOneEvent(t, svc, e); r.Status != "accepted" {
			t.Fatalf("seq %d ingest status=%s, want accepted", seq, r.Status)
		}
		events[seq] = e
	}
	if c, err := repo.AdvanceWatermark(ctx, org, src, 3); err != nil || c != 3 {
		t.Fatalf("advance after 1..3: contiguous=%d err=%v, want 3", c, err)
	}

	// 2. capture seq 2, delete it, roll contiguous back to 1 (the "2 never
	//    delivered" consistent state — leaves max_seen/client_allocated at 3).
	before := snapshotODS(t, db, "rs-2")
	if _, err := db.ExecContext(ctx, "DELETE FROM usage_event_ods WHERE event_id = ?", "rs-2"); err != nil {
		t.Fatalf("delete seq 2: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE usage_source_watermark SET contiguous_seq = 1 WHERE org_id = ? AND source_id = ?", org, src); err != nil {
		t.Fatalf("roll watermark: %v", err)
	}

	// 3. /gaps must surface exactly [2].
	gaps, _, err := repo.GapSeqs(ctx, org, src, 50)
	if err != nil {
		t.Fatalf("GapSeqs: %v", err)
	}
	if len(gaps) != 1 || gaps[0] != 2 {
		t.Fatalf("gaps=%v, want [2] (the WAL-present, collector-absent seq)", gaps)
	}

	// 4. re-send: re-ingest the SAME event. Must be a fresh accept (row was
	//    deleted), not "duplicated", and stored byte-identical.
	if r := ingestOneEvent(t, svc, events[2]); r.Status != "accepted" {
		t.Fatalf("re-ingest status=%s, want accepted (row was deleted → fresh insert)", r.Status)
	}
	after := snapshotODS(t, db, "rs-2")
	if before != after {
		t.Fatalf("re-ingested row not byte-identical:\n before=%+v\n after =%+v", before, after)
	}
	if after.ingest != "accepted" || after.dwd != "pending" {
		t.Fatalf("re-ingested disposition ingest=%s dwd=%s, want accepted/pending (billable again)", after.ingest, after.dwd)
	}

	// 5. watermark zips back to 3; no gaps; ledger empty (recovered, not lost).
	if c, err := repo.AdvanceWatermark(ctx, org, src, 3); err != nil || c != 3 {
		t.Fatalf("advance after re-send: contiguous=%d err=%v, want 3 (re-converged)", c, err)
	}
	gaps, _, err = repo.GapSeqs(ctx, org, src, 50)
	if err != nil || len(gaps) != 0 {
		t.Fatalf("gaps after re-send=%v err=%v, want [] (recovered)", gaps, err)
	}
	var ledger int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM usage_known_loss_ledger WHERE org_id = ? AND source_id = ?", org, src).
		Scan(&ledger); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger != 0 {
		t.Fatalf("known_loss ledger=%d, want 0 (re-sent ⇒ recovered, NOT confirm-lost)", ledger)
	}
}

// TestResend_ReingestIdempotentWhenRowPresent guards the dedup side: if a re-send
// arrives while the original row is still present (e.g. reconcile races a slow
// upload, or a double re-send), the second insert is a no-op DUPLICATE — never a
// second billable row. This is the (org_id, event_id) dedup invariant that makes
// re-send safe to retry.
func TestResend_ReingestIdempotentWhenRowPresent(t *testing.T) {
	db := newContentHashTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	ctx := context.Background()

	e, h := hashEvent("rs-dup", 1, 100, 50, 10, 20, "claude-haiku-4-5", "anthropic")
	e.ContentHash = h
	if r := ingestOneEvent(t, svc, e); r.Status != "accepted" {
		t.Fatalf("first ingest status=%s, want accepted", r.Status)
	}
	if r := ingestOneEvent(t, svc, e); r.Status != "duplicated" {
		t.Fatalf("re-send while present status=%s, want duplicated (idempotent, no double-bill)", r.Status)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM usage_event_ods WHERE event_id = ?", "rs-dup").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count=%d, want 1 (re-send must not create a second row)", n)
	}
}
