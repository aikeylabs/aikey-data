package projector

import (
	"context"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// ODSReader reads pending ODS records for projection.
type ODSReader interface {
	// FetchPending returns ODS records ready for DWD projection, ordered by ods_id.
	FetchPending(ctx context.Context, limit int) ([]ODSRecord, error)

	// MarkProjected marks an ODS record as successfully projected.
	// eventTime is the record's event_time — it prunes the UPDATE to the one
	// monthly partition on PostgreSQL (usage_event_ods is partitioned by
	// event_time, v1.0.1-alpha.4); without it the UPDATE would scan every
	// partition. Harmless on SQLite (not partitioned).
	MarkProjected(ctx context.Context, odsID int64, eventTime aikeytime.Millis) error

	// MarkRetry marks an ODS record for retry with exponential backoff.
	MarkRetry(ctx context.Context, odsID int64, eventTime aikeytime.Millis, retryCount int, errCode, errMsg string) error

	// MarkDeadLetter marks an ODS record as permanently failed.
	MarkDeadLetter(ctx context.Context, odsID int64, eventTime aikeytime.Millis, errCode, errMsg string) error
}

// ControlEventReader reads control events for enrichment (D5: only this table, no joins).
type ControlEventReader interface {
	// FindByVirtualKeyAtTime finds the control event effective at the given time
	// for the specified virtual_key_id.
	// Returns nil if no matching event found.
	FindByVirtualKeyAtTime(ctx context.Context, virtualKeyID string, eventTime aikeytime.Millis) (*ControlEvent, error)
}

// DWDWriter writes enriched facts to the DWD layer.
type DWDWriter interface {
	// Insert writes a DWD fact. Returns true if inserted, false if duplicate.
	Insert(ctx context.Context, fact *DWDFact) (bool, error)
}

// CheckpointStore reads and updates the projector cursor.
type CheckpointStore interface {
	// GetLastScannedOdsID returns the last scanned ods_id for the given task.
	GetLastScannedOdsID(ctx context.Context, taskName string) (int64, error)

	// UpdateCheckpoint updates the projector cursor.
	UpdateCheckpoint(ctx context.Context, taskName string, lastOdsID int64) error
}
