package projector

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/quota"
)

const (
	defaultTaskName     = "default"
	defaultBatchSize    = 100
	defaultScanInterval = 5 * time.Second
	deadLetterThreshold = 20

	// defaultGapScanInterval is how often the delivery-integrity gap scan runs.
	// Far slower than the 5s projection tick: the criteria are age-gated (10m /
	// 24h), so a per-minute scan is plenty to surface a gap shortly after it
	// crosses threshold, while keeping the WARN cadence and DB load low. The
	// real-time view is the /v1/diagnostics/completeness endpoint; this loop is
	// the active log surface (health-signal-surface: a gap must be visible
	// without anyone polling).
	defaultGapScanInterval = 5 * time.Minute

	// canaryVirtualKeyID is the sentinel virtual_key_id the proxy uses for
	// liveness probes. Canary events traverse collector→ODS→projector as a
	// heartbeat signal; the projector acks them (MarkProjected) but must not
	// emit a DWD row to keep business stats clean.
	canaryVirtualKeyID = "__canary__"
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

	// gapScanner is the delivery-integrity completeness scanner (optional).
	// When non-nil, Run starts a second, slower ticker that classifies sources
	// and WARN-logs any with a detected gap. nil → the projector behaves exactly
	// as before (no detection), keeping existing call sites / tests unaffected.
	// Wired via SetGapScanner at both edition entrypoints (cmd/main.go +
	// appkit/appkit.go) so detection rides the projector loop in all 3 editions.
	gapScanner  *integrity.Scanner
	gapInterval time.Duration

	// lossPromoter (optional, stage D1) records stale gaps into the known-loss
	// ledger. When set, detectGaps promotes any Promotable source's unaccounted
	// seqs and re-advances its watermark so contiguous converges. nil → no
	// promotion (detection still WARN-logs). Satisfied by the ingest repository.
	lossPromoter LossPromoter

	// quotaMat (optional, Phase 2 Stage 5) materializes quota_counter + records
	// crossed thresholds as each new fact projects. nil → no quota work (Personal
	// / quota-less). Best-effort + panic-guarded inside, so it can never break
	// projection. Wired via SetQuotaMaterializer at both edition entrypoints.
	quotaMat *quota.Materializer
}

// LossPromoter is the write-side the projector needs to record known losses —
// the consumer-defined minimal interface (the ingest ODSRepository satisfies it).
type LossPromoter interface {
	EnumerateMissingSeqs(ctx context.Context, orgID, sourceID string, lo, hi int64) ([]int64, error)
	RecordKnownLoss(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (contiguous int64, err error)
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

// SetGapScanner enables delivery-integrity gap detection on this worker's loop.
// Call once after NewWorker, before Run. A nil scanner (or never calling this)
// leaves detection off. The interval defaults to defaultGapScanInterval.
func (w *Worker) SetGapScanner(s *integrity.Scanner) {
	w.gapScanner = s
	if w.gapInterval <= 0 {
		w.gapInterval = defaultGapScanInterval
	}
}

// SetLossPromoter enables stage-D1 known-loss promotion on the gap-detection
// tick. Requires a gap scanner too (promotion rides detectGaps). nil → detection
// only WARN-logs, never promotes (keeps existing tests/editions unaffected).
func (w *Worker) SetLossPromoter(p LossPromoter) {
	w.lossPromoter = p
}

// SetQuotaMaterializer enables Phase 2 Stage 5 quota materialization on the
// projection path. nil → no quota work (the default; Personal / quota-less).
func (w *Worker) SetQuotaMaterializer(m *quota.Materializer) {
	w.quotaMat = m
}

// Run starts the projection loop. Blocks until ctx is cancelled. When a gap
// scanner is wired, a second, slower ticker runs completeness detection on the
// same goroutine-pair without any extra wiring at the edition entrypoints.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("projector worker started", "batch_size", w.batchSize, "interval", w.scanInterval)

	ticker := time.NewTicker(w.scanInterval)
	defer ticker.Stop()

	// Gap detection ticker is optional. A stopped ticker (gapScanner == nil)
	// has a nil channel that select never fires on — so the projection path is
	// untouched when detection is disabled.
	var gapC <-chan time.Time
	if w.gapScanner != nil {
		gapTicker := time.NewTicker(w.gapInterval)
		defer gapTicker.Stop()
		gapC = gapTicker.C
		// Run an immediate first detection so a gap that exists at startup
		// surfaces without waiting a full interval.
		w.detectGaps(ctx)
	}

	// Run once immediately on start
	w.scanOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("projector worker stopping")
			return
		case <-ticker.C:
			w.scanOnce(ctx)
		case <-gapC:
			w.detectGaps(ctx)
		}
	}
}

