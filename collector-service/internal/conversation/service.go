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
//
// P0-4 batch rewrite: when the repository supports it the whole batch runs in
// ONE transaction (one commit + WAL fsync per batch — conversation rows carry
// large text fields, so the per-record commit tax was the heaviest here). An
// infra SQL failure rolls the attempt back (nothing durable) and the batch
// replays on the classic per-record path, preserving per-record independence.
func (s *Service) IngestBatch(ctx context.Context, req *ConversationBatchRequest) (*ConversationBatchResponse, []RecordResult) {
	// Single-tenant cluster: pin org BEFORE validate, seq-owner keying, and
	// watermark accounting (no-op when unpinned). Done once up front so both the
	// batch attempt and a per-record replay see identical records.
	if s.pinnedOrgID != "" {
		for i := range req.Records {
			req.Records[i].OrgID = s.pinnedOrgID
		}
	}
	if bc, ok := s.repo.(BatchCapableRepository); ok {
		if resp, results, ok := s.ingestBatchTx(ctx, req, bc); ok {
			return resp, results
		}
	}
	return s.ingestPerRecord(ctx, req)
}

// ingestBatchTx attempts the whole batch in one transaction; ok=false means it
// was rolled back and the caller must replay per-record.
func (s *Service) ingestBatchTx(ctx context.Context, req *ConversationBatchRequest, bc BatchCapableRepository) (*ConversationBatchResponse, []RecordResult, bool) {
	bw, err := bc.BeginBatch(ctx)
	if err != nil {
		slog.Warn("conversation batch tx begin failed; falling back to per-record",
			"event.name", "conversation.batch_tx.begin_failed", "error", err)
		return nil, nil, false
	}
	results := make([]RecordResult, 0, len(req.Records))
	for i := range req.Records {
		results = append(results, s.ingestOne(ctx, &req.Records[i], bw))
		if bw.Failed() {
			_ = bw.Rollback()
			slog.Warn("conversation batch tx failed; replaying per-record",
				"event.name", "conversation.batch_tx.replay", "records", len(req.Records))
			return nil, nil, false
		}
	}
	if err := bw.Commit(); err != nil {
		_ = bw.Rollback()
		slog.Warn("conversation batch tx commit failed; replaying per-record",
			"event.name", "conversation.batch_tx.commit_failed", "error", err)
		return nil, nil, false
	}
	resp, touched := s.tallyResults(req, results)
	s.advanceWatermarks(ctx, req, resp, touched)
	return resp, results, true
}

// ingestPerRecord is the classic per-record autocommit path (fallback + non-
// batch-capable repositories).
func (s *Service) ingestPerRecord(ctx context.Context, req *ConversationBatchRequest) (*ConversationBatchResponse, []RecordResult) {
	results := make([]RecordResult, 0, len(req.Records))
	for i := range req.Records {
		results = append(results, s.ingestOne(ctx, &req.Records[i], s.repo))
	}
	resp, touched := s.tallyResults(req, results)
	s.advanceWatermarks(ctx, req, resp, touched)
	return resp, results
}

type convSrcKey struct{ org, src string }

func (s *Service) tallyResults(req *ConversationBatchRequest, results []RecordResult) (*ConversationBatchResponse, map[convSrcKey]struct{}) {
	resp := &ConversationBatchResponse{}
	touched := make(map[convSrcKey]struct{})
	for i := range results {
		e := &req.Records[i]
		switch results[i].Status {
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
			touched[convSrcKey{org: e.OrgID, src: e.SourceID}] = struct{}{}
		}
	}
	return resp, touched
}

// advanceWatermarks runs the single post-batch watermark advance per touched
// source (outside any batch tx). A failure must NOT fail the batch (records
// ARE stored) — log + omit that source; the proxy conserves and the next batch
// retries.
func (s *Service) advanceWatermarks(ctx context.Context, req *ConversationBatchRequest, resp *ConversationBatchResponse, touched map[convSrcKey]struct{}) {
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
}

// recordWriter is the write surface ingestOne needs — satisfied by both the
// plain Repository (autocommit) and a BatchRecordWriter (one tx per batch).
type recordWriter interface {
	SeqOwner(ctx context.Context, orgID, sourceID string, seq int64) (string, bool, error)
	InsertRecord(ctx context.Context, e *ConversationRecord, quarantined bool) (bool, error)
	UpsertSession(ctx context.Context, orgID, sessionID, ownerAccountID, systemText string) error
}

func (s *Service) ingestOne(ctx context.Context, e *ConversationRecord, w recordWriter) RecordResult {
	if err := validate(e); err != nil {
		slog.Warn("conversation record rejected", "event_id", e.EventID, "reason", err)
		return RecordResult{EventID: e.EventID, Status: "rejected", Reason: err.Error()}
	}

	// Seq-conflict (r3 #2): if (org, source, seq) is already owned by a DIFFERENT
	// event_id, this incoming record is pollution → quarantine (store-but-flag).
	// A retransmit of the SAME event keeps the same owner → not a conflict.
	quarantined := false
	if e.SourceID != "" && e.SourceSeq != nil {
		owner, found, err := w.SeqOwner(ctx, e.OrgID, e.SourceID, *e.SourceSeq)
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

	inserted, err := w.InsertRecord(ctx, e, quarantined)
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
		if err := w.UpsertSession(ctx, e.OrgID, e.SessionID, sessionScope, e.SystemText); err != nil {
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
