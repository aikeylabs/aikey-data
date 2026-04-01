package projector

import (
	"context"
	"log/slog"
	"time"
)

const (
	defaultTaskName     = "default"
	defaultBatchSize    = 100
	defaultScanInterval = 5 * time.Second
	deadLetterThreshold = 20
)

// Worker runs the ODS → DWD projection loop in the background.
type Worker struct {
	odsReader  ODSReader
	dwdWriter  DWDWriter
	checkpoint CheckpointStore
	enricher   *Enricher
	metrics    WorkerMetrics

	batchSize    int
	scanInterval time.Duration
}

// MetricsSnapshot returns current projector counters.
func (w *Worker) MetricsSnapshot() WorkerMetricsSnapshot {
	return w.metrics.Snapshot()
}

// NewWorker creates a projector worker.
func NewWorker(
	odsReader ODSReader,
	dwdWriter DWDWriter,
	checkpoint CheckpointStore,
	enricher *Enricher,
) *Worker {
	return &Worker{
		odsReader:    odsReader,
		dwdWriter:    dwdWriter,
		checkpoint:   checkpoint,
		enricher:     enricher,
		batchSize:    defaultBatchSize,
		scanInterval: defaultScanInterval,
	}
}

// Run starts the projection loop. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("projector worker started", "batch_size", w.batchSize, "interval", w.scanInterval)

	ticker := time.NewTicker(w.scanInterval)
	defer ticker.Stop()

	// Run once immediately on start
	w.scanOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("projector worker stopping")
			return
		case <-ticker.C:
			w.scanOnce(ctx)
		}
	}
}

func (w *Worker) scanOnce(ctx context.Context) {
	w.metrics.ScanCount.Add(1)
	records, err := w.odsReader.FetchPending(ctx, w.batchSize)
	if err != nil {
		slog.Error("projector fetch pending", "error", err)
		return
	}
	if len(records) == 0 {
		return
	}

	slog.Debug("projector batch", "count", len(records))

	var lastOdsID int64
	for i := range records {
		rec := &records[i]
		if err := w.projectOne(ctx, rec); err != nil {
			slog.Error("projector project one", "ods_id", rec.OdsID, "event_id", rec.EventID, "error", err)
			// Error already handled inside projectOne (retry/dead_letter)
		}
		if rec.OdsID > lastOdsID {
			lastOdsID = rec.OdsID
		}
	}

	// Update checkpoint
	if lastOdsID > 0 {
		if err := w.checkpoint.UpdateCheckpoint(ctx, defaultTaskName, lastOdsID); err != nil {
			slog.Error("projector update checkpoint", "error", err)
		}
	}
}

func (w *Worker) projectOne(ctx context.Context, rec *ODSRecord) error {
	fact, err := w.enricher.Enrich(ctx, rec)
	if err != nil {
		return w.handleError(ctx, rec, "ENRICH_FAILED", err.Error())
	}

	inserted, err := w.dwdWriter.Insert(ctx, fact)
	if err != nil {
		return w.handleError(ctx, rec, "DWD_INSERT_FAILED", err.Error())
	}

	if !inserted {
		// Duplicate — already projected, just mark as projected
		slog.Debug("dwd duplicate, marking projected", "event_id", rec.EventID)
	}

	if err := w.odsReader.MarkProjected(ctx, rec.OdsID); err != nil {
		slog.Error("mark projected failed", "ods_id", rec.OdsID, "error", err)
		return err
	}

	w.metrics.Projected.Add(1)
	return nil
}

func (w *Worker) handleError(ctx context.Context, rec *ODSRecord, errCode, errMsg string) error {
	newRetryCount := rec.DwdRetryCount + 1
	if newRetryCount >= deadLetterThreshold {
		slog.Warn("projector dead letter",
			"ods_id", rec.OdsID, "event_id", rec.EventID, "retry_count", newRetryCount)
		w.metrics.DeadLetter.Add(1)
		return w.odsReader.MarkDeadLetter(ctx, rec.OdsID, errCode, errMsg)
	}

	w.metrics.Retried.Add(1)
	slog.Warn("projector retry",
		"ods_id", rec.OdsID, "event_id", rec.EventID,
		"retry_count", newRetryCount, "error_code", errCode)
	return w.odsReader.MarkRetry(ctx, rec.OdsID, newRetryCount, errCode, errMsg)
}
