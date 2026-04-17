// Package diagnostics serves pipeline-observability endpoints for the
// collector-service: watermark-based health probes and a per-event canary
// check. These endpoints belong to collector-service because this service
// owns usage_event_ods and the projector that maintains dwd_status; hosting
// them here lets the same diagnostics reach every edition (trial via the
// embedded collector handler, production via the standalone collector-service
// container).
package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DBQuerier abstracts database query access so callers can inject either
// *sql.DB (production PostgreSQL, trial SQLite) or a test double.
type DBQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Response is the JSON payload returned by GET /v1/diagnostics/pipeline.
type Response struct {
	BusinessWatermarks Watermarks   `json:"business_watermarks"`
	CanaryWatermarks   Watermarks   `json:"canary_watermarks"`
	CanaryProbe        *CanaryProbe `json:"canary_probe,omitempty"`
	Lag                LagInfo      `json:"lag"`
	// WatermarkHealth reflects pipeline health based on watermark freshness only.
	// It does NOT include the actual canary probe result (which lives in proxy /metrics).
	// Consumers should combine this with proxy canary result for a complete picture.
	// Named "watermark_health" (not "health") to avoid confusion with the overall
	// pipeline health that doctor computes from both sources.
	WatermarkHealth string  `json:"watermark_health"`
	StaleStage      *string `json:"stale_stage"`
}

// Watermarks captures the latest ODS/DWD timestamps for a scope.
type Watermarks struct {
	ODSLatestIngestedAt  *time.Time `json:"ods_latest_ingested_at"`
	DWDLatestProjectedAt *time.Time `json:"dwd_latest_projected_at"`
}

// CanaryProbe reports per-event reach for /internal/canary-check.
type CanaryProbe struct {
	LastEventID  string `json:"last_event_id"`
	ODSReceived  bool   `json:"ods_received"`
	DWDProjected bool   `json:"dwd_projected"`
}

// LagInfo reports the ODS→DWD gap for business events.
type LagInfo struct {
	ODSToDWDSeconds *int64 `json:"ods_to_dwd_seconds,omitempty"`
}

