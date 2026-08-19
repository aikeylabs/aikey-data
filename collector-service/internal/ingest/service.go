package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/AiKeyLabs/pkg/usagehash"
)

// Central event.name / error.code constants for stage-C content-hash handling
// (logging-conventions: codes are constants, not string literals scattered
// across call sites). event.name is lowercase dotted; error.code is UPPER_SNAKE.
const (
	eventContentHashMismatch   = "ingest.content_hash.mismatch"
	eventContentHashConflict   = "ingest.content_hash.conflict"
	errCodeContentHashMismatch = "CONTENT_HASH_MISMATCH"
	errCodeContentHashConflict = "CONTENT_HASH_CONFLICT"
)

// Service handles usage event ingestion logic.
type Service struct {
	repo    ODSRepository
	metrics Metrics
}

// NewService creates an ingest service.
func NewService(repo ODSRepository) *Service {
	return &Service{repo: repo}
}

// MetricsSnapshot returns current ingest counters.
func (s *Service) MetricsSnapshot() MetricsSnapshot {
	return s.metrics.Snapshot()
}

// IngestBatch processes a batch of usage events.
// Each event is validated and inserted independently; a single bad event
// does not block the rest of the batch.
//
// P0-4 batch rewrite: when the repository supports it, the whole batch runs
// inside ONE transaction (one commit + WAL fsync per batch instead of one per
// event — the per-event autocommit was the audit pipeline's dominant cost at
// high event rates). Per-event independence is preserved by construction: an
// infrastructure-level SQL failure inside the transaction (which on PostgreSQL
// poisons it) rolls the whole attempt back — nothing was committed — and the
// batch is replayed on the classic per-event path below, where a poisoned
// event only rejects itself. Logical outcomes (duplicates, quarantine, F2
// swallow detection) are per-row inside the transaction and do not trigger
// replay.
func (s *Service) IngestBatch(ctx context.Context, req *BatchRequest) (*BatchResponse, []EventResult) {
	if bc, ok := s.repo.(BatchCapableRepository); ok {
		if resp, results, ok := s.ingestBatchTx(ctx, req, bc); ok {
			return resp, results
		}
		// Fall through: the transaction was rolled back (nothing durable) —
		// replay per-event. WARN was logged by ingestBatchTx.
	}
	return s.ingestPerEvent(ctx, req)
}

// ingestBatchTx attempts the whole batch in one transaction. ok=false means the
// attempt was rolled back and the caller must replay per-event.
func (s *Service) ingestBatchTx(ctx context.Context, req *BatchRequest, bc BatchCapableRepository) (*BatchResponse, []EventResult, bool) {
	bw, err := bc.BeginBatch(ctx)
	if err != nil {
		slog.Warn("ingest batch tx begin failed; falling back to per-event",
			"event.name", "ingest.batch_tx.begin_failed", "error", err)
		return nil, nil, false
	}
	results := make([]EventResult, 0, len(req.Events))
	for i := range req.Events {
		results = append(results, s.ingestOne(ctx, &req.Events[i], bw))
		if bw.Failed() {
			// Infra failure inside the tx (poisons it on PostgreSQL). Roll back —
			// nothing committed — and replay the whole batch per-event so the
			// failing event only rejects itself.
			_ = bw.Rollback()
			slog.Warn("ingest batch tx failed; replaying per-event",
				"event.name", "ingest.batch_tx.replay", "events", len(req.Events))
			return nil, nil, false
		}
	}
	if err := bw.Commit(); err != nil {
		_ = bw.Rollback()
		slog.Warn("ingest batch tx commit failed; replaying per-event",
			"event.name", "ingest.batch_tx.commit_failed", "error", err)
		return nil, nil, false
	}
	resp := s.tallyResults(req, results)
	s.advanceWatermarks(ctx, req, resp)
	return resp, results, true
}

