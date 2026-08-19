package conversation

import (
	"context"
	"testing"
)

// Pre-refactor fences for the batch-transaction rewrite of conversation
// IngestBatch (P0-4 S1). Coverage audit 2026-08-19: every existing real-schema
// call site passed exactly ONE record per batch — the multi-record path (the
// one the rewrite changes) had no fence. Pins, on the real schema:
//
//   1. a mixed multi-record batch keeps exact per-status accounting, exact
//      rows, and the single post-batch watermark advance zips out-of-order
//      in-batch seqs;
//   2. an intra-batch seq conflict (same (src,seq), different event_id, both
//      in ONE batch) quarantines the later record — the SeqOwner check must
//      see the earlier record of the same batch (in a batched transaction
//      this requires read-your-own-writes inside the tx);
//   3. an intra-batch duplicate event_id dedups to one row.

func TestConversationIngestBatch_MultiRecordMixedRealSchema(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()
	org, src, sess := "orgCB1", "srcCB1", "sessCB1"

	// Pre-store seq 1 so its verbatim re-delivery inside the batch is a duplicate.
	pre := rec("cb1-e1", org, sess, "seatCB", src, 1, "SYS-A")
	if r := ingest(ctx, svc, pre); r.Accepted != 1 {
		t.Fatalf("pre-store: %+v", *r)
	}

	bad := rec("cb1-bad", org, sess, "seatCB", src, 5, "")
	bad.RequestStatus = "" // validation reject

	resp := ingest(ctx, svc,
		rec("cb1-e3", org, sess, "seatCB", src, 3, ""), // out of order: 3 before 2
		pre, // verbatim re-delivery → duplicated
		rec("cb1-e2", org, sess, "seatCB", src, 2, ""),
		bad,
	)

	if resp.Accepted != 2 || resp.Duplicated != 1 || resp.Rejected != 1 || resp.Quarantined != 0 {
		t.Fatalf("resp=%+v want accepted=2 duplicated=1 rejected=1", *resp)
	}
	var rows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_records WHERE org_id=?", org).Scan(&rows)
	if rows != 3 {
		t.Fatalf("rows=%d want 3", rows)
	}
	if got := resp.ContiguousSeq[src]; got != 3 {
		t.Fatalf("contiguous=%d want 3 (in-batch out-of-order seqs must zip)", got)
	}
}

// TestConversationIngestBatch_IntraBatchSeqConflict: the conflicting record
// arrives in the SAME batch as the seq's owner. Today's per-record loop sees
// the owner because it was committed one statement earlier; a batched
// transaction must preserve this via read-your-own-writes (SeqOwner SELECT
// inside the same tx), not lose it.
func TestConversationIngestBatch_IntraBatchSeqConflict(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()
	org, src, sess := "orgCB2", "srcCB2", "sessCB2"

	resp := ingest(ctx, svc,
		rec("cb2-owner", org, sess, "seatCB", src, 1, ""),
		rec("cb2-intruder", org, sess, "seatCB", src, 1, ""), // same seq, different event_id
	)

	if resp.Accepted != 1 || resp.Quarantined != 1 {
		t.Fatalf("resp=%+v want accepted=1 quarantined=1", *resp)
	}
	var status string
	if err := db.QueryRowContext(ctx,
		"SELECT ingest_status FROM conversation_records WHERE event_id='cb2-intruder'").Scan(&status); err != nil {
		t.Fatalf("read intruder: %v", err)
	}
	if status != "quarantined" {
		t.Fatalf("intruder ingest_status=%s want quarantined", status)
	}
}

// TestConversationIngestBatch_AbortFallsBackToPerRecord: a hard SQL failure
// inside the batch tx (RAISE(ABORT) trigger) rolls back and replays
// per-record — poisoned record rejected, the rest land exactly once.
func TestConversationIngestBatch_AbortFallsBackToPerRecord(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()
	src := "srcCB4"

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER conv_abort_sentinel BEFORE INSERT ON conversation_records
		 WHEN NEW.org_id = 'orgCB4-abort'
		 BEGIN SELECT RAISE(ABORT, 'forced insert failure'); END;`); err != nil {
		t.Fatalf("install abort trigger: %v", err)
	}

	poisoned := rec("cb4-poisoned", "orgCB4-abort", "sessCB4", "seatCB", src, 1, "")
	resp := ingest(ctx, svc,
		rec("cb4-e1", "orgCB4", "sessCB4", "seatCB", src, 1, ""),
		poisoned,
		rec("cb4-e2", "orgCB4", "sessCB4", "seatCB", src, 2, ""),
	)

	if resp.Accepted != 2 || resp.Rejected != 1 {
		t.Fatalf("resp=%+v want accepted=2 rejected=1 (per-record replay)", *resp)
	}
	var rows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_records WHERE org_id='orgCB4'").Scan(&rows)
	if rows != 2 {
		t.Fatalf("healthy org rows=%d want 2 (no loss, no double insert)", rows)
	}
	if got := resp.ContiguousSeq[src]; got != 2 {
		t.Fatalf("contiguous=%d want 2", got)
	}
}

func TestConversationIngestBatch_IntraBatchDuplicateEventID(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()
	org, src := "orgCB3", "srcCB3"

	r := rec("cb3-dup", org, "sessCB3", "seatCB", src, 1, "")
	resp := ingest(ctx, svc, r, r)

	if resp.Accepted != 1 || resp.Duplicated != 1 {
		t.Fatalf("resp=%+v want accepted=1 duplicated=1", *resp)
	}
	var rows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_records WHERE org_id=?", org).Scan(&rows)
	if rows != 1 {
		t.Fatalf("rows=%d want exactly 1", rows)
	}
}
