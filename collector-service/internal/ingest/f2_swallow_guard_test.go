package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// mkODSEvent builds a minimal UsageEvent carrying exactly the real-schema
// NOT-NULL-without-default columns (event_id, org_id, event_time, occurred_at,
// request_status; request_count defaulted). Mirrors insertSeq in watermark_test
// so both exercise the same real ODS schema (test-fixture-real-schema).
func mkODSEvent(org, eventID string) *UsageEvent {
	now := aikeytime.Now()
	return &UsageEvent{
		EventID:       eventID,
		OrgID:         org,
		EventTime:     now,
		OccurredAt:    now,
		RequestStatus: "success",
		RequestCount:  1,
	}
}

// TestInsertEvent_GenuineDuplicate_ReportedNotErrored verifies the *happy* half
// of the F2 guard: a true UNIQUE conflict (same org_id+event_id re-delivered)
// must be reported as a duplicate — (inserted=false, err=nil) — NOT surfaced as
// an error. This is the path the guard must NOT over-trigger on.
func TestInsertEvent_GenuineDuplicate_ReportedNotErrored(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()
	e := mkODSEvent("orgDup", "evt-dup-1")

	inserted, _, err := repo.InsertEvent(ctx, e, []byte("{}"), false)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v (want inserted=true, nil)", inserted, err)
	}

	inserted2, conflict2, err2 := repo.InsertEvent(ctx, e, []byte("{}"), false)
	if err2 != nil {
		t.Fatalf("genuine duplicate must NOT error (F2 guard over-triggered): %v", err2)
	}
	if inserted2 {
		t.Fatalf("duplicate insert reported inserted=true")
	}
	if conflict2 {
		t.Fatalf("same content re-delivered must not flag a content_hash conflict")
	}
}

// TestInsertEvent_SwallowedConstraintViolation_SurfacesError is the P1-2a fence
// test (architecture-review finding, 2026-06-09). It proves the F2 guard
// (repository_sql.go ~133-169) actually fires on the real schema:
//
// When INSERT OR IGNORE swallows a NON-dedup constraint violation, the row is
// not written and RowsAffected==0 — observationally identical to a duplicate,
// EXCEPT no matching row exists. InsertEvent MUST detect this (verify-SELECT →
// ErrNoRows) and return an error so the caller marks the event "rejected" with
// diagnostics, instead of silently reporting "duplicated" and dropping billing
// data behind an HTTP 200 (the exact F2 failure of 2026-04-27).
//
// We reproduce the swallow deterministically with a BEFORE INSERT trigger using
// RAISE(IGNORE): it produces the identical observable state (0 rows affected, no
// error, no row inserted) that a swallowed NOT NULL/CHECK/FK violation produces.
// SQLite cannot ALTER-ADD a NOT NULL column without a default, so a trigger is
// the cleanest way to manufacture that state on the unmodified real schema.
func TestInsertEvent_SwallowedConstraintViolation_SurfacesError(t *testing.T) {
	db := newWatermarkTestDB(t)
	repo := NewSQLODSRepository(db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER swallow_sentinel BEFORE INSERT ON usage_event_ods
		 WHEN NEW.org_id = 'force-swallow'
		 BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatalf("install swallow trigger: %v", err)
	}

	e := mkODSEvent("force-swallow", "evt-swallowed-1")
	inserted, conflict, err := repo.InsertEvent(ctx, e, []byte("{}"), false)

	if err == nil {
		t.Fatalf("swallowed constraint violation MUST surface an error (F2 guard); "+
			"got inserted=%v conflict=%v err=nil — this is the silent-200 data-loss bug", inserted, conflict)
	}
	if inserted {
		t.Fatalf("swallowed violation reported inserted=true")
	}
	if !strings.Contains(err.Error(), "silently ignored") {
		t.Errorf("expected F2 'silently ignored' diagnostic in error, got: %v", err)
	}
}