// detectGaps is the periodic ACTIVE surface: scan, WARN-log gaps, promote stale
// ones. The readable pull surface is /v1/diagnostics/completeness; the on-demand
// equivalent is ScanNow (D2 reconcile). Errors are logged, not fatal — detection
// is a bypass concern and must never disrupt projection (fault isolation).
func (w *Worker) detectGaps(ctx context.Context) {
	if _, err := w.scanAndPromote(ctx, true); err != nil {
		slog.Error("delivery integrity scan failed",
			"event.name", "integrity.gap_scan.failed", "error", err)
	}
}

// ScanNow runs one scan + known-loss promotion on demand and returns the
// SETTLED per-source completeness (a second scan after promotion) — the `aikey
// audit reconcile` trigger (D2). It forces what the periodic tick would do, so
// the caller sees the converged state immediately (a just-promoted gap shows up
// as known_loss with contiguous advanced, not as an open gap) instead of waiting
// ≤ gapInterval. Returns an error if no scanner is wired.
func (w *Worker) ScanNow(ctx context.Context) ([]integrity.SourceCompleteness, error) {
	if w.gapScanner == nil {
		return nil, fmt.Errorf("delivery integrity scanner not configured")
	}
	if _, err := w.scanAndPromote(ctx, false); err != nil {
		return nil, err
	}
	// Re-scan so the response reflects the post-promotion settled state.
	return w.gapScanner.Scan(ctx)
}

// scanAndPromote is the shared core of detectGaps (periodic) and ScanNow
// (on-demand): scan, optionally WARN-log gaps, and promote any Promotable
// source's stale gap to the known-loss ledger.
func (w *Worker) scanAndPromote(ctx context.Context, warn bool) ([]integrity.SourceCompleteness, error) {
	findings, err := w.gapScanner.Scan(ctx)
	if err != nil {
		return nil, err
	}
	for i := range findings {
		f := &findings[i]
		if warn && !f.Healthy() {
			slog.Warn("delivery integrity gap detected",
				"event.name", "integrity.gap.detected",
				"status", string(f.Status),
				"org_id", f.OrgID,
				"source_id", f.SourceID,
				"contiguous_seq", f.Contiguous,
				"max_seen_seq", f.MaxSeen,
				"client_allocated_seq", f.ClientAllocated,
				"gap_count", f.GapCount,
				"tail_pending", f.TailPending,
			)
		}
		// Stage D1: promote a gap that has aged past KnownLossTimeout to the
		// known-loss ledger. By now the outbox would have re-delivered any seq the
		// client still had in its WAL, so a survivor is genuinely lost.
		if f.Promotable && w.lossPromoter != nil {
			w.promoteKnownLoss(ctx, f)
		}
	}
	return findings, nil
}

