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
	svc := NewService(NewSQLRepository(db), "")
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
	svc := NewService(repo, "")
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

// Single-tenant pin: when NewService has a pinnedOrgID, EVERY record's org is
// forced to it — overriding whatever org the proxy reported. Guards the recurring
// single-tenant org regression (a form-① employee proxy reports the seat's phantom
// home org; dynamic per-VK org must never leak into stored records → audit page
// for the real delivery org would otherwise be empty).
func TestIngest_PinnedOrg_OverridesReportedOrg(t *testing.T) {
	db := newConvTestDB(t)
	const pinned, reported, src, sess = "org-DELIVERY-pinned", "org-PHANTOM-home", "srcA", "sessP"
	svc := NewService(NewSQLRepository(db), pinned)
	ctx := context.Background()

	if r := ingest(ctx, svc, rec("evP", reported, sess, "seatX", src, 1, "SYS")); r.Accepted != 1 {
		t.Fatalf("accepted=%d want 1", r.Accepted)
	}
	var gotOrg string
	if err := db.QueryRowContext(ctx, "SELECT org_id FROM conversation_records WHERE event_id=?", "evP").Scan(&gotOrg); err != nil {
		t.Fatalf("query evP: %v", err)
	}
	if gotOrg != pinned {
		t.Fatalf("record org_id=%q want %q (pin must override reported %q)", gotOrg, pinned, reported)
	}
	// session metadata + watermark must also live under the pinned org, never the reported one
	var sessOrg string
	if err := db.QueryRowContext(ctx, "SELECT org_id FROM conversation_sessions WHERE session_id=?", sess).Scan(&sessOrg); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if sessOrg != pinned {
		t.Fatalf("session org_id=%q want %q", sessOrg, pinned)
	}
	var phantomRows int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_records WHERE org_id=?", reported).Scan(&phantomRows)
	if phantomRows != 0 {
		t.Fatalf("found %d rows under reported phantom org %q, want 0 (pin must absorb all)", phantomRows, reported)
	}
}

// Multi-tenant (no pin): the reported org is preserved as-is — the pin is a
// single-tenant-only override and must not affect general Production.
func TestIngest_NoPin_PreservesReportedOrg(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()
	if r := ingest(ctx, svc, rec("evN", "org-tenantA", "sN", "seatX", "srcN", 1, "")); r.Accepted != 1 {
		t.Fatalf("accepted=%d want 1", r.Accepted)
	}
	var gotOrg string
	if err := db.QueryRowContext(ctx, "SELECT org_id FROM conversation_records WHERE event_id=?", "evN").Scan(&gotOrg); err != nil {
		t.Fatalf("query evN: %v", err)
	}
	if gotOrg != "org-tenantA" {
		t.Fatalf("record org_id=%q want org-tenantA (no pin → passthrough)", gotOrg)
	}
}

// seat_id round-trip through the REAL migration chain + IngestBatch — the
// schema-code-coherence fence for the 2026-07-07 seat-dimension attribution
// column (v1.0.1-alpha.4). Without it, a missing migration or a dropped
// INSERT column would be swallowed by INSERT OR IGNORE and report "accepted"
// while seat_id silently landed NULL.
func TestIngest_SeatIDPersists(t *testing.T) {
	db := newConvTestDB(t)
	svc := NewService(NewSQLRepository(db), "")
	ctx := context.Background()

	r := rec("evt-seat-1", "o1", "s1", "vk-owner-admin", "srcS", 1, "")
	r.SeatID = "seat-dbf603a1"
	if resp := ingest(ctx, svc, r); resp.Accepted != 1 {
		t.Fatalf("accepted=%d want 1", resp.Accepted)
	}

	var gotSeat, gotOwner string
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(seat_id,''), COALESCE(owner_account_id,'') FROM conversation_records WHERE event_id = ?",
		"evt-seat-1",
	).Scan(&gotSeat, &gotOwner); err != nil {
		t.Fatalf("select seat_id: %v", err)
	}
	if gotSeat != "seat-dbf603a1" || gotOwner != "vk-owner-admin" {
		t.Fatalf("seat_id=%q owner=%q — seat dimension must persist ALONGSIDE owner, not replace it", gotSeat, gotOwner)
	}

	// Legacy shape (no seat, older proxy): still accepted, seat lands NULL/''.
	if resp := ingest(ctx, svc, rec("evt-seat-2", "o1", "s1", "acct-7", "srcS", 2, "")); resp.Accepted != 1 {
		t.Fatalf("legacy record rejected")
	}
	var legacySeat sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT seat_id FROM conversation_records WHERE event_id = ?", "evt-seat-2",
	).Scan(&legacySeat); err != nil {
		t.Fatalf("select legacy seat: %v", err)
	}
	if legacySeat.Valid && legacySeat.String != "" {
		t.Fatalf("legacy record seat_id=%q, want NULL/empty", legacySeat.String)
	}
}
