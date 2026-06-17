package conversation

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

type sqlRepo struct{ db *shared.DB }

// NewSQLRepository builds the dialect-aware conversation repository.
func NewSQLRepository(db *shared.DB) Repository { return &sqlRepo{db: db} }

// Column list mirrors the conversation_records DDL (v1.0.1-alpha.2 migration).
// system_text is NOT here — it lives once-per-session in conversation_sessions.
const convColumns = "event_id, conv_date, org_id, session_id, owner_account_id, " +
	"virtual_key_id, source_id, source_seq, model, provider_code, user_text, assistant_text, " +
	"input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, " +
	"total_tokens, duration_ms, request_status, content_bytes, ingest_status, created_at"

const convPlaceholders = "?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?"

// advanceWatermarkScanLimit bounds the per-call source_seq window (mirrors usage).
const advanceWatermarkScanLimit = 10000

// convDate derives the UTC partition date 'YYYY-MM-DD' from the stamp-once
// created_at (epoch ms). Deterministic from the carried CreatedAt → a retransmit
// always yields the same conv_date, so (event_id, conv_date) is a stable dedup
// key (the same property usage relies on for event_time).
func convDate(e *ConversationRecord) string {
	return time.UnixMilli(int64(e.CreatedAt)).UTC().Format("2006-01-02")
}

func (r *sqlRepo) SeqOwner(ctx context.Context, orgID, sourceID string, seq int64) (string, bool, error) {
	var eventID string
	err := r.db.QueryRowContext(ctx,
		"SELECT event_id FROM conversation_records WHERE org_id = ? AND source_id = ? AND source_seq = ? LIMIT 1",
		orgID, sourceID, seq,
	).Scan(&eventID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("seq owner org=%s source=%s seq=%d: %w", orgID, sourceID, seq, err)
	}
	return eventID, true, nil
}