// ingestPerEvent is the classic per-event autocommit path — the pre-batch
// implementation, kept verbatim as the fallback and for repositories without
// batch capability (mocks, legacy).
func (s *Service) ingestPerEvent(ctx context.Context, req *BatchRequest) (*BatchResponse, []EventResult) {
	results := make([]EventResult, 0, len(req.Events))
	for i := range req.Events {
		results = append(results, s.ingestOne(ctx, &req.Events[i], s.repo))
	}
	resp := s.tallyResults(req, results)
	s.advanceWatermarks(ctx, req, resp)
	return resp, results
}

// tallyResults builds the batch response counters, bumps metrics, and marks
// which (org, source) pairs need a watermark advance. Metrics are counted here
// — ONCE per finally-committed outcome — so a rolled-back batch attempt never
// double-counts (the replay's results are the only ones tallied).
func (s *Service) tallyResults(req *BatchRequest, results []EventResult) *BatchResponse {
	resp := &BatchResponse{}
	for i := range results {
		e := &req.Events[i]
		switch results[i].Status {
		case "accepted":
			resp.Accepted++
			s.metrics.Accepted.Add(1)
		case "quarantined":
			// Stored but content-hash-failed (stage C): counted separately, NOT
			// billed. Still watermark-tracked below — its seq DID arrive (no gap);
			// quarantine is a content-quality flag, orthogonal to delivery
			// completeness.
			resp.Quarantined++
			s.metrics.Quarantined.Add(1)
		case "duplicated":
			resp.Duplicated++
			s.metrics.Duplicated.Add(1)
			if results[i].contentHashConflict {
				s.metrics.Conflicts.Add(1)
			}
		case "rejected":
			resp.Rejected++
			s.metrics.Rejected.Add(1)
			continue
		}
		// SourceID empty (v1 / older proxy) → skip gap tracking entirely.
		if e.SourceID != "" && e.SourceSeq != nil {
			if resp.touched == nil {
				resp.touched = make(map[srcKey]struct{})
			}
			resp.touched[srcKey{org: e.OrgID, src: e.SourceID}] = struct{}{}
		}
	}
	return resp
}

// advanceWatermarks runs the single post-batch watermark advance per touched
// source (outside any batch transaction — a watermark failure must never fail
// the batch; events ARE stored).
func (s *Service) advanceWatermarks(ctx context.Context, req *BatchRequest, resp *BatchResponse) {

	// Advance the contiguous high-water mark for every source in this batch and
	// return it so the client can safely prune its WAL up to that seq. A failure
	// here must NOT fail the batch (the events ARE stored) — log and omit that
	// source from the map; the client then conserves (doesn't prune), and the
	// next batch retries the advance. Map stays nil when no v2 source touched,
	// so the wire field is omitted for pure-v1 batches (old-client compatible).
	// One proxy = one source identity, so the batch's single AllocatedSeq scalar
	// (the client's allocator high-water) applies to every touched (org,source).
	// nil → 0 = "no info"; AdvanceWatermark leaves client_allocated_seq unchanged.
	var clientAllocated int64
	if req.AllocatedSeq != nil {
		clientAllocated = *req.AllocatedSeq
	}
	for k := range resp.touched {
		contiguous, err := s.repo.AdvanceWatermark(ctx, k.org, k.src, clientAllocated)
		if err != nil {
			slog.Error("advance watermark failed",
				"event.name", "ingest.watermark.advance_failed",
				"org_id", k.org, "source_id", k.src, "error", err)
			continue
		}
		if resp.ContiguousSeq == nil {
			resp.ContiguousSeq = make(map[string]int64, len(resp.touched))
		}
		resp.ContiguousSeq[k.src] = contiguous
	}
}

// eventInserter is the write surface ingestOne needs — satisfied by both the
// plain ODSRepository (autocommit) and a BatchEventWriter (one tx per batch).
type eventInserter interface {
	InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte, quarantined bool) (inserted, conflict bool, err error)
}

