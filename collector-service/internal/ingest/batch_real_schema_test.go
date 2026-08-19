package ingest

import (
	"context"
	"testing"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// These are the pre-refactor fences for the batch-transaction rewrite of
// IngestBatch (P0-4, update/20260819-审计流水线容量-P0-4核证与批量投影方案.md
// S1). Every multi-event batch semantic that the per-event implementation
// guarantees today is pinned here against the REAL schema, so the batched
// implementation must reproduce it bit-for-bit:
//
//   1. mixed batches (accepted / duplicated / rejected / out-of-order seqs)
//      keep exact per-status accounting and exact rows;
//   2. an intra-batch duplicate event_id dedups to ONE stored row;
//   3. a swallowed constraint violation on ONE event surfaces as that event's
//      rejection WITHOUT blocking the rest of the batch (F2 guard, per-event
//      independence);
//   4. a watermark-advance failure never fails the batch (events stay stored,
//      the source is omitted from contiguous_seq so the client conserves).
//
// Before this file, no multi-event batch ever touched a real SQL repository
// (service_test.go uses an in-memory mock) — coverage audit 2026-08-19.

// batchEvent builds a v2 event carrying a source identity, mirroring insertSeq
// but returned as a value for batch assembly.
func batchEvent(org, src, eventID string, seq int64) UsageEvent {
	now := aikeytime.Now()
	s := seq
	return UsageEvent{
		EventID:       eventID,
		OrgID:         org,
		SourceID:      src,
		SourceSeq:     &s,
		EventTime:     now,
		OccurredAt:    now,
		RequestStatus: "success",
		RequestCount:  1,
	}
}

func odsCount(t *testing.T, db *shared.DB, org string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM usage_event_ods WHERE org_id = ?", org).Scan(&n); err != nil {
		t.Fatalf("count ods: %v", err)
	}
	return n
}

// TestIngestBatch_MultiEventMixedRealSchema: one batch mixing every status,
// with in-batch OUT-OF-ORDER seqs (3,1,2), a duplicate of a previously stored
// event, and a validation reject. The single post-batch watermark advance must
// zip to 3.
func TestIngestBatch_MultiEventMixedRealSchema(t *testing.T) {
	db := newWatermarkTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	ctx := context.Background()
	org, src := "orgB1", "srcB1"

	// Pre-store seq 1 so its re-delivery inside the batch is a duplicate.
	pre := batchEvent(org, src, "b1-e1", 1)
	if r := ingestOneEvent(t, svc, &pre); r.Status != "accepted" {
		t.Fatalf("pre-store: %s", r.Status)
	}

	bad := batchEvent(org, src, "b1-bad", 4)
	bad.RequestStatus = "" // validation reject (required field)

	req := &BatchRequest{Events: []UsageEvent{
		batchEvent(org, src, "b1-e3", 3), // out of order: 3 before 2
		pre,                              // duplicate of pre-stored seq 1
		batchEvent(org, src, "b1-e2", 2),
		bad,
	}}
	resp, results := svc.IngestBatch(ctx, req)

	if resp.Accepted != 2 || resp.Duplicated != 1 || resp.Rejected != 1 || resp.Quarantined != 0 {
		t.Fatalf("resp=%+v want accepted=2 duplicated=1 rejected=1", *resp)
	}
	wantStatus := []string{"accepted", "duplicated", "accepted", "rejected"}
	for i, r := range results {
		if r.Status != wantStatus[i] {
			t.Fatalf("result[%d]=%s want %s", i, r.Status, wantStatus[i])
		}
	}
	if got := odsCount(t, db, org); got != 3 {
		t.Fatalf("ods rows=%d want 3 (e1,e2,e3; reject not stored)", got)
	}
	// The out-of-order batch is contiguous 1..3 once landed → watermark zips to 3.
	if got := resp.ContiguousSeq[src]; got != 3 {
		t.Fatalf("contiguous=%d want 3 (out-of-order in-batch seqs must zip)", got)
	}
}

// TestIngestBatch_IntraBatchDuplicateEventID: the SAME event_id twice in ONE
// batch. Per-event semantics: first insert wins (accepted), second is a
// genuine duplicate — exactly one row stored. A multi-row INSERT rewrite
// behaves differently on intra-statement conflicts (PG raises "cannot affect
// row a second time"), so the batched implementation must keep per-row
// statements or prove equivalence here.
func TestIngestBatch_IntraBatchDuplicateEventID(t *testing.T) {
	db := newWatermarkTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	org, src := "orgB2", "srcB2"

	e := batchEvent(org, src, "b2-dup", 1)
	resp, results := svc.IngestBatch(context.Background(),
		&BatchRequest{Events: []UsageEvent{e, e}})

	if resp.Accepted != 1 || resp.Duplicated != 1 {
		t.Fatalf("resp=%+v want accepted=1 duplicated=1", *resp)
	}
	if results[0].Status != "accepted" || results[1].Status != "duplicated" {
		t.Fatalf("results=[%s,%s] want [accepted,duplicated]", results[0].Status, results[1].Status)
	}
	if got := odsCount(t, db, org); got != 1 {
		t.Fatalf("ods rows=%d want exactly 1", got)
	}
}

