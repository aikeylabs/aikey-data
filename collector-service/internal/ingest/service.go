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
func (s *Service) IngestBatch(ctx context.Context, req *BatchRequest) (*BatchResponse, []EventResult) {
	resp := &BatchResponse{}
	results := make([]EventResult, 0, len(req.Events))

	// Track (org_id, source_id) pairs touched by stored events so we can
	// advance the per-source delivery-integrity watermark once after the batch
	// (not per-event). Only events that landed in ODS (accepted OR duplicated —
	// a duplicate's seq is already stored) and carry a v2 source identity count.
	type srcKey struct{ org, src string }
	touched := make(map[srcKey]struct{})

	for i := range req.Events {
		e := &req.Events[i]
		r := s.ingestOne(ctx, e)
		results = append(results, r)
		switch r.Status {
		case "accepted":
			resp.Accepted++
			s.metrics.Accepted.Add(1)
		case "quarantined":
			// Stored but content-hash-failed (stage C): counted separately, NOT
			// billed. Falls through to watermark tracking below — its seq DID
			// arrive (no gap); quarantine is a content-quality flag, orthogonal
			// to delivery completeness. Metric was incremented in ingestOne.
			resp.Quarantined++
		case "duplicated":
			resp.Duplicated++
			s.metrics.Duplicated.Add(1)
		case "rejected":
			resp.Rejected++
			s.metrics.Rejected.Add(1)
			continue
		}
		// SourceID empty (v1 / older proxy) → skip gap tracking entirely.
		if e.SourceID != "" && e.SourceSeq != nil {
			touched[srcKey{org: e.OrgID, src: e.SourceID}] = struct{}{}
		}
	}

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
	for k := range touched {
		contiguous, err := s.repo.AdvanceWatermark(ctx, k.org, k.src, clientAllocated)
		if err != nil {
			slog.Error("advance watermark failed",
				"event.name", "ingest.watermark.advance_failed",
				"org_id", k.org, "source_id", k.src, "error", err)
			continue
		}
		if resp.ContiguousSeq == nil {
			resp.ContiguousSeq = make(map[string]int64, len(touched))
		}
		resp.ContiguousSeq[k.src] = contiguous
	}
	return resp, results
}

func (s *Service) ingestOne(ctx context.Context, e *UsageEvent) EventResult {
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
		s.metrics.Quarantined.Add(1)
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

	inserted, conflict, err := s.repo.InsertEvent(ctx, e, rawJSON, quarantined)
	if err != nil {
		slog.Error("insert event failed", "event_id", e.EventID, "error", err)
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: "internal error"}
	}
	if conflict {
		// §6.2: same event_id re-delivered with a DIFFERENT content_hash. The
		// first-stored row is kept (we do NOT overwrite — never silently pick
		// one); surface the conflict loudly for review.
		s.metrics.Conflicts.Add(1)
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
		return EventResult{EventID: e.EventID, Status: "duplicated"}
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
