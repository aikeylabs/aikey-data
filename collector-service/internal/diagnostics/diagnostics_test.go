package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// setupTestDB creates an in-memory SQLite DB with the minimal ODS/DWD schema
// used by diagnostics. We keep the schema local to this test to avoid coupling
// to migrations — only the columns diagnostics reads are needed.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE usage_event_ods (
			ods_id INTEGER PRIMARY KEY,
			event_id TEXT,
			org_id TEXT,
			virtual_key_id TEXT,
			ingest_received_at TEXT,
			dwd_status TEXT
		);
		CREATE TABLE usage_fact_dwd (
			event_id TEXT,
			org_id TEXT,
			projected_at TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertODS(t *testing.T, db *sql.DB, eventID, orgID, ingest, status string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO usage_event_ods (event_id, org_id, virtual_key_id, ingest_received_at, dwd_status)
		 VALUES (?, ?, ?, ?, ?)`,
		eventID, orgID, orgID, ingest, status,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func insertDWD(t *testing.T, db *sql.DB, eventID, orgID, projectedAt string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO usage_fact_dwd (event_id, org_id, projected_at) VALUES (?, ?, ?)`,
		eventID, orgID, projectedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestCanaryWatermarks_UsesODSDwdStatus verifies the canary DWD watermark
// comes from usage_event_ods.dwd_status='projected', NOT usage_fact_dwd.
// Regression guard: before 2026-04-17 the projector excluded canaries from
// DWD so this watermark was frozen at the last pre-exclusion entry, producing
// perpetual degraded health.
func TestCanaryWatermarks_UsesODSDwdStatus(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Minute).Format("2006-01-02 15:04:05")
	stale := now.Add(-24 * time.Hour).Format("2006-01-02 15:04:05")

	insertODS(t, db, "canary-1", "__canary__", recent, "projected")
	insertODS(t, db, "canary-2", "__canary__", now.Format("2006-01-02 15:04:05"), "pending")
	// Stale DWD row with org_id='__canary__' — must be ignored now
	insertDWD(t, db, "legacy-canary", "__canary__", stale)

	wm := queryCanaryWatermarks(context.Background(), db)
	if wm.ODSLatestIngestedAt == nil {
		t.Fatal("canary ODS watermark missing")
	}
	if !wm.ODSLatestIngestedAt.After(now.Add(-time.Second)) {
		t.Errorf("canary ODS watermark too old: %v", wm.ODSLatestIngestedAt)
	}
	if wm.DWDLatestProjectedAt == nil {
		t.Fatal("canary DWD watermark missing — should come from ODS dwd_status='projected'")
	}
	age := now.Sub(*wm.DWDLatestProjectedAt)
	if age > 2*time.Minute {
		t.Errorf("canary DWD watermark too old (%v) — still reading stale usage_fact_dwd?", age)
	}
}

// TestCanaryWatermarks_OnlyProjectedAcks verifies that canaries still in
// 'pending' or 'retry' do NOT advance the DWD watermark.
func TestCanaryWatermarks_OnlyProjectedAcks(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().UTC()
	newPending := now.Format("2006-01-02 15:04:05")
	oldProjected := now.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")

	insertODS(t, db, "canary-old", "__canary__", oldProjected, "projected")
	insertODS(t, db, "canary-new", "__canary__", newPending, "pending")

	wm := queryCanaryWatermarks(context.Background(), db)
	if wm.DWDLatestProjectedAt == nil {
		t.Fatal("expected DWD watermark")
	}
	age := now.Sub(*wm.DWDLatestProjectedAt)
	if age < 9*time.Minute || age > 11*time.Minute {
		t.Errorf("DWD watermark should track only 'projected' rows, age=%v", age)
	}
}

// TestBusinessWatermarks_ExcludesCanary verifies business queries ignore canaries.
func TestBusinessWatermarks_ExcludesCanary(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	insertODS(t, db, "biz-1", "org-a", now, "projected")
	insertODS(t, db, "canary-1", "__canary__", now, "projected")
	insertDWD(t, db, "biz-1", "org-a", now)
	insertDWD(t, db, "canary-1", "__canary__", now)

	wm := queryBusinessWatermarks(context.Background(), db)
	if wm.ODSLatestIngestedAt == nil || wm.DWDLatestProjectedAt == nil {
		t.Fatal("business watermarks should be present")
	}

	// Remove the business row, keep only canary — business watermark must become nil
	_, _ = db.Exec(`DELETE FROM usage_event_ods WHERE org_id != '__canary__'`)
	_, _ = db.Exec(`DELETE FROM usage_fact_dwd WHERE org_id != '__canary__'`)
	wm = queryBusinessWatermarks(context.Background(), db)
	if wm.ODSLatestIngestedAt != nil || wm.DWDLatestProjectedAt != nil {
		t.Errorf("business watermarks must be nil when only canary data exists, got ODS=%v DWD=%v",
			wm.ODSLatestIngestedAt, wm.DWDLatestProjectedAt)
	}
}

// TestHandleCanaryCheck_DWDFromODSDwdStatus verifies the /internal/canary-check
// endpoint reads DWD-projected state from usage_event_ods.dwd_status, not from
// usage_fact_dwd (which never contains canary rows).
func TestHandleCanaryCheck_DWDFromODSDwdStatus(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	insertODS(t, db, "canary-acked", "__canary__", now, "projected")
	insertODS(t, db, "canary-pending", "__canary__", now, "pending")

	handler := HandleCanaryCheck(db)

	t.Run("acked canary reports DWD projected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/internal/canary-check?event_id=canary-acked", nil)
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var got CanaryProbe
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.ODSReceived || !got.DWDProjected {
			t.Errorf("expected ODS=true DWD=true, got %+v", got)
		}
	})

	t.Run("pending canary reports DWD not projected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/internal/canary-check?event_id=canary-pending", nil)
		handler.ServeHTTP(rr, req)
		var got CanaryProbe
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.ODSReceived {
			t.Errorf("expected ODSReceived=true, got %+v", got)
		}
		if got.DWDProjected {
			t.Errorf("expected DWDProjected=false (pending), got %+v", got)
		}
	})

	t.Run("missing canary returns 200 with ODS=false", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/internal/canary-check?event_id=canary-missing", nil)
		handler.ServeHTTP(rr, req)
		var got CanaryProbe
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ODSReceived || got.DWDProjected {
			t.Errorf("expected all false for missing event, got %+v", got)
		}
	})
}

