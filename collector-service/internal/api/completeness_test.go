package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// TestCompletenessEndpoint exercises the HTTP wiring end-to-end on a real
// schema: a source with an aged middle gap must make the endpoint report
// healthy=false and surface that source with status=middle_gap. This is the
// externally-readable signal the release E2E asserts on.
func TestCompletenessEndpoint(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := shared.NewDB(raw, shared.DialectSQLite)

	// Seed a source with a hole at seq 4 (contiguous=3, max_seen=5).
	repo := ingest.NewSQLODSRepository(db)
	for _, seq := range []int64{1, 2, 3, 5} {
		s := seq
		now := aikeytime.Now()
		e := &ingest.UsageEvent{
			EventID: "ev" + string(rune('0'+seq)), OrgID: "orgE", SourceID: "srcE", SourceSeq: &s,
			EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
		}
		if _, _, err := repo.InsertEvent(context.Background(), e, []byte("{}"), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.AdvanceWatermark(context.Background(), "orgE", "srcE", 0); err != nil {
		t.Fatal(err)
	}
	// Age the gap past the 10m middle-gap threshold.
	if _, err := db.ExecContext(context.Background(),
		"UPDATE usage_source_watermark SET updated_at = (CAST(strftime('%s','now') AS INTEGER) * 1000 - ?) WHERE source_id = ?",
		(11 * time.Minute).Milliseconds(), "srcE"); err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(db, integrity.DefaultCriteria())
	rec := httptest.NewRecorder()
	handleCompleteness(scanner)(rec, httptest.NewRequest(http.MethodGet, "/v1/diagnostics/completeness", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp completenessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Healthy {
		t.Fatalf("healthy=true, want false (a middle gap exists)")
	}
	if resp.GapSources != 1 {
		t.Fatalf("gap_sources=%d, want 1", resp.GapSources)
	}
	if len(resp.Sources) != 1 || resp.Sources[0].Status != integrity.StatusMiddleGap || resp.Sources[0].GapCount != 1 {
		t.Fatalf("sources=%+v, want one middle_gap with gap_count=1", resp.Sources)
	}
}

// TestCompletenessEndpoint_HealthyWhenEmpty: no sources → healthy=true (the
// E2E baseline before any gap is injected).
func TestCompletenessEndpoint_HealthyWhenEmpty(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := shared.NewDB(raw, shared.DialectSQLite)

	rec := httptest.NewRecorder()
	handleCompleteness(integrity.NewScanner(db, integrity.DefaultCriteria()))(
		rec, httptest.NewRequest(http.MethodGet, "/v1/diagnostics/completeness", nil))

	var resp completenessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Healthy || resp.GapSources != 0 {
		t.Fatalf("empty DB: healthy=%v gap_sources=%d, want true/0", resp.Healthy, resp.GapSources)
	}
}
