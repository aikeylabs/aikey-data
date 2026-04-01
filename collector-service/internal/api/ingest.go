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

	resp, results := h.svc.IngestBatch(r.Context(), &req)

	// Log rejected events for observability
	for _, er := range results {
		if er.Status == "rejected" {
			slog.Warn("event rejected in batch", "event_id", er.EventID, "reason", er.Reason)
		}
	}

	shared.JSON(w, http.StatusOK, resp)
}