// promoteKnownLoss enumerates a Promotable source's unaccounted seqs and records
// them in the known-loss ledger, then re-advances contiguous past them. Bounded
// by the repo's per-call scan limit (a huge gap promotes across several ticks).
func (w *Worker) promoteKnownLoss(ctx context.Context, f *integrity.SourceCompleteness) {
	hi := f.MaxSeen
	if f.ClientAllocated > hi {
		hi = f.ClientAllocated
	}
	missing, err := w.lossPromoter.EnumerateMissingSeqs(ctx, f.OrgID, f.SourceID, f.Contiguous, hi)
	if err != nil {
		slog.Error("known-loss enumerate failed",
			"event.name", "integrity.known_loss.enumerate_failed",
			"org_id", f.OrgID, "source_id", f.SourceID, "error", err)
		return
	}
	if len(missing) == 0 {
		return
	}
	reason := "stale_" + string(f.Status) // stale_middle_gap | stale_tail_gap
	contiguous, err := w.lossPromoter.RecordKnownLoss(ctx, f.OrgID, f.SourceID, missing, reason)
	if err != nil {
		slog.Error("known-loss record failed",
			"event.name", "integrity.known_loss.record_failed",
			"org_id", f.OrgID, "source_id", f.SourceID, "error", err)
		return
	}
	slog.Warn("delivery integrity: seqs promoted to known-loss ledger",
		"event.name", "integrity.known_loss.promoted",
		"reason", reason,
		"org_id", f.OrgID,
		"source_id", f.SourceID,
		"promoted_count", len(missing),
		"first_seq", missing[0],
		"last_seq", missing[len(missing)-1],
		"contiguous_seq", contiguous,
	)
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
	// Canary short-circuit: ack (MarkProjected) without enriching or writing
	// to usage_fact_dwd. Canaries are liveness probes, not business data —
	// inserting them would pollute /user/overview stats. Diagnostics queries
	// ODS.dwd_status='projected' for the canary DWD watermark, so acking here
	// is what advances that watermark and keeps watermark_health healthy.
	if rec.VirtualKeyID.Valid && rec.VirtualKeyID.String == canaryVirtualKeyID {
		if err := w.odsReader.MarkProjected(ctx, rec.OdsID, rec.EventTime); err != nil {
			slog.Error("canary mark projected failed", "ods_id", rec.OdsID, "error", err)
			return err
		}
		w.metrics.Projected.Add(1)
		return nil
	}

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

	// Phase 2 Stage 5: materialize quota_counter + record threshold crossings,
	// ONLY for genuinely-new facts (inserted) so re-projection never double-counts
	// (tied to the DWD insert's org_id+event_id idempotency). Best-effort +
	// panic-guarded inside — never blocks projection.
	if inserted && w.quotaMat != nil {
		tokenDelta := float64(fact.InputTokens + fact.OutputTokens + fact.CachedInputTokens +
			fact.CacheCreationInputTokens + fact.ReasoningTokens)
		w.quotaMat.OnFact(ctx, fact.SeatID, tokenDelta, billableFloat(fact.BillableAmount),
			fact.BillingPeriod, fact.EventTime.Time())
	}

	if err := w.odsReader.MarkProjected(ctx, rec.OdsID, rec.EventTime); err != nil {
		slog.Error("mark projected failed", "ods_id", rec.OdsID, "error", err)
		return err
	}

	w.metrics.Projected.Add(1)
	return nil
}

// billableFloat parses the DWD fact's billable_amount (a *string decimal, nil
// when unpriced) into a float for $ quota accumulation; unparseable/nil → 0.
func billableFloat(s *string) float64 {
	if s == nil || *s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		return 0
	}
	return v
}

func (w *Worker) handleError(ctx context.Context, rec *ODSRecord, errCode, errMsg string) error {
	newRetryCount := rec.DwdRetryCount + 1
	if newRetryCount >= deadLetterThreshold {
		slog.Warn("projector dead letter",
			"ods_id", rec.OdsID, "event_id", rec.EventID, "retry_count", newRetryCount)
		w.metrics.DeadLetter.Add(1)
		return w.odsReader.MarkDeadLetter(ctx, rec.OdsID, rec.EventTime, errCode, errMsg)
	}

	w.metrics.Retried.Add(1)
	slog.Warn("projector retry",
		"ods_id", rec.OdsID, "event_id", rec.EventID,
		"retry_count", newRetryCount, "error_code", errCode)
	return w.odsReader.MarkRetry(ctx, rec.OdsID, rec.EventTime, newRetryCount, errCode, errMsg)
}
