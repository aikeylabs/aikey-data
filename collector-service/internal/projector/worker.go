package projector

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/integrity"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/quota"
	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
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
//
// DEPLOYMENT CONSTRAINT — single instance only (2026-06-10 perf review,
// user decision: document, don't lock). The pending/retry scan has no
// cross-instance coordination (no FOR UPDATE SKIP LOCKED / advisory lock /
// lease), so running the projector in two collector replicas means both
// claim the same ODS rows. The DWD insert itself is conflict-guarded
// (ON CONFLICT DO NOTHING on the dedup key) so no duplicate fact rows,
// but side effects race: unpriced_models event_count double-bumps
// (ON CONFLICT DO UPDATE) and the scan checkpoint can regress. If
// horizontal collector scaling is ever committed, add SKIP LOCKED row
// claims here first — until then, deploy exactly one projector-enabled
// collector (see the Production install SOP).
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

	// batchDB, when set (SetBatchDB at the edition entrypoints), enables the
	// P0-4 single-transaction scan path: each scan batch's DWD inserts, ODS
	// marks and the checkpoint commit as ONE transaction (one WAL fsync per
	// scan instead of 2+ per event). nil → classic per-event autocommit
	// (mock-backed tests, or any wiring that predates the batch path).
	batchDB *shared.DB
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

// SetBatchDB enables the single-transaction scan path (P0-4). Pass the same
// *shared.DB the SQL reader/writer/checkpoint were built on.
func (w *Worker) SetBatchDB(db *shared.DB) {
	w.batchDB = db
}

// BacklogSnapshot is the projection backlog read for /metrics (P0-4: the
// backlog was previously invisible — health-signal-surface requires it be
// externally readable, because a lagging projector means the audit trail,
// usage dashboards AND quota enforcement are all stale while every HTTP
// ingest still returns 200).
type BacklogSnapshot struct {
	// Available is false when no batch DB is wired (mock-backed tests) — the
	// gauges are then meaningless zeros, not a healthy-empty backlog.
	Available bool `json:"backlog_available"`
	// Pending counts un-projected ODS rows (dwd_status pending or retry —
	// served by the idx_ods_dwd_unprojected partial index).
	Pending int64 `json:"backlog_pending"`
	// OldestPendingAgeMS is the age of the oldest un-projected row (its
	// ingest_received_at to now) — the projection lag ceiling. 0 when empty.
	OldestPendingAgeMS int64 `json:"backlog_oldest_pending_age_ms"`
}

// Backlog measures the current projection backlog. Consumption rate and
// catch-up time are derivable by any poller from projector_events_projected_total
// deltas plus these gauges, so they are deliberately not materialized here.
func (w *Worker) Backlog(ctx context.Context) BacklogSnapshot {
	if w.batchDB == nil {
		return BacklogSnapshot{}
	}
	var snap BacklogSnapshot
	q := fmt.Sprintf(
		`SELECT COUNT(*), COALESCE(MAX(%s), 0) FROM usage_event_ods WHERE dwd_status IN ('pending','retry')`,
		w.batchDB.AgeMillis("ingest_received_at"))
	if err := w.batchDB.QueryRowContext(ctx, q).Scan(&snap.Pending, &snap.OldestPendingAgeMS); err != nil {
		slog.Warn("projector backlog query failed",
			"event.name", "projector.backlog.query_failed", "error", err)
		return BacklogSnapshot{}
	}
	snap.Available = true
	if snap.OldestPendingAgeMS < 0 {
		snap.OldestPendingAgeMS = 0
	}
	return snap
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
	w.drainOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("projector worker stopping")
			return
		case <-ticker.C:
			w.drainOnce(ctx)
		case <-gapC:
			w.detectGaps(ctx)
		}
	}
}

// drainMaxRoundsPerTick bounds how many back-to-back scans one tick may run.
// Why a cap (not unbounded): a row that stays 'pending' after a failed scan
// (its retry-mark ALSO failed — e.g. the DB is erroring) would otherwise be
// re-fetched in a hot loop. The cap turns that pathology into bounded work per
// tick while still allowing 400×batchSize events of catch-up per tick — far
// above the measured per-scan throughput, so it never binds in healthy
// operation.
const drainMaxRoundsPerTick = 400

