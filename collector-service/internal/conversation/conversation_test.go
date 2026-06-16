package conversation

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// newConvTestDB bootstraps an in-memory SQLite with the REAL data-component
// schema (incl. the v1.0.1-alpha.2 conversation_* tables) via the dbmigrate
// registry — never a hand-rolled CREATE TABLE (test-fixture-real-schema
// principle). This proves the migration and the repo SQL agree on a real schema.
func newConvTestDB(t *testing.T) *shared.DB {
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

func rec(eventID, org, session, owner, source string, seq int64, systemText string) ConversationRecord {
	s := seq
	return ConversationRecord{
		EventID:        eventID,
		OrgID:          org,
		SessionID:      session,
		OwnerAccountID: owner,
		SourceID:       source,
		SourceSeq:      &s,
		UserText:       "Q-" + eventID,
		AssistantText:  "A-" + eventID,
		SystemText:     systemText,
		RequestStatus:  "ok",
		CreatedAt:      aikeytime.Now(),
	}
}

func ingest(ctx context.Context, svc *Service, recs ...ConversationRecord) *ConversationBatchResponse {
	resp, _ := svc.IngestBatch(ctx, &ConversationBatchRequest{Records: recs})
	return resp
}

// Idempotency + seq-conflict quarantine + system_text first-wins, all through
// the real Service.IngestBatch path (not a simplified inline copy).
func TestIngest_Idempotent_SeqConflict_SessionFirstWins(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db))
	ctx := context.Background()
	org, src, sess := "org1", "srcA", "sess1"

	// fresh insert → accepted, watermark contiguous=1
	if r := ingest(ctx, svc, rec("ev1", org, sess, "seatX", src, 1, "SYS-A")); r.Accepted != 1 || r.ContiguousSeq[src] != 1 {
		t.Fatalf("batch1: accepted=%d contiguous=%d want 1/1", r.Accepted, r.ContiguousSeq[src])
	}

	// exact re-delivery of ev1 → duplicated (idempotent)
	if r := ingest(ctx, svc, rec("ev1", org, sess, "seatX", src, 1, "SYS-A")); r.Duplicated != 1 {
		t.Fatalf("batch2: duplicated=%d want 1", r.Duplicated)
	}

	// same (src, seq=1) but a DIFFERENT event_id = pollution → quarantined (r3 #2)
	if r := ingest(ctx, svc, rec("evX", org, sess, "seatX", src, 1, "")); r.Quarantined != 1 {
		t.Fatalf("batch3: quarantined=%d want 1", r.Quarantined)
	}
	var ingestStatus string
	if err := db.QueryRowContext(ctx, "SELECT ingest_status FROM conversation_records WHERE event_id=?", "evX").Scan(&ingestStatus); err != nil {
		t.Fatalf("query evX: %v", err)
	}
	if ingestStatus != "quarantined" {
		t.Fatalf("evX ingest_status=%q want quarantined", ingestStatus)
	}

	// later turn, same session, SYS-B must NOT overwrite SYS-A (first-wins, r3 #4)
	ingest(ctx, svc, rec("ev2", org, sess, "seatX", src, 2, "SYS-B"))
	var sysText string
	if err := db.QueryRowContext(ctx, "SELECT system_text FROM conversation_sessions WHERE org_id=? AND session_id=?", org, sess).Scan(&sysText); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if sysText != "SYS-A" {
		t.Fatalf("session system_text=%q want SYS-A (first-wins)", sysText)
	}
	var sessRows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_sessions WHERE org_id=? AND session_id=?", org, sess).Scan(&sessRows)
	if sessRows != 1 {
		t.Fatalf("conversation_sessions rows=%d want 1", sessRows)
	}
}

// Watermark zipper advances over a contiguous run and STOPS at the first gap;
// once the gap is filled it zips past (the WAL-pruning contract).
func TestAdvanceWatermark_ZipperStopsAtGap(t *testing.T) {
	db := newConvTestDB(t)
	repo := NewSQLRepository(db)
	svc := NewService(repo)
	ctx := context.Background()
	org, src := "org1", "srcA"

	// arrive 1,2,3,5 — seq 4 missing
	for _, seq := range []int64{1, 2, 3, 5} {
		ingest(ctx, svc, rec("ev"+strconv.FormatInt(seq, 10), org, "s", "seatX", src, seq, ""))
	}
	c, err := repo.AdvanceWatermark(ctx, org, src, 5)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if c != 3 {
		t.Fatalf("contiguous=%d want 3 (stops before gap at 4)", c)
	}

	// fill the gap → contiguous zips to 5
	ingest(ctx, svc, rec("ev4", org, "s", "seatX", src, 4, ""))
	if c, _ = repo.AdvanceWatermark(ctx, org, src, 5); c != 5 {
		t.Fatalf("contiguous=%d want 5 after gap filled", c)
	}
}