// TestIngestBatch_SwallowedViolationDoesNotBlockBatch: per-event independence
// under a swallowed constraint violation. Event 2 of 3 hits a RAISE(IGNORE)
// trigger (the deterministic stand-in for a swallowed NOT NULL/CHECK/FK, same
// device as f2_swallow_guard_test.go); it must come back "rejected" while
// events 1 and 3 land normally. This is the semantic the batch-transaction
// rewrite's fallback path exists to preserve: a poisoned SQL statement aborts
// a PG transaction, so the rewrite must degrade to per-event replay instead of
// failing the whole batch.
func TestIngestBatch_SwallowedViolationDoesNotBlockBatch(t *testing.T) {
	db := newWatermarkTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	ctx := context.Background()
	src := "srcB3"

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER swallow_batch_sentinel BEFORE INSERT ON usage_event_ods
		 WHEN NEW.org_id = 'orgB3-swallow'
		 BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatalf("install swallow trigger: %v", err)
	}

	poisoned := batchEvent("orgB3-swallow", src, "b3-poisoned", 1)
	resp, results := svc.IngestBatch(ctx, &BatchRequest{Events: []UsageEvent{
		batchEvent("orgB3", src, "b3-e1", 1),
		poisoned,
		batchEvent("orgB3", src, "b3-e2", 2),
	}})

	if resp.Accepted != 2 || resp.Rejected != 1 {
		t.Fatalf("resp=%+v want accepted=2 rejected=1", *resp)
	}
	if results[1].Status != "rejected" {
		t.Fatalf("poisoned event status=%s want rejected", results[1].Status)
	}
	if got := odsCount(t, db, "orgB3"); got != 2 {
		t.Fatalf("healthy org rows=%d want 2 (batch must not be blocked)", got)
	}
	if got := odsCount(t, db, "orgB3-swallow"); got != 0 {
		t.Fatalf("swallowed org rows=%d want 0", got)
	}
	// The healthy org's watermark still advanced.
	if got := resp.ContiguousSeq[src]; got != 2 {
		t.Fatalf("contiguous=%d want 2", got)
	}
}

// TestIngestBatch_AbortingViolationFallsBackToPerEvent: a HARD SQL failure
// (RAISE(ABORT) — poisons a transaction, unlike the swallowed RAISE(IGNORE)
// above) must trigger the batch-tx rollback + per-event replay: the poisoned
// event alone is rejected, the others land, nothing is double-inserted.
func TestIngestBatch_AbortingViolationFallsBackToPerEvent(t *testing.T) {
	db := newWatermarkTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	ctx := context.Background()
	src := "srcB5"

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER abort_batch_sentinel BEFORE INSERT ON usage_event_ods
		 WHEN NEW.org_id = 'orgB5-abort'
		 BEGIN SELECT RAISE(ABORT, 'forced insert failure'); END;`); err != nil {
		t.Fatalf("install abort trigger: %v", err)
	}

	poisoned := batchEvent("orgB5-abort", src, "b5-poisoned", 1)
	resp, results := svc.IngestBatch(ctx, &BatchRequest{Events: []UsageEvent{
		batchEvent("orgB5", src, "b5-e1", 1),
		poisoned,
		batchEvent("orgB5", src, "b5-e2", 2),
	}})

	if resp.Accepted != 2 || resp.Rejected != 1 {
		t.Fatalf("resp=%+v want accepted=2 rejected=1 (per-event replay)", *resp)
	}
	if results[1].Status != "rejected" {
		t.Fatalf("poisoned status=%s want rejected", results[1].Status)
	}
	if got := odsCount(t, db, "orgB5"); got != 2 {
		t.Fatalf("healthy org rows=%d want 2 (no loss, no double insert)", got)
	}
	if got := resp.ContiguousSeq[src]; got != 2 {
		t.Fatalf("contiguous=%d want 2", got)
	}
}

// TestIngestBatch_WatermarkFailureDoesNotFailBatch: AdvanceWatermark blowing up
// must not reject stored events — the source is simply omitted from
// contiguous_seq (client conserves its WAL, next batch retries). Forced
// deterministically by making the watermark upsert error via trigger.
func TestIngestBatch_WatermarkFailureDoesNotFailBatch(t *testing.T) {
	db := newWatermarkTestDB(t)
	svc := NewService(NewSQLODSRepository(db))
	ctx := context.Background()
	org, src := "orgB4", "srcB4"

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER watermark_fail_sentinel BEFORE INSERT ON usage_source_watermark
		 WHEN NEW.org_id = 'orgB4'
		 BEGIN SELECT RAISE(ABORT, 'forced watermark failure'); END;`); err != nil {
		t.Fatalf("install watermark trigger: %v", err)
	}

	resp, results := svc.IngestBatch(ctx, &BatchRequest{Events: []UsageEvent{
		batchEvent(org, src, "b4-e1", 1),
		batchEvent(org, src, "b4-e2", 2),
	}})

	if resp.Accepted != 2 {
		t.Fatalf("accepted=%d want 2 — watermark failure must not fail the batch", resp.Accepted)
	}
	for i, r := range results {
		if r.Status != "accepted" {
			t.Fatalf("result[%d]=%s want accepted", i, r.Status)
		}
	}
	if got := odsCount(t, db, org); got != 2 {
		t.Fatalf("ods rows=%d want 2 (events ARE stored)", got)
	}
	if _, ok := resp.ContiguousSeq[src]; ok {
		t.Fatalf("contiguous_seq must OMIT the failed source so the client conserves its WAL")
	}
}
