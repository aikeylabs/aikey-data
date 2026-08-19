package ingest

import "context"

// ODSRepository persists raw usage events to the ODS layer.
type ODSRepository interface {
	// InsertEvent inserts a single event. inserted is true on a fresh insert,
	// false on a duplicate event_id. quarantined sets the disposition columns
	// (ingest_status / dwd_status) so a content-hash-failed event is
	// stored-but-not-billed (stage C). conflict is true ONLY on a duplicate
	// whose STORED content_hash differs from this event's — a §6.2
	// corruption/tamper signal; the stored row is kept (never overwritten) and
	// the caller logs + counts the conflict.
	InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte, quarantined bool) (inserted, conflict bool, err error)

	// AdvanceWatermark recomputes the connected-no-gap high-water mark
	// (contiguous_seq) for one (orgID, sourceID) by walking the source's
	// stored source_seq values upward from the current watermark, stopping at
	// the first gap, and persists it to usage_source_watermark together with
	// max_seen_seq, client_allocated_seq, and last_event_at (the columns B' gap
	// detection reads). Returns the new contiguous_seq the client may safely
	// prune its WAL up to.
	//
	// clientAllocated is the client's allocator high-water from the batch
	// (BatchRequest.AllocatedSeq); pass 0 when the batch carries no allocated_seq
	// (v1 / older proxy) — the column is then left unchanged (monotonic MAX, 0 is
	// a no-op). All watermark columns are monotonic: a late/stale batch can never
	// lower a high-water mark. AdvanceWatermark is ledger-aware (stage D1): a seq
	// in usage_known_loss_ledger counts as present, so contiguous advances past it.
	AdvanceWatermark(ctx context.Context, orgID, sourceID string, clientAllocated int64) (contiguous int64, err error)

	// EnumerateMissingSeqs returns the seqs in (lo, hi] absent from BOTH ODS and
	// the known-loss ledger — the genuinely-unaccounted seqs the projector
	// promotes to known-loss once a gap is stale (stage D1). Bounded per call.
	EnumerateMissingSeqs(ctx context.Context, orgID, sourceID string, lo, hi int64) (seqs []int64, err error)

	// RecordKnownLoss appends seqs to the known-loss ledger (idempotent) and
	// re-advances the watermark so contiguous_seq zips past them. Returns the new
	// contiguous_seq (stage D1, server-timeout promotion).
	RecordKnownLoss(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (contiguous int64, err error)

	// GapSeqs returns the source's unaccounted seqs (bounded) so a client can
	// check them against its WAL (stage D3, client-confirmed reconciliation).
	GapSeqs(ctx context.Context, orgID, sourceID string, limit int64) (seqs []int64, truncated bool, err error)

	// ConfirmLost ledgers the client-declared-lost seqs that are GENUINELY absent
	// (guards against marking received seqs lost). Returns the count promoted +
	// new contiguous (stage D3).
	ConfirmLost(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (promoted int, contiguous int64, err error)
}

// BatchEventWriter runs a whole ingest batch inside ONE transaction (one
// commit + WAL fsync per batch instead of per event — the P0-4 batch rewrite).
// InsertEvent keeps the exact per-event semantics of ODSRepository.InsertEvent.
// Failed reports an infrastructure-level SQL failure inside the transaction
// (on PostgreSQL that poisons the tx); the caller then Rollbacks and replays
// the batch on the per-event autocommit path, which preserves per-event
// independence. Commit/Rollback close the transaction.
type BatchEventWriter interface {
	InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte, quarantined bool) (inserted, conflict bool, err error)
	Failed() bool
	Commit() error
	Rollback() error
}

// BatchCapableRepository is the optional capability a repository implements to
// enable the single-transaction batch path. The service type-asserts it: mock
// or legacy repositories without it transparently keep the per-event path.
type BatchCapableRepository interface {
	BeginBatch(ctx context.Context) (BatchEventWriter, error)
}