// HandlePipeline returns the pipeline watermark diagnostics view.
// Business and canary watermarks are queried separately to avoid canary
// events polluting business freshness indicators.
func HandlePipeline(db DBQuerier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		resp := Response{}

		// Business watermarks (exclude canary)
		resp.BusinessWatermarks = queryBusinessWatermarks(ctx, db)
		// Canary watermarks: DWD side queries ODS.dwd_status='projected'
		// instead of usage_fact_dwd, because the projector acks canaries
		// without writing DWD rows (see projector/worker.go projectOne).
		resp.CanaryWatermarks = queryCanaryWatermarks(ctx, db)

		// Lag calculation
		if resp.BusinessWatermarks.ODSLatestIngestedAt != nil && resp.BusinessWatermarks.DWDLatestProjectedAt != nil {
			lag := int64(resp.BusinessWatermarks.ODSLatestIngestedAt.Sub(*resp.BusinessWatermarks.DWDLatestProjectedAt).Seconds())
			if lag < 0 {
				lag = 0
			}
			resp.Lag.ODSToDWDSeconds = &lag
		}

		// Health determination: use canary watermark freshness as primary signal.
		// If canary events are flowing (recent canary in DWD-ack), the pipeline is healthy.
		// If canary watermark is stale (>15min), mark degraded with stale stage.
		// If no canary data exists yet, fall back to business watermark assessment.
		resp.WatermarkHealth, resp.StaleStage = EvaluateHealth(resp.CanaryWatermarks, resp.BusinessWatermarks)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// queryBusinessWatermarks returns ODS/DWD latest timestamps for non-canary events.
func queryBusinessWatermarks(ctx context.Context, db DBQuerier) Watermarks {
	var wm Watermarks

	if t, ok := scanTime(db.QueryRowContext(ctx,
		"SELECT MAX(ingest_received_at) FROM usage_event_ods WHERE org_id != '__canary__'",
	)); ok {
		wm.ODSLatestIngestedAt = &t
	}

	if t, ok := scanTime(db.QueryRowContext(ctx,
		"SELECT MAX(projected_at) FROM usage_fact_dwd WHERE org_id != '__canary__'",
	)); ok {
		wm.DWDLatestProjectedAt = &t
	}

	return wm
}

// queryCanaryWatermarks returns ODS/DWD latest timestamps for canary events.
//
// DWD side: the projector worker does NOT insert canary rows into
// usage_fact_dwd (it would pollute business stats). Instead it acks canary
// ODS rows by setting dwd_status='projected'. So the canary "DWD watermark"
// is derived from the latest canary ODS row in 'projected' state.
// Using ingest_received_at (not a projector timestamp) is intentional — the
// signal we care about for liveness is "when was the last canary that made
// it all the way through", which is adequately represented by its ingest time.
func queryCanaryWatermarks(ctx context.Context, db DBQuerier) Watermarks {
	var wm Watermarks

	if t, ok := scanTime(db.QueryRowContext(ctx,
		"SELECT MAX(ingest_received_at) FROM usage_event_ods WHERE org_id = '__canary__'",
	)); ok {
		wm.ODSLatestIngestedAt = &t
	}

	if t, ok := scanTime(db.QueryRowContext(ctx,
		"SELECT MAX(ingest_received_at) FROM usage_event_ods WHERE org_id = '__canary__' AND dwd_status = 'projected'",
	)); ok {
		wm.DWDLatestProjectedAt = &t
	}

	return wm
}

// scanTime reads a single nullable timestamp column and parses it.
func scanTime(row *sql.Row) (time.Time, bool) {
	var s sql.NullString
	if err := row.Scan(&s); err != nil || !s.Valid {
		return time.Time{}, false
	}
	return parseTimestamp(s.String)
}

// parseTimestamp handles both SQLite datetime() format ("2026-04-16 08:18:01"),
// PostgreSQL timestamp ("2026-04-16 08:18:01+00"), and RFC3339 variants.
func parseTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05",       // SQLite datetime('now')
		"2006-01-02 15:04:05-07",    // PostgreSQL with tz offset
		"2006-01-02 15:04:05.999999", // PostgreSQL fractional seconds
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05", // no timezone
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// EvaluateHealth determines pipeline health using canary watermark freshness as
// the primary signal. Business traffic freshness is not used for health judgment
// because low-traffic / idle environments would always appear degraded.
//
// Priority:
//  1. Canary DWD watermark (== last acked canary) recent (<15min) → healthy
//  2. Canary ODS present but DWD-ack stale → degraded at "projector"
//  3. Canary ODS stale → degraded at "collector"
//  4. No canary data yet → fall back to "healthy" (canary hasn't run yet)
func EvaluateHealth(canaryWM, _ Watermarks) (string, *string) {
	// If no canary data exists yet, we can't judge — default to healthy.
	if canaryWM.ODSLatestIngestedAt == nil && canaryWM.DWDLatestProjectedAt == nil {
		return "healthy", nil
	}

	now := time.Now().UTC()

	// Check canary DWD freshness (projector stage).
	if canaryWM.DWDLatestProjectedAt != nil {
		age := now.Sub(*canaryWM.DWDLatestProjectedAt)
		if age < 15*time.Minute {
			return "healthy", nil
		}
		// DWD stale but ODS recent → projector problem
		if canaryWM.ODSLatestIngestedAt != nil {
			odsAge := now.Sub(*canaryWM.ODSLatestIngestedAt)
			if odsAge < 15*time.Minute {
				stage := "projector"
				return "degraded", &stage
			}
		}
	}

	// ODS present but DWD absent or stale → check ODS freshness
	if canaryWM.ODSLatestIngestedAt != nil {
		age := now.Sub(*canaryWM.ODSLatestIngestedAt)
		if age < 15*time.Minute {
			// ODS recent but DWD not → projector stalled
			stage := "projector"
			return "degraded", &stage
		}
		// Both stale → collector/reporter problem
		stage := "collector"
		return "degraded", &stage
	}

	return "healthy", nil
}

// HandleCanaryCheck checks whether a specific canary event has reached each
// pipeline stage. Used by the proxy's Canary probe to verify full pipeline
// traversal.
// GET /internal/canary-check?event_id=canary-1713264900
func HandleCanaryCheck(db DBQuerier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eventID := r.URL.Query().Get("event_id")
		if eventID == "" || !strings.HasPrefix(eventID, "canary-") {
			http.Error(w, `{"error":"invalid canary event_id"}`, http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		result := CanaryProbe{LastEventID: eventID}

		// Check ODS: is the canary event in usage_event_ods, and has the
		// projector acked it? The projector does not write canaries to
		// usage_fact_dwd — it advances dwd_status='projected' on the ODS row
		// as the ack. So we read both signals from a single ODS row.
		var dwdStatus sql.NullString
		err := db.QueryRowContext(ctx,
			"SELECT dwd_status FROM usage_event_ods WHERE event_id = ? LIMIT 1", eventID,
		).Scan(&dwdStatus)
		if err == nil {
			result.ODSReceived = true
			result.DWDProjected = (dwdStatus.Valid && dwdStatus.String == "projected")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

