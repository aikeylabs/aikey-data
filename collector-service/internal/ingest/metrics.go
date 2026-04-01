package ingest

import "sync/atomic"

// Metrics tracks ingest counters.
type Metrics struct {
	Accepted   atomic.Int64
	Duplicated atomic.Int64
	Rejected   atomic.Int64
}

// Snapshot returns current counter values.
type MetricsSnapshot struct {
	Accepted   int64 `json:"ingest_events_accepted_total"`
	Duplicated int64 `json:"ingest_events_duplicated_total"`
	Rejected   int64 `json:"ingest_events_rejected_total"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Accepted:   m.Accepted.Load(),
		Duplicated: m.Duplicated.Load(),
		Rejected:   m.Rejected.Load(),
	}
}
