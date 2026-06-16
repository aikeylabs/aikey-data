package conversation

import "context"

// Repository persists conversation records + per-session system prompt +
// per-source delivery watermark. The interface mirrors the relevant subset of
// internal/ingest.ODSRepository (InsertEvent / AdvanceWatermark) so the two
// streams read the same; known-loss promotion (EnumerateMissingSeqs /
// RecordKnownLoss) is a P3 collector-worker concern and not part of P1.
type Repository interface {
	// SeqOwner returns the event_id that already owns (orgID, sourceID, seq), if
	// any. Used to detect the pollution case (same source_seq claimed by a
	// DIFFERENT event_id) so the incoming record can be quarantined (r3 #2).
	SeqOwner(ctx context.Context, orgID, sourceID string, seq int64) (eventID string, found bool, err error)

	// InsertRecord inserts one turn idempotently on (event_id, conv_date).
	// inserted is true on a fresh insert, false on a genuine duplicate.
	// quarantined sets ingest_status='quarantined' (stored-but-flagged) — its
	// source_seq still arrived (no gap), so it still counts for watermark advance.
	InsertRecord(ctx context.Context, e *ConversationRecord, quarantined bool) (inserted bool, err error)

	// UpsertSession stores a session's system prompt ONCE (first-wins via
	// ON CONFLICT (org_id, session_id) DO NOTHING) — the stateless proxy resends
	// system_text every turn; the collector keeps the first (r3 #4 / system_text A).
	UpsertSession(ctx context.Context, orgID, sessionID, ownerAccountID, systemText string) error

	// AdvanceWatermark recomputes the connected-no-gap high-water (contiguous_seq)
	// for one (orgID, sourceID) — walks stored source_seq upward from the current
	// watermark, stopping at the first gap, treating known-loss-ledger seqs as
	// present (so a promoted gap lets contiguous advance past it). Persists
	// contiguous + max_seen + client_allocated + last_event_at. Returns the new
	// contiguous_seq the proxy may prune its content WAL up to. Mirrors
	// ingest.ODSRepository.AdvanceWatermark; self-contained (touches only the
	// conversation_* tables, never the usage path).
	AdvanceWatermark(ctx context.Context, orgID, sourceID string, clientAllocated int64) (contiguous int64, err error)
}