func (r *sqlRepo) InsertRecord(ctx context.Context, e *ConversationRecord, quarantined bool) (bool, error) {
	ingestStatus := "accepted"
	if quarantined {
		ingestStatus = "quarantined"
	}
	cd := convDate(e)
	// Conflict target = the PK. PostgreSQL infers (event_id, conv_date) — the
	// partition key must be in it; SQLite's INSERT OR IGNORE ignores the target.
	insert := r.db.InsertOrIgnoreOn("conversation_records", convColumns, convPlaceholders, "event_id, conv_date")
	res, err := r.db.ExecContext(ctx, insert,
		e.EventID, cd, e.OrgID, nullStr(e.SessionID), nullStr(e.OwnerAccountID),
		nullStr(e.VirtualKeyID), nullStr(e.SourceID), e.SourceSeq, nullStr(e.Model), nullStr(e.ProviderCode),
		nullStr(e.UserText), nullStr(e.AssistantText),
		e.InputTokens, e.OutputTokens, e.CachedInputTokens, e.CacheCreationInputTokens, e.ReasoningTokens,
		e.TotalTokens, e.DurationMs, e.RequestStatus, e.ContentBytes, ingestStatus, r.db.BindMillis(e.CreatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("insert conversation record %s: %w", e.EventID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// rowsAffected==0: verify it's a genuine (event_id, conv_date) duplicate, not a
	// swallowed NOT NULL/CHECK violation. Mirrors usage InsertEvent's F2 guard —
	// never report "duplicated" while data was silently dropped.
	var got string
	verr := r.db.QueryRowContext(ctx,
		"SELECT event_id FROM conversation_records WHERE org_id = ? AND event_id = ? AND conv_date = ? LIMIT 1",
		e.OrgID, e.EventID, cd,
	).Scan(&got)
	if verr == nil {
		return false, nil // genuine duplicate
	}
	if verr == sql.ErrNoRows {
		return false, fmt.Errorf("conversation INSERT silently ignored (no PK conflict on org=%s event_id=%s conv_date=%s) — likely NOT NULL/CHECK violation", e.OrgID, e.EventID, cd)
	}
	return false, fmt.Errorf("verify dedup event_id=%s: %w", e.EventID, verr)
}

func (r *sqlRepo) UpsertSession(ctx context.Context, orgID, sessionID, ownerAccountID, systemText string) error {
	// first_seen_at is the dialect-aware NowMillis() SQL expression inlined into
	// the VALUES list (PG NOW()-epoch / SQLite epoch-ms); the other 4 are bound.
	insert := r.db.InsertOrIgnoreOn("conversation_sessions",
		"org_id, session_id, owner_account_id, system_text, first_seen_at",
		fmt.Sprintf("?,?,?,?,%s", r.db.NowMillis()),
		"org_id, session_id")
	if _, err := r.db.ExecContext(ctx, insert, orgID, sessionID, nullStr(ownerAccountID), nullStr(systemText)); err != nil {
		return fmt.Errorf("upsert session org=%s session=%s: %w", orgID, sessionID, err)
	}
	return nil
}

// AdvanceWatermark — see Repository.AdvanceWatermark. Zipper-advances the
// connected-no-gap high-water for one source and persists it. Ledger-aware:
// a seq in conversation_known_loss_ledger counts as present (so contiguous
// advances past a promoted gap). Mirrors ingest.AdvanceWatermark exactly,
// against the conversation_* tables.
func (r *sqlRepo) AdvanceWatermark(ctx context.Context, orgID, sourceID string, clientAllocated int64) (int64, error) {
	var contiguous int64
	err := r.db.QueryRowContext(ctx,
		"SELECT contiguous_seq FROM conversation_source_watermark WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&contiguous)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("read watermark org=%s source=%s: %w", orgID, sourceID, err)
	}

	hi := contiguous + advanceWatermarkScanLimit
	present, err := r.presentSeqSet(ctx, orgID, sourceID, contiguous, hi)
	if err != nil {
		return 0, err
	}
	for present[contiguous+1] {
		contiguous++
	}

	var maxSeen int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(source_seq), 0) FROM conversation_records WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&maxSeen); err != nil {
		return 0, fmt.Errorf("max_seen org=%s source=%s: %w", orgID, sourceID, err)
	}

	now := r.db.NowMillis()
	maxSeenG := r.db.Greatest("conversation_source_watermark.max_seen_seq", "excluded.max_seen_seq")
	clientAllocG := r.db.Greatest("conversation_source_watermark.client_allocated_seq", "excluded.client_allocated_seq")
	upsert := fmt.Sprintf(
		`INSERT INTO conversation_source_watermark (org_id, source_id, contiguous_seq, max_seen_seq, client_allocated_seq, last_event_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, %s, %s)
		 ON CONFLICT (org_id, source_id) DO UPDATE SET
		   contiguous_seq       = ?,
		   max_seen_seq         = %s,
		   client_allocated_seq = %s,
		   last_event_at        = %s,
		   updated_at           = %s`,
		now, now, maxSeenG, clientAllocG, now, now,
	)
	if _, err := r.db.ExecContext(ctx, upsert, orgID, sourceID, contiguous, maxSeen, clientAllocated, contiguous); err != nil {
		return 0, fmt.Errorf("upsert watermark org=%s source=%s: %w", orgID, sourceID, err)
	}
	return contiguous, nil
}

// presentSeqSet returns the seqs in (lo, hi] that count as present for
// contiguity: stored in conversation_records OR ledgered as known-loss. NULL
// source_seq rows (older proxy) are excluded by the `> ?` comparison.
func (r *sqlRepo) presentSeqSet(ctx context.Context, orgID, sourceID string, lo, hi int64) (map[int64]bool, error) {
	present := make(map[int64]bool)
	recRows, err := r.db.QueryContext(ctx,
		"SELECT source_seq FROM conversation_records WHERE org_id = ? AND source_id = ? AND source_seq > ? AND source_seq <= ?",
		orgID, sourceID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("scan record seqs org=%s source=%s: %w", orgID, sourceID, err)
	}
	for recRows.Next() {
		var seq int64
		if err := recRows.Scan(&seq); err != nil {
			recRows.Close()
			return nil, err
		}
		present[seq] = true
	}
	recRows.Close()
	if err := recRows.Err(); err != nil {
		return nil, err
	}
	ledRows, err := r.db.QueryContext(ctx,
		"SELECT seq FROM conversation_known_loss_ledger WHERE org_id = ? AND source_id = ? AND seq > ? AND seq <= ?",
		orgID, sourceID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("scan ledger seqs org=%s source=%s: %w", orgID, sourceID, err)
	}
	for ledRows.Next() {
		var seq int64
		if err := ledRows.Scan(&seq); err != nil {
			ledRows.Close()
			return nil, err
		}
		present[seq] = true
	}
	ledRows.Close()
	return present, ledRows.Err()
}

// ScanStaleGaps — see Repository.ScanStaleGaps. Self-contained on
// conversation_source_watermark; staleBeforeMs is bound dialect-aware so the
// last_event_at comparison works on PG (TIMESTAMPTZ) and SQLite (INTEGER ms).
func (r *sqlRepo) ScanStaleGaps(ctx context.Context, staleBeforeMs int64) ([]StaleGap, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT org_id, source_id, contiguous_seq, max_seen_seq, client_allocated_seq
		   FROM conversation_source_watermark
		  WHERE (max_seen_seq > contiguous_seq OR client_allocated_seq > contiguous_seq)
		    AND last_event_at < ?`,
		r.db.BindMillis(aikeytime.Millis(staleBeforeMs)))
	if err != nil {
		return nil, fmt.Errorf("scan stale gaps: %w", err)
	}
	defer rows.Close()
	var out []StaleGap
	for rows.Next() {
		var g StaleGap
		if err := rows.Scan(&g.OrgID, &g.SourceID, &g.Contiguous, &g.MaxSeen, &g.ClientAllocated); err != nil {
			return nil, fmt.Errorf("scan stale gap row: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// EnumerateMissingSeqs — see Repository.EnumerateMissingSeqs. Bounded by
// advanceWatermarkScanLimit (same as AdvanceWatermark) so a huge gap promotes
// across several ticks rather than one giant scan.
func (r *sqlRepo) EnumerateMissingSeqs(ctx context.Context, orgID, sourceID string, contiguous, hi int64) ([]int64, error) {
	if scanHi := contiguous + advanceWatermarkScanLimit; hi > scanHi {
		hi = scanHi
	}
	if hi <= contiguous {
		return nil, nil
	}
	present, err := r.presentSeqSet(ctx, orgID, sourceID, contiguous, hi)
	if err != nil {
		return nil, err
	}
	var missing []int64
	for seq := contiguous + 1; seq <= hi; seq++ {
		if !present[seq] {
			missing = append(missing, seq)
		}
	}
	return missing, nil
}

// RecordKnownLoss — see Repository.RecordKnownLoss. Ledgers each lost seq
// idempotently (ON CONFLICT (org_id, source_id, seq) DO NOTHING), then
// re-advances the watermark: presentSeqSet now counts the ledgered seqs, so the
// zipper steps past the promoted gap. clientAllocated=0 is safe — AdvanceWatermark
// keeps the GREATEST existing value, so it never shrinks the reserved high-water.
func (r *sqlRepo) RecordKnownLoss(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (int64, error) {
	insert := r.db.InsertOrIgnoreOn(
		"conversation_known_loss_ledger", "org_id, source_id, seq, reason", "?, ?, ?, ?", "org_id, source_id, seq")
	for _, seq := range seqs {
		if _, err := r.db.ExecContext(ctx, insert, orgID, sourceID, seq, reason); err != nil {
			return 0, fmt.Errorf("ledger known-loss seq=%d org=%s source=%s: %w", seq, orgID, sourceID, err)
		}
	}
	return r.AdvanceWatermark(ctx, orgID, sourceID, 0)
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
