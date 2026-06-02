package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
	"github.com/AiKeyLabs/aikey-data/query-service/internal/usage"
)

// adminRepo is the narrow slice of usage.Repository the admin endpoints
// need (interface segregation — keeps these handlers testable with a tiny
// mock instead of the full Repository surface). usage.Repository is a
// superset, so the real sqlRepo satisfies it.
type adminRepo interface {
	ListUnpricedModels(ctx context.Context, status string) ([]usage.UnpricedModel, error)
	UpdateUnpricedModelStatus(ctx context.Context, provider, model, status string) error
	GetEventAudit(ctx context.Context, eventID string) (*usage.EventAudit, error)
}

// AdminHandler serves the cost-pricing admin endpoints (Stage 3): the
// pending-pricing queue (unpriced_models) and the per-event audit trail.
//
// Mounted under /v1/admin/* behind the SAME service-token auth as the
// user endpoints. query-service has no user-role concept — it is an
// internal service the control backend calls. The "is this caller an
// admin?" check is enforced one layer up in aikey-control (which gates
// the /admin web routes by role, Stage 6); query-service only verifies
// the service token. See router.go and the Stage 3 design §4.2.3.
type AdminHandler struct {
	repo adminRepo
}

// NewAdminHandler wires an AdminHandler to the shared usage repository
// (same DB connection as the user handlers).
func NewAdminHandler(repo usage.Repository) *AdminHandler {
	return &AdminHandler{repo: repo}
}

// validUnpricedStatus is the closed set a maintainer may assign — guarded
// so the status-update endpoint can never write an arbitrary string.
var validUnpricedStatus = map[string]bool{
	"pending":      true,
	"acknowledged": true,
	"fixed":        true,
}

// ListUnpricedModels handles GET /v1/admin/unpriced-models?status=<filter>.
// status is optional; "" or "all" returns every row.
func (h *AdminHandler) ListUnpricedModels(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "all" && !validUnpricedStatus[status] {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "status must be pending|acknowledged|fixed|all")
		return
	}
	rows, err := h.repo.ListUnpricedModels(r.Context(), status)
	if err != nil {
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	if rows == nil {
		rows = []usage.UnpricedModel{}
	}
	shared.JSON(w, http.StatusOK, map[string]any{"models": rows})
}

// UpdateUnpricedModelStatus handles
// POST /v1/admin/unpriced-models/{provider}/{model}?status=<new>.
//
// provider + model are the composite PK (path params, URL-safe because
// model names carry no slashes for the direct providers Phase 1 sees).
func (h *AdminHandler) UpdateUnpricedModelStatus(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	model := r.PathValue("model")
	status := r.URL.Query().Get("status")
	if provider == "" || model == "" {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "provider and model are required")
		return
	}
	if !validUnpricedStatus[status] {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "status must be pending|acknowledged|fixed")
		return
	}
	err := h.repo.UpdateUnpricedModelStatus(r.Context(), provider, model, status)
	if errors.Is(err, usage.ErrNotFound) {
		shared.Error(w, http.StatusNotFound, "NOT_FOUND", "no such unpriced model")
		return
	}
	if err != nil {
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	shared.JSON(w, http.StatusOK, map[string]any{"provider": provider, "model": model, "status": status})
}

// GetEventAudit handles GET /v1/admin/events/{event_id}/audit — the full
// cost-audit trail for one event (DWD row + JOINed pricing snapshot).
func (h *AdminHandler) GetEventAudit(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("event_id")
	if eventID == "" {
		shared.Error(w, http.StatusBadRequest, "INVALID_PARAMS", "event_id is required")
		return
	}
	audit, err := h.repo.GetEventAudit(r.Context(), eventID)
	if errors.Is(err, usage.ErrNotFound) {
		shared.Error(w, http.StatusNotFound, "NOT_FOUND", "no such event")
		return
	}
	if err != nil {
		shared.Error(w, http.StatusInternalServerError, "QUERY_FAILED", "internal error")
		return
	}
	shared.JSON(w, http.StatusOK, audit)
}