// TestEvaluateHealth_CanaryDWDHealthy verifies recent canary ack → healthy.
func TestEvaluateHealth_CanaryDWDHealthy(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Minute)

	canaryWM := Watermarks{
		ODSLatestIngestedAt:  &recent,
		DWDLatestProjectedAt: &recent,
	}
	health, stage := EvaluateHealth(canaryWM, Watermarks{})
	if health != "healthy" || stage != nil {
		t.Errorf("expected healthy,nil got %q,%v", health, stage)
	}
}

// TestEvaluateHealth_DegradedAtProjector_OnlyWhenCanaryStaleButODSFresh
// verifies the projector-stage degradation triggers only when the projector
// actually stops acking canaries while collector keeps ingesting.
func TestEvaluateHealth_DegradedAtProjector_OnlyWhenCanaryStaleButODSFresh(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Minute)
	stale := now.Add(-30 * time.Minute)

	canaryWM := Watermarks{
		ODSLatestIngestedAt:  &recent,
		DWDLatestProjectedAt: &stale,
	}
	health, stage := EvaluateHealth(canaryWM, Watermarks{})
	if health != "degraded" {
		t.Errorf("expected degraded, got %q", health)
	}
	if stage == nil || *stage != "projector" {
		t.Errorf("expected stage=projector, got %v", stage)
	}
}
