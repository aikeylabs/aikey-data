package conversation

import (
	"context"
	"fmt"
	"log/slog"
)

// Service handles conversation record ingestion. Mirrors ingest.Service:
// per-record validate→insert, then ONE post-batch watermark advance per source.
type Service struct {
	repo        Repository
	pinnedOrgID string
}

// NewService creates a conversation ingest service. pinnedOrgID, when non-empty
// (single-tenant Cluster: CLUSTER_DELIVERY_ORG_ID), forces EVERY conversation
// record's org to that one fixed delivery org — an authoritative override of
// whatever org the proxy reported.
//
// Why: the Cluster edition is single-tenant (one cluster = one org, fixed). A
// form-① employee proxy resolves the seat's *home* org from its local VK cache,
// which is NOT the cluster delivery org (it has zero seats/VKs in this control
// DB); without this pin its conversations land in that phantom org and the audit
// page for the real org shows nothing. Same pin key-delivery/MyOrg use in
// control-master main.go ("one cluster = one org, in code, not deployment
// discipline"). Multi-tenant Production leaves it "" → trust the reported org.
func NewService(repo Repository, pinnedOrgID string) *Service {
	return &Service{repo: repo, pinnedOrgID: pinnedOrgID}
}

// IngestBatch processes a batch of conversation records. Each record is handled
// independently (a bad record doesn't block the rest); the per-source contiguous
// watermark is advanced once after the batch and returned so the proxy can prune
// its content WAL.
func (s *Service) IngestBatch(ctx context.Context, req *ConversationBatchRequest) (*ConversationBatchResponse, []RecordResult) {
	resp := &ConversationBatchResponse{}
	results := make([]RecordResult, 0, len(req.Records))

	type srcKey struct{ org, src string }
	touched := make(map[srcKey]struct{})

	for i := range req.Records {
		e := &req.Records[i]
		// Single-tenant cluster: pin org to the fixed delivery org BEFORE validate,
		// seq-owner keying, and watermark accounting — so the whole pipeline is
		// consistent on the one org (the proxy-reported org, e.g. a form-① seat's
		// phantom home org, is overridden). No-op when unpinned (multi-tenant).
		if s.pinnedOrgID != "" {
			e.OrgID = s.pinnedOrgID
		}
		r := s.ingestOne(ctx, e)
		results = append(results, r)
		switch r.Status {
		case "accepted":
			resp.Accepted++
		case "quarantined":
			resp.Quarantined++
		case "duplicated":
			resp.Duplicated++
		case "rejected":
			resp.Rejected++
			continue
		}
		// accepted/quarantined/duplicated all "arrived" (their seq is accounted),
		// so they count toward the watermark. v1/older proxy (no source identity)
		// skips gap tracking entirely.
		if e.SourceID != "" && e.SourceSeq != nil {
			touched[srcKey{org: e.OrgID, src: e.SourceID}] = struct{}{}
		}
	}

	// Advance the contiguous high-water per source and return it. A failure here
	// must NOT fail the batch (records ARE stored) — log + omit that source from
	// the map; the proxy then conserves (doesn't prune) and the next batch retries.
	var clientAllocated int64
	if req.AllocatedSeq != nil {
		clientAllocated = *req.AllocatedSeq
	}
	for k := range touched {
		contiguous, err := s.repo.AdvanceWatermark(ctx, k.org, k.src, clientAllocated)
		if err != nil {
			slog.Error("advance conversation watermark failed",
				"event.name", "conversation.watermark.advance_failed",
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

func (s *Service) ingestOne(ctx context.Context, e *ConversationRecord) RecordResult {
	if err := validate(e); err != nil {
		slog.Warn("conversation record rejected", "event_id", e.EventID, "reason", err)
		return RecordResult{EventID: e.EventID, Status: "rejected", Reason: err.Error()}
	}

	// Seq-conflict (r3 #2): if (org, source, seq) is already owned by a DIFFERENT
	// event_id, this incoming record is pollution → quarantine (store-but-flag).
	// A retransmit of the SAME event keeps the same owner → not a conflict.
	quarantined := false
	if e.SourceID != "" && e.SourceSeq != nil {
		owner, found, err := s.repo.SeqOwner(ctx, e.OrgID, e.SourceID, *e.SourceSeq)
		if err != nil {
			slog.Error("conversation seq-owner check failed",
				"event.name", "conversation.seq_owner.failed", "event_id", e.EventID, "error", err)
			return RecordResult{EventID: e.EventID, Status: "rejected", Reason: "internal error"}
		}
		if found && owner != e.EventID {
			quarantined = true
			slog.Warn("conversation record quarantined: source_seq conflict",
				"event.name", "conversation.source_seq.conflict",
				"error.code", "CONVERSATION_SOURCE_SEQ_CONFLICT",
				"org_id", e.OrgID, "source_id", e.SourceID, "source_seq", *e.SourceSeq,
				"incoming_event_id", e.EventID, "owner_event_id", owner)
		}
	}

	inserted, err := s.repo.InsertRecord(ctx, e, quarantined)
	if err != nil {
		slog.Error("insert conversation record failed", "event_id", e.EventID, "error", err)
		return RecordResult{EventID: e.EventID, Status: "rejected", Reason: "internal error"}
	}

	// system_text first-wins (r3 #4 / Q1 option A): store the system prompt ONCE
	// per session. Best-effort — the record is already stored; a session-metadata
	// hiccup must not fail the turn.
	//
	// Session scope key = the SEAT key (seat_id, owner fallback), NOT raw owner
	// (2026-07-07 seat-dimension attribution): query-service scopes system_text
	// lookups by the same key the audit UI navigates with, and
	// conversation_sessions has no seat column — its owner_account_id column
	// carries whichever key scopes the session. Legacy rows (pre-seat proxies)
	// hold owner, which is exactly what the query's COALESCE falls back to.
	if e.SystemText != "" {
		sessionScope := e.SeatID
		if sessionScope == "" {
			sessionScope = e.OwnerAccountID
		}
		if err := s.repo.UpsertSession(ctx, e.OrgID, e.SessionID, sessionScope, e.SystemText); err != nil {
			slog.Warn("upsert conversation session failed",
				"event.name", "conversation.session.upsert_failed",
				"org_id", e.OrgID, "session_id", e.SessionID, "error", err)
		}
	}

	if !inserted {
		return RecordResult{EventID: e.EventID, Status: "duplicated"}
	}
	if quarantined {
		return RecordResult{EventID: e.EventID, Status: "quarantined", Reason: "source_seq conflict"}
	}
	return RecordResult{EventID: e.EventID, Status: "accepted"}
}

func validate(e *ConversationRecord) error {
	if e.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if e.OrgID == "" {
		return fmt.Errorf("org_id is required")
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	if e.RequestStatus == "" {
		return fmt.Errorf("request_status is required")
	}
	return nil
}
