package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Materializer accumulates per-seat usage into quota_counter and records crossed
// thresholds, driven by the DWD projector (one call per newly-projected fact —
// idempotent because the caller only invokes it when the DWD insert was genuinely
// new). The enabled quota_subject config is cached with a short TTL so we don't
// re-read it per fact. All work is best-effort: callers ignore errors so the
// usage pipeline is never blocked by quota.
type Materializer struct {
	store    *Storage
	ttl      time.Duration
	notifier AlertNotifier // optional; nil = no email alerts on crossing

	mu        sync.Mutex
	seatIndex map[string][]*Subject // seat_id → applicable subjects (own seat + its groups)
	loadedAt  time.Time
	loaded    bool
}

// SetNotifier attaches an optional crossing-alert notifier (email via control).
// Call before serving; nil-safe (no notifier ⇒ crossings are recorded but not
// emailed). Kept a setter so NewMaterializer's signature stays unchanged.
func (m *Materializer) SetNotifier(n AlertNotifier) { m.notifier = n }

// NewMaterializer builds the materializer. ttl<=0 defaults to 30s.
func NewMaterializer(store *Storage, ttl time.Duration) *Materializer {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Materializer{store: store, ttl: ttl, seatIndex: map[string][]*Subject{}}
}

// ensureConfig (re)loads the enabled subjects when the cache is stale. On load
// error it keeps the last-known config (logs a WARN) — staleness is far better
// than dropping accumulation. now is injected so the caller's clock is the source
// of truth (testability).
func (m *Materializer) ensureConfig(ctx context.Context, now time.Time) {
	m.mu.Lock()
	fresh := m.loaded && now.Sub(m.loadedAt) < m.ttl
	m.mu.Unlock()
	if fresh {
		return
	}
	subs, err := m.store.LoadEnabledSubjects(ctx)
	if err != nil {
		slog.Warn("quota.config.load_failed", "error", err.Error())
		return
	}
	index := map[string][]*Subject{}
	for i := range subs {
		sub := &subs[i]
		switch sub.SubjectKind {
		case KindSeat:
			index[sub.SubjectID] = append(index[sub.SubjectID], sub)
		case KindGroup:
			for _, seatID := range sub.Members {
				index[seatID] = append(index[seatID], sub)
			}
		}
	}
	m.mu.Lock()
	m.seatIndex = index
	m.loadedAt = now
	m.loaded = true
	m.mu.Unlock()
}

func (m *Materializer) applicable(seatID string) []*Subject {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seatIndex[seatID]
}

// OnFact accumulates one projected fact's usage into every applicable subject's
// counter and records newly-crossed thresholds. tokenDelta is the raw token sum
// (input+output+cached+creation+reasoning); billable is the $ amount (0 when
// unpriced). billingPeriod is the DWD 'YYYY-MM' month; eventTime dates daily
// windows. No-op when the seat has no quota.
func (m *Materializer) OnFact(ctx context.Context, seatID string, tokenDelta, billable float64, billingPeriod string, eventTime time.Time) {
	if m == nil || seatID == "" {
		return
	}
	// Hard fault isolation for the projection hot path: a bug here must never
	// crash the projector goroutine.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("quota.materialize.panic", "seat_id", seatID, "panic", r)
		}
	}()
	now := time.Now()
	m.ensureConfig(ctx, now)
	subs := m.applicable(seatID)
	if len(subs) == 0 {
		return
	}
	for _, sub := range subs {
		m.applyToSubject(ctx, sub, tokenDelta, billable, billingPeriod, eventTime)
	}
}

type bucketKey struct{ metric, period string }