// drainOnce scans until the backlog is below one full batch (or the round cap
// / ctx stops it). P0-4 second finding: the 5s ticker × batchSize=100 imposed
// a hard 20 events/sec projection ceiling REGARDLESS of per-scan cost — the
// 2026-08-19 elevated capacity ladder measured DWD falling behind at exactly
// that rate while ingest ran ~300 events/sec. Batching (one tx per scan) made
// scans cheap; this loop makes throughput scale with scan cost instead of
// ticker cadence: a full fetch means more backlog is waiting, so scan again
// immediately.
func (w *Worker) drainOnce(ctx context.Context) {
	for i := 0; i < drainMaxRoundsPerTick; i++ {
		n := w.scanOnce(ctx)
		if n < w.batchSize {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	slog.Warn("projector drain hit per-tick round cap with backlog remaining",
		"event.name", "projector.drain.round_cap",
		"rounds", drainMaxRoundsPerTick, "batch_size", w.batchSize)
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

// scanOnce runs one scan and returns the number of records fetched (the
// drain loop keeps scanning while this equals batchSize — a full fetch means
// more backlog is waiting).
func (w *Worker) scanOnce(ctx context.Context) int {
	w.metrics.ScanCount.Add(1)
	records, err := w.odsReader.FetchPending(ctx, w.batchSize)
	if err != nil {
		slog.Error("projector fetch pending", "error", err)
		return 0
	}
	if len(records) == 0 {
		return 0
	}

	slog.Debug("projector batch", "count", len(records))

	// P0-4 batch rewrite: project the whole scan batch inside ONE transaction
	// (one commit + WAL fsync per scan instead of 2+ per event). If the tx
	// attempt fails at the SQL level it is rolled back (nothing durable) and
	// the batch replays on the classic per-event path below.
	if w.batchDB != nil && w.projectBatchTx(ctx, records) {
		return len(records)
	}
	w.projectPerEvent(ctx, records)
	return len(records)
}

// projectPerEvent is the classic per-event autocommit path — the pre-batch
// implementation, kept verbatim as the fallback (and the only path when no
// batch DB is wired, e.g. mock-backed tests).
func (w *Worker) projectPerEvent(ctx context.Context, records []ODSRecord) {
	var lastOdsID int64
	for i := range records {
		rec := &records[i]
		out, err := w.projectOneOn(ctx, rec, w.odsReader, w.dwdWriter, nil)
		w.bumpOutcome(out, err)
		if err != nil {
			slog.Error("projector project one", "ods_id", rec.OdsID, "event_id", rec.EventID, "error", err)
			// Error already handled inside projectOneOn (retry/dead_letter)
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

// projectBatchTx runs one scan batch inside a single transaction. Returns
// false when the attempt was rolled back (caller replays per-event). Quota
// materialization is debounced per (seat, billing period, UTC day) and runs
// AFTER commit — RecomputeFromDWD is an absolute materialized view (SUM over
// committed DWD), so collapsing per-fact calls into one per key yields the
// identical final counter (fenced by TestPipeline_QuotaCounterMatchesDWDSum).
func (w *Worker) projectBatchTx(ctx context.Context, records []ODSRecord) bool {
	tx, err := w.batchDB.BeginTx(ctx)
	if err != nil {
		slog.Warn("projector batch tx begin failed; falling back to per-event",
			"event.name", "projector.batch_tx.begin_failed", "error", err)
		return false
	}
	txReader := &sqlODSReader{db: w.batchDB, ex: tx}
	txWriter := &sqlDWDWriter{db: w.batchDB, ex: tx}

	var facts []*DWDFact
	var sink func(*DWDFact)
	if w.quotaMat != nil {
		sink = func(f *DWDFact) { facts = append(facts, f) }
	}

	type pendingBump struct {
		out projectOutcome
		err error
	}
	bumps := make([]pendingBump, 0, len(records))
	var lastOdsID int64
	for i := range records {
		rec := &records[i]
		out, perr := w.projectOneOn(ctx, rec, txReader, txWriter, sink)
		if perr != nil {
			// Any SQL failure inside the tx (insert/mark) poisons a PostgreSQL
			// transaction — abort the attempt; the per-event replay gives the
			// failing record its individual retry/dead-letter handling.
			_ = tx.Rollback()
			slog.Warn("projector batch tx failed; replaying per-event",
				"event.name", "projector.batch_tx.replay",
				"ods_id", rec.OdsID, "event_id", rec.EventID, "error", perr)
			return false
		}
		bumps = append(bumps, pendingBump{out, perr})
		if rec.OdsID > lastOdsID {
			lastOdsID = rec.OdsID
		}
	}
	if lastOdsID > 0 {
		txCheckpoint := &sqlCheckpointStore{db: w.batchDB, ex: tx}
		if err := txCheckpoint.UpdateCheckpoint(ctx, defaultTaskName, lastOdsID); err != nil {
			_ = tx.Rollback()
			slog.Warn("projector batch tx checkpoint failed; replaying per-event",
				"event.name", "projector.batch_tx.checkpoint_failed", "error", err)
			return false
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		slog.Warn("projector batch tx commit failed; replaying per-event",
			"event.name", "projector.batch_tx.commit_failed", "error", err)
		return false
	}

	// Metrics count only committed outcomes (a rolled-back attempt is recounted
	// by its replay, never double-counted).
	for _, b := range bumps {
		w.bumpOutcome(b.out, b.err)
	}

	// Post-commit quota debounce: one OnFact per (seat, billingPeriod, UTC day)
	// with summed deltas. The key preserves every periodKey the per-fact calls
	// would have produced (daily keys off the UTC date, weekly off its ISO week,
	// monthly off billingPeriod), and the summed deltas preserve the >0
	// recompute-skip guard (a sum of non-negatives is >0 iff any part was).
	if w.quotaMat != nil && len(facts) > 0 {
		type qKey struct{ seat, period, day string }
		type qAcc struct {
			tokens, usd float64
			eventTime   time.Time
		}
		agg := make(map[qKey]*qAcc)
		for _, f := range facts {
			et := f.EventTime.Time()
			k := qKey{seat: f.SeatID, period: f.BillingPeriod, day: et.UTC().Format("2006-01-02")}
			a := agg[k]
			if a == nil {
				a = &qAcc{eventTime: et}
				agg[k] = a
			}
			a.tokens += float64(f.InputTokens + f.OutputTokens + f.CachedInputTokens +
				f.CacheCreationInputTokens + f.ReasoningTokens)
			a.usd += billableFloat(f.BillableAmount)
		}
		for k, a := range agg {
			w.quotaMat.OnFact(ctx, k.seat, a.tokens, a.usd, k.period, a.eventTime)
		}
	}
	return true
}

// projectOutcome classifies one record's terminal state for metrics counting,
// which is deferred to the caller so a rolled-back batch attempt never counts.
type projectOutcome int

const (
	outcomeProjected projectOutcome = iota
	outcomeRetried
	outcomeDeadLetter
)

// bumpOutcome applies the pre-batch metric semantics: Projected only counts a
// clean success (a failed MarkProjected left the row pending — it will be
// re-scanned); Retried/DeadLetter count the classification itself.
func (w *Worker) bumpOutcome(out projectOutcome, err error) {
	switch out {
	case outcomeProjected:
		if err == nil {
			w.metrics.Projected.Add(1)
		}
	case outcomeRetried:
		w.metrics.Retried.Add(1)
	case outcomeDeadLetter:
		w.metrics.DeadLetter.Add(1)
	}
}

// projectOne keeps the pre-batch signature (used directly by unit tests): one
// record through the worker's default reader/writer, metrics bumped inline.
func (w *Worker) projectOne(ctx context.Context, rec *ODSRecord) error {
	out, err := w.projectOneOn(ctx, rec, w.odsReader, w.dwdWriter, nil)
	w.bumpOutcome(out, err)
	return err
}

// projectOneOn projects one record through the given reader/writer (autocommit
// or tx-bound). factSink, when non-nil, receives each genuinely-new fact
// INSTEAD of the inline quota OnFact — the batch path uses it to debounce
// quota recomputes until after commit.
func (w *Worker) projectOneOn(ctx context.Context, rec *ODSRecord, odsW ODSReader, dwdW DWDWriter, factSink func(*DWDFact)) (projectOutcome, error) {
	// Canary short-circuit: ack (MarkProjected) without enriching or writing
	// to usage_fact_dwd. Canaries are liveness probes, not business data —
	// inserting them would pollute /user/overview stats. Diagnostics queries
	// ODS.dwd_status='projected' for the canary DWD watermark, so acking here
	// is what advances that watermark and keeps watermark_health healthy.
	if rec.VirtualKeyID.Valid && rec.VirtualKeyID.String == canaryVirtualKeyID {
		if err := odsW.MarkProjected(ctx, rec.OdsID, rec.EventTime); err != nil {
			slog.Error("canary mark projected failed", "ods_id", rec.OdsID, "error", err)
			return outcomeProjected, err
		}
		return outcomeProjected, nil
	}

	fact, err := w.enricher.Enrich(ctx, rec)
	if err != nil {
		return w.handleErrorOn(ctx, rec, odsW, "ENRICH_FAILED", err.Error())
	}

	inserted, err := dwdW.Insert(ctx, fact)
	if err != nil {
		return w.handleErrorOn(ctx, rec, odsW, "DWD_INSERT_FAILED", err.Error())
	}

	if !inserted {
		// Duplicate — already projected, just mark as projected
		slog.Debug("dwd duplicate, marking projected", "event_id", rec.EventID)
	}

	// Phase 2 Stage 5: materialize quota_counter + record threshold crossings,
	// ONLY for genuinely-new facts (inserted) so re-projection never double-counts
	// (tied to the DWD insert's org_id+event_id idempotency). Best-effort +
	// panic-guarded inside — never blocks projection. In batch-tx mode factSink
	// collects the fact instead; the debounced OnFact runs after commit.
	if inserted {
		if factSink != nil {
			factSink(fact)
		} else if w.quotaMat != nil {
			tokenDelta := float64(fact.InputTokens + fact.OutputTokens + fact.CachedInputTokens +
				fact.CacheCreationInputTokens + fact.ReasoningTokens)
			w.quotaMat.OnFact(ctx, fact.SeatID, tokenDelta, billableFloat(fact.BillableAmount),
				fact.BillingPeriod, fact.EventTime.Time())
		}
	}

	if err := odsW.MarkProjected(ctx, rec.OdsID, rec.EventTime); err != nil {
		slog.Error("mark projected failed", "ods_id", rec.OdsID, "error", err)
		return outcomeProjected, err
	}

	return outcomeProjected, nil
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

// handleErrorOn classifies a failed record (retry vs dead-letter) and writes
// the mark through the given reader. Metrics are the caller's job (bumpOutcome)
// so a rolled-back batch attempt never counts.
func (w *Worker) handleErrorOn(ctx context.Context, rec *ODSRecord, odsW ODSReader, errCode, errMsg string) (projectOutcome, error) {
	newRetryCount := rec.DwdRetryCount + 1
	if newRetryCount >= deadLetterThreshold {
		slog.Warn("projector dead letter",
			"ods_id", rec.OdsID, "event_id", rec.EventID, "retry_count", newRetryCount)
		return outcomeDeadLetter, odsW.MarkDeadLetter(ctx, rec.OdsID, rec.EventTime, errCode, errMsg)
	}

	slog.Warn("projector retry",
		"ods_id", rec.OdsID, "event_id", rec.EventID,
		"retry_count", newRetryCount, "error_code", errCode)
	return outcomeRetried, odsW.MarkRetry(ctx, rec.OdsID, rec.EventTime, newRetryCount, errCode, errMsg)
}
