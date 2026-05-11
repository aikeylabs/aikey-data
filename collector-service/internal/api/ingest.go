// Package api provides HTTP handlers for the collector service.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// IngestHandler handles batch usage event ingestion.
type IngestHandler struct {
	svc *ingest.Service
}

// NewIngestHandler creates an ingest HTTP handler.
func NewIngestHandler(svc *ingest.Service) *IngestHandler {
	return &IngestHandler{svc: svc}
}

const maxBatchSize = 500

// HandleBatch processes POST /v1/usage-events:batch
func (h *IngestHandler) HandleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		shared.Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}

	var req ingest.BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		shared.Error(w, http.StatusBadRequest, "INVALID_JSON", "cannot parse request body: "+err.Error())
		return
	}

	if len(req.Events) == 0 {
		shared.Error(w, http.StatusBadRequest, "EMPTY_BATCH", "events array is empty")
		return
	}
	if len(req.Events) > maxBatchSize {
		shared.Error(w, http.StatusBadRequest, "BATCH_TOO_LARGE", "max batch size is 500")
		return
	}

	// 2026-05-11 B-phase: when ingest is authenticated via user JWT
	// (shared.IngestAuth puts the verified account_id on context),
	// force-overwrite every event's AccountID with the JWT-attested
	// value. Any client-claim mismatch is logged as a WARN — useful
	// signal for "proxy code thinks it's account X but bearer says Y"
	// inconsistencies, and a tripwire for malicious clients that try to
	// forge cross-account events. service_token path skips this (admin
	// trust mode; client claim is honoured).
	//
	// Why force-overwrite even on match: keeps the post-middleware
	// contract simple ("downstream sees JWT account_id, period"). No
	// branch in handlers between "matched, leave alone" vs "mismatched,
	// overwrite".
	if subject, ok := shared.GetIngestSubject(r.Context()); ok && subject.Source == "jwt" {
		for i := range req.Events {
			ev := &req.Events[i]
			if ev.AccountID != "" && ev.AccountID != subject.AccountID {
				slog.Warn("ingest: client-claimed account_id overridden by JWT",
					"event.name", "ingest.account_id.overridden_by_jwt",
					"event_id", ev.EventID,
					"claimed_account_id", ev.AccountID,
					"jwt_account_id", subject.AccountID,
				)
			}
			ev.AccountID = subject.AccountID
		}
	}

	resp, results := h.svc.IngestBatch(r.Context(), &req)

	// Log rejected events for observability
	for _, er := range results {
		if er.Status == "rejected" {
			slog.Warn("event rejected in batch", "event_id", er.EventID, "reason", er.Reason)
		}
	}

	shared.JSON(w, http.StatusOK, resp)
}
