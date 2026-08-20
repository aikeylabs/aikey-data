package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate"
	"github.com/AiKeyLabs/aikey-config-tool/pkg/dbmigrate/versions"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/conversation"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/ingest"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
	_ "modernc.org/sqlite"
)

// P0-4 A-fix fences (2026-08-19): the ingest wire contract must distinguish
// "never resend" from "resend later". Before this fix a TRANSIENT insert
// failure (PG contention, timeout, pool exhaustion) surfaced as a per-event
// `rejected` inside an HTTP 200 — and per-event results are NOT on the wire,
// so the proxy advanced its sentSeq past the event and never re-sent it:
// silent, permanent usage loss (ladder forensics 2026-08-19, worker-b middle
// gap). The approved classification:
//
//   - transient infra failure → whole batch 5xx (INGEST_TRANSIENT_FAILURE);
//     the proxy's existing retryable classification re-sends from the WAL,
//     idempotent inserts dedup the already-stored part → exactly-once.
//   - validation failure      → 200 + rejected (genuinely never-resend).
//   - F2 swallowed violation  → 200 + rejected (needs schema fix, resending
//     cannot help; kept LOUD via the F2 error log, never a silent duplicate).

func newTransientTestDB(t *testing.T) *shared.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := versions.UpgradeComponentsTo(context.Background(), raw,
		dbmigrate.DialectSQLite, []dbmigrate.Component{dbmigrate.ComponentData}, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return shared.NewDB(raw, shared.DialectSQLite)
}

func postUsageBatch(t *testing.T, h *IngestHandler, events []ingest.UsageEvent) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"events": events})
	req := httptest.NewRequest(http.MethodPost, "/v1/usage-events:batch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleBatch(w, req)
	return w
}

func usageEvt(org, eventID string, seq int64) ingest.UsageEvent {
	now := aikeytime.Now()
	s := seq
	return ingest.UsageEvent{
		EventID: eventID, OrgID: org, SourceID: "srcT", SourceSeq: &s,
		EventTime: now, OccurredAt: now, RequestStatus: "success", RequestCount: 1,
	}
}

// TestHandleBatch_TransientInsertFailure_Returns503: a hard SQL failure on one
// event must fail the WHOLE batch with 503 so the proxy re-sends it — never a
// 200 that quietly drops the event from the resend set.
func TestHandleBatch_TransientInsertFailure_Returns503(t *testing.T) {
	db := newTransientTestDB(t)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	h := NewIngestHandler(svc)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER transient_503 BEFORE INSERT ON usage_event_ods
		 WHEN NEW.org_id = 'orgT503'
		 BEGIN SELECT RAISE(ABORT, 'simulated transient storage failure'); END;`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	w := postUsageBatch(t, h, []ingest.UsageEvent{
		usageEvt("orgTOK", "t503-e1", 1),
		usageEvt("orgT503", "t503-poisoned", 1),
		usageEvt("orgTOK", "t503-e2", 2),
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 — a transient insert failure inside a 200 is the "+
			"P0-4 silent-loss wire defect (proxy cannot know to re-send)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INGEST_TRANSIENT_FAILURE")) {
		t.Fatalf("body=%s want error code INGEST_TRANSIENT_FAILURE", w.Body.String())
	}
	// The healthy events of the batch may or may not have been stored (they were
	// inserted before the failure) — both are fine: the proxy re-sends the whole
	// batch and idempotent inserts dedup. What must NOT happen is losing them.
}

// TestHandleBatch_ValidationReject_Still200: validation failures are genuinely
// terminal — the batch stays 200 with a per-event rejected count so healthy
// events aren't re-sent forever alongside a permanently-bad one.
func TestHandleBatch_ValidationReject_Still200(t *testing.T) {
	db := newTransientTestDB(t)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	h := NewIngestHandler(svc)

	bad := usageEvt("orgTV", "tv-bad", 2)
	bad.RequestStatus = "" // required-field validation failure

	w := postUsageBatch(t, h, []ingest.UsageEvent{
		usageEvt("orgTV", "tv-ok", 1),
		bad,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (validation rejects must not 503)", w.Code)
	}
	var resp struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || resp.Rejected != 1 {
		t.Fatalf("resp=%+v want accepted=1 rejected=1", resp)
	}
}

// TestHandleBatch_F2Swallow_Still200Rejected: a swallowed constraint violation
// (RAISE(IGNORE) — the schema-drift shape F2 guards) is NOT transient:
// resending cannot help until the schema is fixed. It must stay a per-event
// rejected inside a 200 (loud in logs), not convert to 503 head-of-line
// blocking for the whole source.
func TestHandleBatch_F2Swallow_Still200Rejected(t *testing.T) {
	db := newTransientTestDB(t)
	svc := ingest.NewService(ingest.NewSQLODSRepository(db))
	h := NewIngestHandler(svc)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER f2_swallow_503 BEFORE INSERT ON usage_event_ods
		 WHEN NEW.org_id = 'orgTF2'
		 BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	w := postUsageBatch(t, h, []ingest.UsageEvent{
		usageEvt("orgTOK2", "tf2-e1", 1),
		usageEvt("orgTF2", "tf2-swallowed", 1),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (F2 swallow is not transient — 503 would head-of-line block)", w.Code)
	}
	var resp struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || resp.Rejected != 1 {
		t.Fatalf("resp=%+v want accepted=1 rejected=1", resp)
	}
}

// TestConversationBatch_TransientInsertFailure_Returns503: the conversation
// lane has the same wire defect — same classification fix.
func TestConversationBatch_TransientInsertFailure_Returns503(t *testing.T) {
	db := newTransientTestDB(t)
	svc := conversation.NewService(conversation.NewSQLRepository(db), "")
	h := NewConversationHandler(svc)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER conv_transient_503 BEFORE INSERT ON conversation_records
		 WHEN NEW.org_id = 'orgCT503'
		 BEGIN SELECT RAISE(ABORT, 'simulated transient storage failure'); END;`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	seq := int64(1)
	rec := conversation.ConversationRecord{
		EventID: "ct503-poisoned", OrgID: "orgCT503", SessionID: "sessCT",
		SourceID: "srcCT", SourceSeq: &seq,
		UserText: "q", AssistantText: "a", RequestStatus: "ok", CreatedAt: aikeytime.Now(),
	}
	body, _ := json.Marshal(map[string]any{"records": []conversation.ConversationRecord{rec}})
	req := httptest.NewRequest(http.MethodPost, "/v1/conversation-records:batch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleBatch(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 (conversation transient insert failure must be retryable on the wire)", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INGEST_TRANSIENT_FAILURE")) {
		t.Fatalf("body=%s want INGEST_TRANSIENT_FAILURE", w.Body.String())
	}
}
