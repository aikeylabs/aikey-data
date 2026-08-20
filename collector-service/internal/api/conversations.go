package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/conversation"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// ConversationHandler handles batch conversation-record ingestion for the
// enterprise Conversation Audit feature.
type ConversationHandler struct {
	svc *conversation.Service
}

// NewConversationHandler creates a conversation ingest HTTP handler.
func NewConversationHandler(svc *conversation.Service) *ConversationHandler {
	return &ConversationHandler{svc: svc}
}

// HandleBatch processes POST /v1/conversation-records:batch.
//
// Unlike usage ingest, this handler does NOT force-overwrite any per-record
// identity from the JWT. owner_account_id is the SEAT (proxy-verified
// attribution); overwriting it with the bearer/JWT subject would collapse every
// employee's records onto one principal and break per-seat aggregation (review
// #3). org_id is likewise trusted from the proxy — the org-scoped team JWT that
// shared.IngestAuth already verified is what gates who may post here, exactly as
// usage trusts its event org_id. Hard "owner ∈ org" validation needs org_seats
// (a control-plane table the collector doesn't own); the enforced isolation is
// read-side org scoping (every audit query is WHERE org_id=?, design §6.4).
func (h *ConversationHandler) HandleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		shared.Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}

	var req conversation.ConversationBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_JSON", "cannot parse request body: "+err.Error())
		return
	}
	if len(req.Records) == 0 {
		shared.Error(w, http.StatusBadRequest, "EMPTY_BATCH", "records array is empty")
		return
	}
	if len(req.Records) > conversation.MaxBatchSize {
		shared.Error(w, http.StatusBadRequest, "BATCH_TOO_LARGE", "max batch size is 500")
		return
	}

	resp, results := h.svc.IngestBatch(r.Context(), &req)
	for _, rr := range results {
		if rr.Status == "rejected" {
			slog.Warn("conversation record rejected in batch",
				"event.name", "conversation.record.rejected",
				"event_id", rr.EventID, "reason", rr.Reason)
		}
	}
	// P0-4 A-fix: same wire classification as usage ingest — a transient
	// storage failure makes the whole batch retryable (503); a 200 would let
	// the proxy's content-WAL cursor advance past the record and lose the turn.
	if conversation.HasTransientFailure(results) {
		shared.Error(w, http.StatusServiceUnavailable, "INGEST_TRANSIENT_FAILURE",
			"transient storage failure while persisting the batch; retry the batch (idempotent)")
		return
	}
	shared.JSON(w, http.StatusOK, resp)
}
