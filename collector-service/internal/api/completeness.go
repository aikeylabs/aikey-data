package api

import (
	"net/http"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/projector"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// completenessResponse is the GET /v1/diagnostics/completeness body: the
// per-source delivery-completeness view plus a single rollup the release E2E
// (and any external monitor) can assert on without parsing every source.
//
// healthy = "no source has a detected gap AND no source has a quarantined
// (content_hash-failed) event" — the exact ops assertion gap_count=0 且
// quarantine_count=0. This is the externally readable health signal the
// financial-grade audit requires (health-signal-surface): it must be assertable
// from an endpoint, not just visible in logs or inferred from a UI dashboard.
type completenessResponse struct {
	Sources         []integrity.SourceCompleteness `json:"sources"`
	GapSources      int                            `json:"gap_sources"`
	QuarantineTotal int64                          `json:"quarantine_total"`
	Healthy         bool                           `json:"healthy"`
}

// handleCompleteness serves the live per-source completeness classification.
// Read-only and unauthenticated, consistent with the sibling diagnostics
// endpoints (pipeline / canary-check) — it returns delivery-integrity metadata
// (org_id, source_id, seq high-water marks), not usage amounts or credentials,
// and is meant to sit behind the same network boundary as those endpoints.
func handleCompleteness(scanner *integrity.Scanner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		findings, err := scanner.Scan(r.Context())
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		shared.JSON(w, http.StatusOK, buildCompletenessResponse(findings))
	}
}

// handleReconcile serves POST /v1/diagnostics/reconcile (the `aikey audit
// reconcile` trigger, D2): force an immediate scan + known-loss promotion, then
// return the SETTLED completeness so the caller sees the converged verdict
// ("对平 / 仍缺 N / 已知丢失 M") without waiting for the periodic projector tick.
// Same read-only-data shape as /completeness; the only side effect is promoting
// already-stale (≥ KnownLossTimeout) gaps to the audit ledger — idempotent and
// equivalent to what the tick does on its own.
func handleReconcile(projWorker *projector.Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		findings, err := projWorker.ScanNow(r.Context())
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		shared.JSON(w, http.StatusOK, buildCompletenessResponse(findings))
	}
}

// buildCompletenessResponse rolls up per-source findings into the wire response
// shared by /completeness and /reconcile. healthy excludes known_loss (a source
// "complete with documented losses" is healthy — only open gaps + quarantine
// make it unhealthy, matching the ops assertion gap_count=0 且 quarantine_count=0).
func buildCompletenessResponse(findings []integrity.SourceCompleteness) completenessResponse {
	gaps := 0
	var quarantine int64
	for i := range findings {
		if findings[i].Status != integrity.StatusOK {
			gaps++
		}
		quarantine += findings[i].QuarantineCount
	}
	return completenessResponse{
		Sources:         findings,
		GapSources:      gaps,
		QuarantineTotal: quarantine,
		Healthy:         gaps == 0 && quarantine == 0,
	}
}
