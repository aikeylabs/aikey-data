package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Service handles usage event ingestion logic.
type Service struct {
	repo    ODSRepository
	metrics Metrics
}

// NewService creates an ingest service.
func NewService(repo ODSRepository) *Service {
	return &Service{repo: repo}
}

// MetricsSnapshot returns current ingest counters.
func (s *Service) MetricsSnapshot() MetricsSnapshot {
	return s.metrics.Snapshot()
}

// IngestBatch processes a batch of usage events.
// Each event is validated and inserted independently; a single bad event
// does not block the rest of the batch.
func (s *Service) IngestBatch(ctx context.Context, req *BatchRequest) (*BatchResponse, []EventResult) {
	resp := &BatchResponse{}
	results := make([]EventResult, 0, len(req.Events))

	for i := range req.Events {
		e := &req.Events[i]
		r := s.ingestOne(ctx, e)
		results = append(results, r)
		switch r.Status {
		case "accepted":
			resp.Accepted++
			s.metrics.Accepted.Add(1)
		case "duplicated":
			resp.Duplicated++
			s.metrics.Duplicated.Add(1)
		case "rejected":
			resp.Rejected++
			s.metrics.Rejected.Add(1)
		}
	}
	return resp, results
}

func (s *Service) ingestOne(ctx context.Context, e *UsageEvent) EventResult {
	if err := validate(e); err != nil {
		slog.Warn("event rejected", "event_id", e.EventID, "reason", err)
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: err.Error()}
	}

	// Default schema version
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	// Warn on unknown schema version but still ingest (forward-compatible).
	// A newer proxy may send v2 events before collector is upgraded.
	// Missing new fields will be zero-valued, which is acceptable.
	if e.SchemaVersion > MaxSchemaVersion {
		slog.Warn("ingest: unknown schema version, ingesting anyway",
			"event_id", e.EventID,
			"got", e.SchemaVersion,
			"max_supported", MaxSchemaVersion)
	}
	// Default request count
	if e.RequestCount == 0 {
		e.RequestCount = 1
	}

	rawJSON, err := json.Marshal(e)
	if err != nil {
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: "marshal raw event: " + err.Error()}
	}

	inserted, err := s.repo.InsertEvent(ctx, e, rawJSON)
	if err != nil {
		slog.Error("insert event failed", "event_id", e.EventID, "error", err)
		return EventResult{EventID: e.EventID, Status: "rejected", Reason: "internal error"}
	}
	if !inserted {
		return EventResult{EventID: e.EventID, Status: "duplicated"}
	}
	return EventResult{EventID: e.EventID, Status: "accepted"}
}

func validate(e *UsageEvent) error {
	if e.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if e.OrgID == "" {
		return fmt.Errorf("org_id is required")
	}
	if e.EventTime.IsZero() {
		return fmt.Errorf("event_time is required")
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	if e.RequestStatus == "" {
		return fmt.Errorf("request_status is required")
	}
	return nil
}
