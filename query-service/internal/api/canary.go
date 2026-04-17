package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

// queryDB is the minimal surface the canary-check handler needs. Kept local
// to this file so we don't widen the public handler contract for a liveness
// endpoint.
type queryDB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CanaryStageResponse is the JSON payload of GET /internal/canary-check on
// the query-service. QueryReadable is true iff the query-service can read
// the canary row from usage_event_ods AND the projector has already acked
// it (dwd_status='projected'). This gives the proxy's Canary probe a signal
// that the query-side of the pipeline is live.
type CanaryStageResponse struct {
	LastEventID   string `json:"last_event_id"`
	QueryReadable bool   `json:"query_readable"`
}

// HandleQueryCanaryCheck serves /internal/canary-check for the query-service.
//
// Why a dedicated handler here (instead of reusing collector-service's
// HandleCanaryCheck): in production the query-service has its own DB
// connection pool and container. Hitting its endpoint verifies that the
// query-side is actually reachable and can see the projector's ack, which
// is a different liveness signal than "collector can see its own writes".
//
// Why read usage_event_ods (not usage_fact_dwd): canary rows are never
// inserted into usage_fact_dwd (the projector excludes them from DWD writes
// to keep business stats clean — see aikey-data/collector-service projector
// worker). The only on-DB evidence of a canary's full traversal is
// ODS.dwd_status='projected'.
func HandleQueryCanaryCheck(db queryDB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eventID := r.URL.Query().Get("event_id")
		if eventID == "" || !strings.HasPrefix(eventID, "canary-") {
			http.Error(w, `{"error":"invalid canary event_id"}`, http.StatusBadRequest)
			return
		}

		resp := CanaryStageResponse{LastEventID: eventID}
		var dwdStatus sql.NullString
		err := db.QueryRowContext(r.Context(),
			"SELECT dwd_status FROM usage_event_ods WHERE event_id = ? LIMIT 1", eventID,
		).Scan(&dwdStatus)
		resp.QueryReadable = (err == nil && dwdStatus.Valid && dwdStatus.String == "projected")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
