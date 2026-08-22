package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// mockODS implements ODSRepository for unit testing.
type mockODS struct {
	inserted    map[string]bool // org/event_id -> inserted
	quarantined map[string]bool // org/event_id -> inserted as quarantined (stage C)
}

func newMockODS() *mockODS {
	return &mockODS{inserted: make(map[string]bool)}
}

func (m *mockODS) InsertEvent(_ context.Context, e *UsageEvent, _ []byte, quarantined bool) (bool, bool, error) {
	key := e.OrgID + "/" + e.EventID
	if m.inserted[key] {
		return false, false, nil // duplicate (mock doesn't track stored hashes → no conflict)
	}
	m.inserted[key] = true
	if quarantined {
		if m.quarantined == nil {
			m.quarantined = map[string]bool{}
		}
		m.quarantined[key] = true
	}
	return true, false, nil
}

// AdvanceWatermark — mock returns 0 (the service layer only calls this for
// events carrying a v2 source_id/source_seq; the existing service tests use
// v1-shaped events so it's never reached, but the method is needed to satisfy
// the ODSRepository interface). Watermark-advance correctness is covered by the
// SQL repository tests on a real schema, not here.
func (m *mockODS) AdvanceWatermark(_ context.Context, _, _ string, _ int64) (int64, error) {
	return 0, nil
}

func (m *mockODS) EnumerateMissingSeqs(_ context.Context, _, _ string, _, _ int64) ([]int64, error) {
	return nil, nil
}

func (m *mockODS) RecordKnownLoss(_ context.Context, _, _ string, _ []int64, _ string) (int64, error) {
	return 0, nil
}

// ApplyStreamFloor: the mock does not model watermarks, and no test here
// exercises the stream switch — it is covered against the REAL schema in
// internal/api (stream_switch_test.go), where the idempotency guard actually
// runs SQL. A mock that pretended to implement it would fence nothing.
func (m *mockODS) ApplyStreamFloor(_ context.Context, _, _ string, floor int64) (int64, bool, error) {
	return floor, true, nil
}

func (m *mockODS) GapSeqs(_ context.Context, _, _ string, _ int64) ([]int64, bool, error) {
	return nil, false, nil
}

func (m *mockODS) ConfirmLost(_ context.Context, _, _ string, _ []int64, _ string) (int, int64, error) {
	return 0, 0, nil
}

func TestIngestBatch_HappyPath(t *testing.T) {
	svc := NewService(newMockODS())
	now := aikeytime.FromTime(time.Now())
	req := &BatchRequest{
		Source:        "aikey-proxy",
		SourceVersion: "0.1.0",
		Events: []UsageEvent{
			{EventID: "e1", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"},
			{EventID: "e2", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"},
		},
	}
	resp, results := svc.IngestBatch(context.Background(), req)
	if resp.Accepted != 2 {
		t.Errorf("expected 2 accepted, got %d", resp.Accepted)
	}
	if resp.Duplicated != 0 || resp.Rejected != 0 {
		t.Errorf("unexpected duplicated=%d rejected=%d", resp.Duplicated, resp.Rejected)
	}
	for _, r := range results {
		if r.Status != "accepted" {
			t.Errorf("event %s: expected accepted, got %s", r.EventID, r.Status)
		}
	}
}

func TestIngestBatch_Duplicate(t *testing.T) {
	svc := NewService(newMockODS())
	now := aikeytime.FromTime(time.Now())
	e := UsageEvent{EventID: "e1", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"}

	req := &BatchRequest{Events: []UsageEvent{e}}
	svc.IngestBatch(context.Background(), req)

	// Send same event again
	resp, _ := svc.IngestBatch(context.Background(), req)
	if resp.Duplicated != 1 {
		t.Errorf("expected 1 duplicated, got %d", resp.Duplicated)
	}
}

func TestIngestBatch_Validation(t *testing.T) {
	svc := NewService(newMockODS())

	tests := []struct {
		name  string
		event UsageEvent
	}{
		{"missing event_id", UsageEvent{OrgID: "org1", EventTime: aikeytime.Now(), OccurredAt: aikeytime.Now(), RequestStatus: "ok"}},
		{"missing org_id", UsageEvent{EventID: "e1", EventTime: aikeytime.Now(), OccurredAt: aikeytime.Now(), RequestStatus: "ok"}},
		{"missing event_time", UsageEvent{EventID: "e1", OrgID: "org1", OccurredAt: aikeytime.Now(), RequestStatus: "ok"}},
		{"missing occurred_at", UsageEvent{EventID: "e1", OrgID: "org1", EventTime: aikeytime.Now(), RequestStatus: "ok"}},
		{"missing request_status", UsageEvent{EventID: "e1", OrgID: "org1", EventTime: aikeytime.Now(), OccurredAt: aikeytime.Now()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &BatchRequest{Events: []UsageEvent{tt.event}}
			resp, _ := svc.IngestBatch(context.Background(), req)
			if resp.Rejected != 1 {
				t.Errorf("expected 1 rejected, got %d", resp.Rejected)
			}
		})
	}
}

func TestIngestBatch_MixedResults(t *testing.T) {
	svc := NewService(newMockODS())
	now := aikeytime.FromTime(time.Now())
	req := &BatchRequest{
		Events: []UsageEvent{
			{EventID: "e1", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"},
			{EventID: "", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"}, // invalid
			{EventID: "e3", OrgID: "org1", EventTime: now, OccurredAt: now, RequestStatus: "success"},
		},
	}
	resp, _ := svc.IngestBatch(context.Background(), req)
	if resp.Accepted != 2 {
		t.Errorf("expected 2 accepted, got %d", resp.Accepted)
	}
	if resp.Rejected != 1 {
		t.Errorf("expected 1 rejected, got %d", resp.Rejected)
	}
}
