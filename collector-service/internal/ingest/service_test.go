package ingest

import (
	"context"
	"testing"
	"time"
)

// mockODS implements ODSRepository for unit testing.
type mockODS struct {
	inserted map[string]bool // event_id -> inserted
}

func newMockODS() *mockODS {
	return &mockODS{inserted: make(map[string]bool)}
}

func (m *mockODS) InsertEvent(_ context.Context, e *UsageEvent, _ []byte) (bool, error) {
	key := e.OrgID + "/" + e.EventID
	if m.inserted[key] {
		return false, nil // duplicate
	}
	m.inserted[key] = true
	return true, nil
}

func TestIngestBatch_HappyPath(t *testing.T) {
	svc := NewService(newMockODS())
	now := time.Now()
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
	now := time.Now()
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
		{"missing event_id", UsageEvent{OrgID: "org1", EventTime: time.Now(), OccurredAt: time.Now(), RequestStatus: "ok"}},
		{"missing org_id", UsageEvent{EventID: "e1", EventTime: time.Now(), OccurredAt: time.Now(), RequestStatus: "ok"}},
		{"missing event_time", UsageEvent{EventID: "e1", OrgID: "org1", OccurredAt: time.Now(), RequestStatus: "ok"}},
		{"missing occurred_at", UsageEvent{EventID: "e1", OrgID: "org1", EventTime: time.Now(), RequestStatus: "ok"}},
		{"missing request_status", UsageEvent{EventID: "e1", OrgID: "org1", EventTime: time.Now(), OccurredAt: time.Now()}},
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
	now := time.Now()
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
