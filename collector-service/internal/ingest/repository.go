package ingest

import "context"

// ODSRepository persists raw usage events to the ODS layer.
type ODSRepository interface {
	// InsertEvent inserts a single event. Returns true if inserted, false if duplicate.
	InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte) (inserted bool, err error)
}
