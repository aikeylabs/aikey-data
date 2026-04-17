package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"
)

func setupCanaryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE usage_event_ods (
		event_id TEXT, org_id TEXT, dwd_status TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestHandleQueryCanaryCheck_Projected verifies the query-stage canary
// returns query_readable=true when the projector has acked the event.
func TestHandleQueryCanaryCheck_Projected(t *testing.T) {
	db := setupCanaryTestDB(t)
	_, _ = db.Exec(`INSERT INTO usage_event_ods VALUES (?, ?, ?)`,
		"canary-123", "__canary__", "projected")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/internal/canary-check?event_id=canary-123", nil)
	HandleQueryCanaryCheck(db).ServeHTTP(rr, req)

	var got CanaryStageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.QueryReadable {
		t.Errorf("expected QueryReadable=true for projected canary, got %+v", got)
	}
}

// TestHandleQueryCanaryCheck_Pending verifies pending canaries are not
// reported as readable (projector hasn't acked them yet).
func TestHandleQueryCanaryCheck_Pending(t *testing.T) {
	db := setupCanaryTestDB(t)
	_, _ = db.Exec(`INSERT INTO usage_event_ods VALUES (?, ?, ?)`,
		"canary-456", "__canary__", "pending")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/internal/canary-check?event_id=canary-456", nil)
	HandleQueryCanaryCheck(db).ServeHTTP(rr, req)

	var got CanaryStageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.QueryReadable {
		t.Errorf("expected QueryReadable=false for pending canary, got %+v", got)
	}
}

// TestHandleQueryCanaryCheck_BadRequest verifies malformed event_id is rejected.
func TestHandleQueryCanaryCheck_BadRequest(t *testing.T) {
	db := setupCanaryTestDB(t)
	for _, eid := range []string{"", "not-canary-prefix", "canary"} {
		url := "/internal/canary-check?event_id=" + eid
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		HandleQueryCanaryCheck(db).ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("event_id=%q: expected 400, got %d", eid, rr.Code)
		}
	}
}

// TestHandleQueryCanaryCheck_DBUnreachable ensures DB errors map to readable=false
// rather than bubbling up as 500 — the proxy's canary probe expects a 200 payload
// to distinguish "projector acked" from "projector did not ack".
func TestHandleQueryCanaryCheck_DBUnreachable(t *testing.T) {
	// Closed DB triggers a QueryRow error.
	db, _ := sql.Open("sqlite", ":memory:")
	_ = db.Close()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/internal/canary-check?event_id=canary-x", nil).WithContext(context.Background())
	HandleQueryCanaryCheck(db).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	var got CanaryStageResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.QueryReadable {
		t.Errorf("expected QueryReadable=false on DB error, got %+v", got)
	}
}