func (m *Materializer) applyToSubject(ctx context.Context, sub *Subject, tokenDelta, billable float64, billingPeriod string, eventTime time.Time) {
	seatIDs := subjectSeatIDs(sub)
	if len(seatIDs) == 0 {
		return
	}
	// 1) Recompute BOTH metrics (tokens + usd) for every distinct period the
	//    subject's rules reference — "dual-value" (D-U2): quota_counter always
	//    carries token AND usd usage regardless of which metric the rule limits
	//    on, so the admin can switch a rule's metric with the counter already
	//    populated and the overview can show both dimensions. Threshold
	//    enforcement (step 2) still only applies to metrics that have a rule.
	type acc struct {
		used      float64
		triggered []int
		pk        string
	}
	results := map[bucketKey]acc{}
	for _, period := range distinctPeriods(sub.Rules) {
		pk := periodKey(period, billingPeriod, eventTime)
		for _, metric := range allMetrics {
			// Cheap guard: only recompute a metric this fact actually moved (e.g. an
			// unpriced fact, billable=0, can't change the usd total). Recompute reads
			// the authoritative SUM(DWD) and is idempotent, so skipping only avoids a
			// redundant SUM — the row keeps its last correct value.
			if deltaFor(metric, tokenDelta, billable) <= 0 {
				continue
			}
			used, triggered, err := m.store.RecomputeFromDWD(ctx, sub.SubjectID, metric, pk, seatIDs)
			if err != nil {
				slog.Warn("quota.counter.recompute_failed", "subject_id", sub.SubjectID, "metric", metric, "error", err.Error())
				continue
			}
			results[bucketKey{metric, period}] = acc{used: used, triggered: triggered, pk: pk}
		}
	}
	// 2) Per bucket, check ALL its rules' thresholds against the accumulated used,
	//    record newly crossed (dedup via triggered_thresholds).
	hardBlockCrossed := false
	for k, res := range results {
		triggered := res.triggered
		changed := false
		for _, r := range sub.Rules {
			if r.Metric != k.metric || r.Period != k.period || r.LimitAmount <= 0 {
				continue
			}
			for _, th := range r.Thresholds {
				if res.used >= r.LimitAmount*float64(th.Pct)/100 && !containsInt(triggered, th.Pct) {
					triggered = append(triggered, th.Pct)
					changed = true
					if th.Action == ActionHardBlock {
						hardBlockCrossed = true
					}
					slog.Info("quota.threshold.crossed",
						"subject_id", sub.SubjectID, "metric", k.metric, "period_key", res.pk,
						"pct", th.Pct, "action", th.Action, "used", res.used, "limit", r.LimitAmount)
					// Email alert (architecture B): only on the NEW crossing (we're
					// inside the !containsInt dedup), only when the admin enabled it.
					if th.Notify && m.notifier != nil {
						m.notifier.NotifyCrossing(CrossingAlert{
							SubjectID:   sub.SubjectID,
							SubjectKind: sub.SubjectKind,
							Metric:      k.metric,
							Period:      k.period,
							Pct:         th.Pct,
							Used:        res.used,
							Limit:       r.LimitAmount,
							NotifyEmail: th.NotifyEmail,
						})
					}
				}
			}
		}
		if changed {
			if err := m.store.MarkTriggered(ctx, sub.SubjectID, k.metric, res.pk, triggered); err != nil {
				slog.Warn("quota.threshold.mark_failed", "subject_id", sub.SubjectID, "error", err.Error())
			}
		}
	}
	// 3) P5 push (timeliness): a newly-crossed hard_block bumps the subject's seats'
	//    sync_version so the CLI re-pulls + the proxy learns the over-limit baseline
	//    promptly, instead of waiting for the periodic backstop pull. Best-effort —
	//    post-D-U8 this only sharpens cross-machine propagation (P7 already enforces
	//    the local machine in real time). Once per applyToSubject (deduped across
	//    metrics/periods via the single flag).
	if hardBlockCrossed {
		m.store.BumpSyncVersionForSeats(ctx, seatIDs)
	}
}

// allMetrics is the fixed set materialized per period (D-U2 dual-value), so
// quota_counter always carries both token and usd usage independent of the
// rule's metric.
var allMetrics = []string{MetricTokens, MetricUSD}

// distinctPeriods returns the unique periods the subject's rules reference (e.g.
// ["monthly"] or ["daily","monthly"]). Both metrics are materialized for each.
func distinctPeriods(rules []Rule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if r.Period == "" || seen[r.Period] {
			continue
		}
		seen[r.Period] = true
		out = append(out, r.Period)
	}
	return out
}

// subjectSeatIDs is the set of seats whose DWD usage rolls up to this subject:
// a seat-subject is just itself; a group-subject is all its members (shared pool).
func subjectSeatIDs(sub *Subject) []string {
	switch sub.SubjectKind {
	case KindSeat:
		return []string{sub.SubjectID}
	case KindGroup:
		return sub.Members
	default:
		return nil
	}
}

func deltaFor(metric string, tokenDelta, billable float64) float64 {
	switch metric {
	case MetricTokens:
		return tokenDelta
	case MetricUSD:
		return billable
	default:
		return 0
	}
}

// periodKey mirrors the proxy's quota.PeriodKey so counters/baselines align.
func periodKey(period, billingPeriod string, eventTime time.Time) string {
	switch period {
	case "daily":
		return "daily:" + eventTime.UTC().Format("2006-01-02")
	case "weekly":
		// ISO week (Mon-Sun); lockstep with proxy.PeriodKey + dwdPeriodFilter.
		y, w := eventTime.UTC().ISOWeek()
		return fmt.Sprintf("weekly:%04d-W%02d", y, w)
	case "monthly":
		m := billingPeriod
		if m == "" {
			m = eventTime.UTC().Format("2006-01")
		}
		return "monthly:" + m
	default:
		return period + ":all"
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