func (s *Service) ingestOne(ctx context.Context, e *UsageEvent, ins eventInserter) EventResult {
	if err := validate(e); err != nil {
		slog.Warn("event rejected", "event_id", e.EventID, "reason", err)
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: err.Error()}
	}

	// Default schema version
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	// Warn on unknown schema version but still ingest (forward-compatible).
	// A newer proxy may send v2 events before collector is upgraded.
	// Missing new fields will be zero-valued, which is acceptable.
	if e.SchemaVersion > MaxSchemaVersion {
		slog.Warn("ingest: unknown schema version, ingesting anyway",
			"event_id", e.EventID,
			"got", e.SchemaVersion,
			"max_supported", MaxSchemaVersion)
	}
	// Default request count
	if e.RequestCount == 0 {
		e.RequestCount = 1
	}

	rawJSON, err := json.Marshal(e)
	if err != nil {
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: "marshal raw event: " + err.Error()}
	}

	// Stage C — strict content-hash validation (§5.7). RECOMPUTE the hash from
	// this event's own metering fields and compare to the client-stamped value.
	// A mismatch means a field was corrupted between the client hashing it and
	// us reading it (transit/schema drift) — we QUARANTINE the event (store but
	// don't bill) and alert, rather than silently billing a value that may have
	// become 0. Only events carrying a KNOWN scheme are validated; an empty hash
	// (older proxy) or an unrecognized future scheme is skipped (conserve —
	// accept normally, never false-quarantine; see pkg/usagehash.SchemeKnown).
	quarantined := false
	if usagehash.SchemeKnown(e.ContentHash) && !usagehash.Verify(contentHashInput(e), e.ContentHash) {
		quarantined = true
		// Metric counted in tallyResults (once per finally-committed outcome).
		slog.Warn("usage event quarantined: content_hash mismatch",
			"event.name", eventContentHashMismatch,
			"error.code", errCodeContentHashMismatch,
			"trace_id", e.TraceID,
			"event_id", e.EventID,
			"org_id", e.OrgID,
			"source_id", e.SourceID,
			"source_seq", e.SourceSeq,
			"stamped", e.ContentHash,
			"recomputed", usagehash.Compute(contentHashInput(e)),
		)
	}

	inserted, conflict, err := ins.InsertEvent(ctx, e, rawJSON, quarantined)
	if err != nil {
		slog.Error("insert event failed", "event_id", e.EventID, "error", err)
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: "internal error"}
	}
	if conflict {
		// §6.2: same event_id re-delivered with a DIFFERENT content_hash. The
		// first-stored row is kept (we do NOT overwrite — never silently pick
		// one); surface the conflict loudly for review. Metric counted in
		// tallyResults via the result flag.
		slog.Warn("usage event content_hash conflict on duplicate",
			"event.name", eventContentHashConflict,
			"error.code", errCodeContentHashConflict,
			"trace_id", e.TraceID,
			"event_id", e.EventID,
			"org_id", e.OrgID,
			"incoming", e.ContentHash,
		)
	}
	if !inserted {
		return EventResult{EventID: e.EventID, Status: "duplicated", contentHashConflict: conflict}
	}
	if quarantined {
		return EventResult{EventID: e.EventID, Status: "quarantined", Reason: "content_hash mismatch"}
	}
	return EventResult{EventID: e.EventID, Status: "accepted"}
}

// contentHashInput projects a UsageEvent's metering fields into the canonical
// hash input, deref-or-zero on the *int64 pointers EXACTLY as the proxy does
// (a nil/omitted field on the wire hashes as 0 on both sides). The server field
// CachedInputTokens carries the wire field cache_read_input_tokens — the same
// value the proxy hashed as CacheReadInputTokens.
func contentHashInput(e *UsageEvent) usagehash.Input {
	return usagehash.Input{
		InputTokens:              derefI64(e.InputTokens),
		OutputTokens:             derefI64(e.OutputTokens),
		TotalTokens:              derefI64(e.TotalTokens),
		CacheReadInputTokens:     derefI64(e.CachedInputTokens),
		CacheCreationInputTokens: derefI64(e.CacheCreationInputTokens),
		Model:                    e.Model,
		ProviderCode:             e.ProviderCode,
	}
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func validate(e *UsageEvent) error {
	if e.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if e.OrgID == "" {
		return fmt.Errorf("org_id is required")
	}
	if e.EventTime.IsZero() {
		return fmt.Errorf("event_time is required")
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	if e.RequestStatus == "" {
		return fmt.Errorf("request_status is required")
	}
	return nil
}
