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

	// --- P3 known-loss promotion (collector worker; mirrors usage's
	// integrity/projector stale-gap promotion but self-contained on the
	// conversation_* tables — never touches the usage path) ---

	// ScanStaleGaps returns every source whose contiguous_seq trails its
	// high-water (a delivery gap) AND whose last_event_at predates staleBeforeMs
	// — by then the content WAL has had ample time to re-deliver, so a
	// still-missing seq is genuinely lost (not just in-flight).
	ScanStaleGaps(ctx context.Context, staleBeforeMs int64) ([]StaleGap, error)

	// EnumerateMissingSeqs returns the seqs in (contiguous, hi] that are neither
	// stored in conversation_records nor already in the known-loss ledger.
	EnumerateMissingSeqs(ctx context.Context, orgID, sourceID string, contiguous, hi int64) ([]int64, error)

	// RecordKnownLoss ledgers the given lost seqs (idempotent) then re-advances
	// the watermark past them, returning the new contiguous_seq the proxy may
	// prune its content WAL up to. Without this a permanently-lost seq would
	// stall contiguous_seq forever → the content WAL could never prune.
	RecordKnownLoss(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (contiguous int64, err error)
}

// BatchRecordWriter runs a whole conversation batch inside ONE transaction
// (one commit + WAL fsync per batch — P0-4 batch rewrite). Same per-record
// semantics as Repository; SeqOwner sees the batch's own uncommitted rows
// (read-your-own-writes) so intra-batch seq conflicts are still caught.
// Failed reports an infrastructure-level SQL failure (poisons a PostgreSQL
// tx); the caller Rollbacks and replays per-record.
type BatchRecordWriter interface {
	SeqOwner(ctx context.Context, orgID, sourceID string, seq int64) (eventID string, found bool, err error)
	InsertRecord(ctx context.Context, e *ConversationRecord, quarantined bool) (inserted bool, err error)
	UpsertSession(ctx context.Context, orgID, sessionID, ownerAccountID, systemText string) error
	Failed() bool
	Commit() error
	Rollback() error
}

// BatchCapableRepository is the optional capability enabling the batch path;
// repositories without it keep the per-record path.
type BatchCapableRepository interface {
	BeginBatch(ctx context.Context) (BatchRecordWriter, error)
}

// StaleGap is one source with an aged delivery gap, returned by ScanStaleGaps.
type StaleGap struct {
	OrgID           string
	SourceID        string
	Contiguous      int64
	MaxSeen         int64
	ClientAllocated int64
}

// HighWater is the largest seq the source could possibly deliver — the ceiling
// for missing-seq enumeration (covers both middle gaps ≤ max_seen and tail gaps
// up to the reserved-ahead client_allocated).
func (g StaleGap) HighWater() int64 {
	hi := g.MaxSeen
	if g.ClientAllocated > hi {
		hi = g.ClientAllocated
	}
	return hi
}
