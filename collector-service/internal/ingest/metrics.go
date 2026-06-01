package ingest

import "sync/atomic"

// Metrics tracks ingest counters.
type Metrics struct {
	Accepted   atomic.Int64
	Duplicated atomic.Int64
	Rejected   atomic.Int64
	// Quarantined: events stored but flagged content_hash-mismatch (stage C) —
	// NOT billed. Conflicts: duplicate event_id re-delivered with a different
	// content_hash (§6.2 corruption/tamper); the stored row is kept, the
	// conflict is counted + logged, never silently overwritten.
	Quarantined atomic.Int64
	Conflicts   atomic.Int64
}

// Snapshot returns current counter values.
type MetricsSnapshot struct {
	Accepted    int64 `json:"ingest_events_accepted_total"`
	Duplicated  int64 `json:"ingest_events_duplicated_total"`
	Rejected    int64 `json:"ingest_events_rejected_total"`
	Quarantined int64 `json:"ingest_events_quarantined_total"`
	Conflicts   int64 `json:"ingest_content_hash_conflicts_total"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Accepted:    m.Accepted.Load(),
		Duplicated:  m.Duplicated.Load(),
		Rejected:    m.Rejected.Load(),
		Quarantined: m.Quarantined.Load(),
		Conflicts:   m.Conflicts.Load(),
	}
}
