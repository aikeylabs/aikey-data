package projector

import "sync/atomic"

// WorkerMetrics tracks projector counters.
type WorkerMetrics struct {
	Projected  atomic.Int64
	Retried    atomic.Int64
	DeadLetter atomic.Int64
	ScanCount  atomic.Int64
}

type WorkerMetricsSnapshot struct {
	Projected  int64 `json:"projector_events_projected_total"`
	Retried    int64 `json:"projector_events_retry_total"`
	DeadLetter int64 `json:"projector_events_dead_letter_total"`
	ScanCount  int64 `json:"projector_scan_count_total"`
}

func (m *WorkerMetrics) Snapshot() WorkerMetricsSnapshot {
	return WorkerMetricsSnapshot{
		Projected:  m.Projected.Load(),
		Retried:    m.Retried.Load(),
		DeadLetter: m.DeadLetter.Load(),
		ScanCount:  m.ScanCount.Load(),
	}
}
