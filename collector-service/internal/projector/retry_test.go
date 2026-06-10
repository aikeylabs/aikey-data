package projector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// TestMarkRetry_SetsRetryStateAndErrorFields covers MarkRetry, the retry
// counterpart of MarkDeadLetter. Both share the `WHERE ods_id = ? AND
// event_time = ?` shape; MarkDeadLetter once shipped a parameter-order
// regression where the UPDATE matched zero rows and the projector stalled
// (bugfix 20260522). MarkRetry was untested — this asserts the UPDATE
// actually lands on the row (status flips to 'retry', count + error fields
// persist), guarding the same regression class on the retry path.
func TestMarkRetry_SetsRetryStateAndErrorFields(t *testing.T) {
	db := newSQLiteODSTestDB(t)
	eventTime := insertODSTestRow(t, db, 77, "pending", aikeytime.Millis(0))

	reader := NewSQLODSReader(db)
	if err := reader.MarkRetry(context.Background(), 77, eventTime, 3, "ENRICH_FAILED", "upstream 5xx"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}

	var (
		status   string
		retry    int
		errCode  sql.NullString
		errMsg   sql.NullString
		nextAt   sql.NullInt64
	)
	if err := db.DB.QueryRow(
		`SELECT dwd_status, dwd_retry_count, dwd_last_error_code, dwd_last_error_msg, dwd_next_retry_at
		 FROM usage_event_ods WHERE ods_id = ?`, 77,
	).Scan(&status, &retry, &errCode, &errMsg, &nextAt); err != nil {
		t.Fatalf("post-check select: %v", err)
	}

	if status != "retry" {
		t.Errorf("dwd_status: want 'retry', got %q — MarkRetry UPDATE missed the row (parameter-order regression?)", status)
	}
	if retry != 3 {
		t.Errorf("dwd_retry_count: want 3, got %d", retry)
	}
	if !errCode.Valid || errCode.String != "ENRICH_FAILED" {
		t.Errorf("dwd_last_error_code: want 'ENRICH_FAILED', got %#v", errCode)
	}
	if !errMsg.Valid || errMsg.String != "upstream 5xx" {
		t.Errorf("dwd_last_error_msg: want 'upstream 5xx', got %#v", errMsg)
	}
	if !nextAt.Valid || nextAt.Int64 == 0 {
		t.Errorf("dwd_next_retry_at: want a scheduled time, got %#v", nextAt)
	}
}

// TestRetryDelay_Tiers pins the backoff schedule: a wrong tier boundary would
// either hammer upstream (too short) or stall recovery (too long).
func TestRetryDelay_Tiers(t *testing.T) {
	cases := []struct {
		retryCount int
		want       time.Duration
	}{
		{1, 1 * time.Minute},
		{3, 1 * time.Minute},   // upper edge of tier 1
		{4, 10 * time.Minute},  // lower edge of tier 2
		{10, 10 * time.Minute}, // upper edge of tier 2
		{11, 1 * time.Hour},    // lower edge of tier 3
		{99, 1 * time.Hour},
	}
	for _, c := range cases {
		if got := retryDelay(c.retryCount); got != c.want {
			t.Errorf("retryDelay(%d) = %v, want %v", c.retryCount, got, c.want)
		}
	}
}
