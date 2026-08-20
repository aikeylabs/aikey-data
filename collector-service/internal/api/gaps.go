package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// gapsRepo is the narrow delivery-integrity slice the D3 reconciliation
// endpoints need (the ingest ODSRepository satisfies it). Kept minimal so the
// handlers don't depend on the full repository surface.
type gapsRepo interface {
	GapSeqs(ctx context.Context, orgID, sourceID string, limit int64) ([]int64, bool, error)
	ConfirmLost(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (promoted int, contiguous int64, err error)
}

type gapsResponse struct {
	OrgID       string  `json:"org_id"`
	SourceID    string  `json:"source_id"`
	MissingSeqs []int64 `json:"missing_seqs"`
	Truncated   bool    `json:"truncated"`
}

// handleGaps serves GET /v1/diagnostics/gaps?org_id=&source_id=&limit= — the
// unaccounted seqs for one source, so a client (proxy) can check them against
// its WAL and re-send or confirm-lost (stage D3). Read-only.
func handleGaps(repo gapsRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org := r.URL.Query().Get("org_id")
		src := r.URL.Query().Get("source_id")
		if org == "" || src == "" {
			shared.JSON(w, http.StatusBadRequest, map[string]string{"error": "org_id and source_id are required"})
			return
		}
		var limit int64
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, _ = strconv.ParseInt(v, 10, 64)
		}
		seqs, truncated, err := repo.GapSeqs(r.Context(), org, src, limit)
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if seqs == nil {
			seqs = []int64{} // serialize as [] not null
		}
		shared.JSON(w, http.StatusOK, gapsResponse{OrgID: org, SourceID: src, MissingSeqs: seqs, Truncated: truncated})
	}
}

type confirmLostRequest struct {
	OrgID    string  `json:"org_id"`
	SourceID string  `json:"source_id"`
	Seqs     []int64 `json:"seqs"`
}

type confirmLostResponse struct {
	Promoted   int   `json:"promoted"`       // how many were actually ledgered (genuinely absent)
	Contiguous int64 `json:"contiguous_seq"` // new high-water after promotion
}

// handleConfirmLost serves POST /v1/diagnostics/confirm-lost — the client
// declares these seqs unrecoverable (not in its WAL); the server ledgers the
// GENUINELY-absent ones immediately (reason=client_confirmed) instead of waiting
// for the KnownLossTimeout (stage D3). State-changing but guarded: a seq that's
// actually in ODS/ledger is never marked lost.
func handleConfirmLost(repo gapsRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req confirmLostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			shared.JSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		if req.OrgID == "" || req.SourceID == "" {
			shared.JSON(w, http.StatusBadRequest, map[string]string{"error": "org_id and source_id are required"})
			return
		}
		promoted, contiguous, err := repo.ConfirmLost(r.Context(), req.OrgID, req.SourceID, req.Seqs, "client_confirmed")
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// 🔴 Ledgering is PERMANENT destruction of billable events, and this is
		// the second of the two paths that do it. The projector's own
		// timeout-promotion has warned since it was written
		// (integrity.known_loss.promoted); this client-driven path shipped
		// silent, so a client could write off a source's whole history and
		// leave no trace on the server at all — which is exactly what happened
		// on 2026-08-20: 768 seqs ledgered, not one line in the server log to
		// find them by. Same event shape as the projector's WARN on purpose, so
		// one query over event.name finds every loss regardless of who declared
		// it. See workflow/CI/bugfix/20260820-usage-delivery-org-partitioned-seq-stream.md
		if promoted > 0 {
			slog.Warn("delivery integrity: seqs ledgered as known-loss on CLIENT declaration",
				"event.name", "integrity.known_loss.promoted",
				"reason", "client_confirmed",
				"org_id", req.OrgID,
				"source_id", req.SourceID,
				"promoted_count", promoted,
				"requested_count", len(req.Seqs),
				"contiguous_seq", contiguous)
		}
		shared.JSON(w, http.StatusOK, confirmLostResponse{Promoted: promoted, Contiguous: contiguous})
	}
}
