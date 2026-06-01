// Package integrity implements server-side delivery-completeness (gap)
// detection for the financial-grade usage audit (design doc 阶段6-企业定制/
// 20260530-财务对账级用量审计-完整技术方案.md, phase B').
//
// It is a pure READ-SIDE classifier over usage_source_watermark — the small
// per-source projection table the ingest path maintains (contiguous_seq /
// max_seen_seq / client_allocated_seq / last_event_at / updated_at). It never
// touches the ingest or projection WRITE paths, so detection is an isolated,
// stateless concern (CQRS: classify from the projection, don't re-scan the
// events table). The same Scanner backs BOTH surfaces:
//
//   - the projector's periodic WARN log (active surface), and
//   - GET /v1/diagnostics/completeness (pull surface, health-signal-surface).
//
// Three gap criteria, all age-gated so in-flight reordering is NOT a gap:
//
//	middle gap : contiguous_seq < max_seen_seq, stale > MiddleGapAge
//	             — a hole inside the delivered range (a seq below the high-water
//	             mark never arrived). Exact hole count is computed from ODS.
//	tail  gap  : client_allocated_seq > max_seen_seq, stale > TailGapTimeout
//	             — the client allocated seqs the server never even saw (lost
//	             before WAL append, or never uploaded). The (max_seen, allocated]
//	             span is suspected tail loss.
//	silence    : last_event_at older than SilenceTimeout
//	             — a source went quiet; may be benign (idle) or a dead client.
package integrity

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// Status is the single most-severe classification for a source.
type Status string

const (
	StatusOK        Status = "ok"
	StatusMiddleGap Status = "middle_gap"
	StatusTailGap   Status = "tail_gap"
	StatusSilent    Status = "silent"
)

// Criteria holds the age thresholds. Defaults live in code (DefaultCriteria);
// they're injectable so tests can use zero thresholds (flag immediately) and a
// future config layer can override without touching this package.
type Criteria struct {
	// MiddleGapAge: a hole below max_seen must persist at least this long before
	// it's a gap (vs. just-reordered in-flight events). Default 10m.
	MiddleGapAge time.Duration
	// TailGapTimeout: client_allocated > max_seen must persist at least this long
	// before the tail is treated as suspected loss (vs. still uploading). Default 10m.
	TailGapTimeout time.Duration
	// SilenceTimeout: no event for a source for this long → silent. Default 24h.
	SilenceTimeout time.Duration
	// KnownLossTimeout (stage D1): a gap (unaccounted seq above contiguous) that
	// stays stale this long is PROMOTED to the known-loss ledger — by now the
	// outbox would have re-delivered any seq the client still had in its WAL, so
	// a survivor is genuinely lost. Longer than the alarm thresholds (give it a
	// full day to self-heal before declaring loss). Default 24h.
	KnownLossTimeout time.Duration
}

// DefaultCriteria returns the production thresholds.
func DefaultCriteria() Criteria {
	return Criteria{
		MiddleGapAge:     10 * time.Minute,
		TailGapTimeout:   10 * time.Minute,
		SilenceTimeout:   24 * time.Hour,
		KnownLossTimeout: 24 * time.Hour,
	}
}

// SourceCompleteness is the per-source completeness view. It doubles as the
// /v1/diagnostics/completeness DTO (JSON tags), so the wire shape is stable for
// phase D (which fills known_loss_count from the confirmed-lost ledger).
type SourceCompleteness struct {
	OrgID           string `json:"org_id"`
	SourceID        string `json:"source_id"`
	Contiguous      int64  `json:"contiguous_seq"`       // delivered, no-gap high-water
	MaxSeen         int64  `json:"max_seen_seq"`         // highest seq ever stored
	ClientAllocated int64  `json:"client_allocated_seq"` // highest seq the client ever handed out
	GapCount        int64  `json:"gap_count"`            // exact unaccounted seqs in (contiguous, max_seen]; 0 unless middle_gap
	TailPending     int64  `json:"tail_pending"`         // allocated seqs above the accounted high-water (received+lost)
	KnownLoss       int64  `json:"known_loss_count"`     // rows in usage_known_loss_ledger for this source (stage D1)
	QuarantineCount int64  `json:"quarantine_count"`     // events stored-but-not-billed (content_hash mismatch, stage C)
	Status          Status `json:"status"`
	// Ages are pointers so "never set" (NULL column) serialises as absent rather
	// than a misleading 0. StaleSeconds = now - updated_at (how long the source's
	// watermark has been frozen). LastEventAgeSeconds = now - last_event_at.
	StaleSeconds        *int64 `json:"stale_seconds,omitempty"`
	LastEventAgeSeconds *int64 `json:"last_event_age_seconds,omitempty"`

	// Promotable (stage D1, internal — not serialized) flags a source whose gap
	// has aged past KnownLossTimeout: the projector enumerates its unaccounted
	// seqs and records them in the known-loss ledger. Range to enumerate is
	// (Contiguous, max(MaxSeen, ClientAllocated)].
	Promotable bool `json:"-"`
}

// Healthy reports whether the source has no detected gap.
func (s SourceCompleteness) Healthy() bool { return s.Status == StatusOK }

// Scanner classifies completeness from usage_source_watermark.
type Scanner struct {
	db       *shared.DB
	criteria Criteria
}

// NewScanner builds a Scanner. db is the same dialect-aware handle the ingest
// and projector layers use.
func NewScanner(db *shared.DB, c Criteria) *Scanner {
	return &Scanner{db: db, criteria: c}
}

// Scan classifies every source in usage_source_watermark. The watermark read is
// cheap (one row per source). The exact ODS hole-count query runs ONLY for
// sources flagged as a middle gap (rare) — healthy sources never hit the events
// table, so a steady-state scan is a single small query.
func (s *Scanner) Scan(ctx context.Context) ([]SourceCompleteness, error) {
	q := fmt.Sprintf(
		`SELECT org_id, source_id, contiguous_seq, max_seen_seq, client_allocated_seq, %s, %s
		   FROM usage_source_watermark`,
		s.db.AgeMillis("updated_at"), s.db.AgeMillis("last_event_at"),
	)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("scan watermark: %w", err)
	}

	out := make([]SourceCompleteness, 0, 16)
	for rows.Next() {
		var sc SourceCompleteness
		var staleMs, lastEventMs sql.NullInt64
		if err := rows.Scan(&sc.OrgID, &sc.SourceID, &sc.Contiguous, &sc.MaxSeen,
			&sc.ClientAllocated, &staleMs, &lastEventMs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan watermark row: %w", err)
		}
		sc.classify(s.criteria, staleMs, lastEventMs)
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Second pass (rows now closed): exact hole count for flagged middle gaps.
	for i := range out {
		if out[i].Status != StatusMiddleGap {
			continue
		}
		out[i].GapCount = s.middleHoleCount(ctx, &out[i])
	}

	// Third pass: per-source quarantine counts (stage C). One grouped query over
	// ONLY the quarantined rows (partial index idx_ods_quarantine), so healthy
	// sources cost nothing. Quarantine is orthogonal to the gap Status — a source
	// can be delivery-complete (ok) yet have quarantined rows; the endpoint's
	// top-level health factors both in.
	qc, err := s.quarantineCounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].QuarantineCount = qc[srcKey{out[i].OrgID, out[i].SourceID}]
	}

	// Fourth pass: per-source known-loss counts from the ledger (stage D1).
	kl, err := s.knownLossCounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].KnownLoss = kl[srcKey{out[i].OrgID, out[i].SourceID}]
	}
	return out, nil
}

type srcKey struct{ org, src string }

// quarantineCounts returns the count of quarantined (content_hash-failed) rows
// per (org, source). Backed by the partial index on ingest_status='quarantined'.
func (s *Scanner) quarantineCounts(ctx context.Context) (map[srcKey]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, source_id, COUNT(*) FROM usage_event_ods
		   WHERE ingest_status = 'quarantined' AND source_id IS NOT NULL
		   GROUP BY org_id, source_id`)
	if err != nil {
		return nil, fmt.Errorf("scan quarantine counts: %w", err)
	}
	defer rows.Close()
	out := map[srcKey]int64{}
	for rows.Next() {
		var k srcKey
		var n int64
		if err := rows.Scan(&k.org, &k.src, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// classify sets Status, TailPending, Promotable, and the age fields from the
// watermark columns. GapCount stays 0 here; Scan fills it for middle gaps.
//
// All gap conditions are CONTIGUITY-based (not max_seen-based) so they're
// ledger-aware for free: contiguous_seq advances past known-loss ledger entries
// (AdvanceWatermark), so once a gap's seqs are promoted to the ledger and
// contiguous catches up to client_allocated, the source classifies back to ok
// WITHOUT any ledger query in classify(). Pre-ledger this is equivalent to the
// B' max_seen-based conditions (contiguous == max_seen when the received range
// has no hole).
func (sc *SourceCompleteness) classify(c Criteria, staleMs, lastEventMs sql.NullInt64) {
	// accounted high-water for "still pending above it" = max(received, contiguous).
	// TailPending = allocated seqs above that (drops to 0 once the tail is ledgered
	// and contiguous catches up).
	accounted := sc.MaxSeen
	if sc.Contiguous > accounted {
		accounted = sc.Contiguous
	}
	if sc.ClientAllocated > accounted {
		sc.TailPending = sc.ClientAllocated - accounted
	}
	if staleMs.Valid {
		secs := staleMs.Int64 / 1000
		sc.StaleSeconds = &secs
	}
	if lastEventMs.Valid {
		secs := lastEventMs.Int64 / 1000
		sc.LastEventAgeSeconds = &secs
	}

	midStale := staleMs.Valid && staleMs.Int64 > c.MiddleGapAge.Milliseconds()
	tailStale := staleMs.Valid && staleMs.Int64 > c.TailGapTimeout.Milliseconds()
	promoteStale := staleMs.Valid && staleMs.Int64 > c.KnownLossTimeout.Milliseconds()
	silent := lastEventMs.Valid && lastEventMs.Int64 > c.SilenceTimeout.Milliseconds()

	// highest seq we know about (received or allocated). Unaccounted seqs above
	// contiguous exist iff contiguous < this.
	maxKnown := sc.MaxSeen
	if sc.ClientAllocated > maxKnown {
		maxKnown = sc.ClientAllocated
	}

	// Severity precedence: a hole inside the received range is the most
	// actionable, then a stale allocated-but-unaccounted tail, then plain silence.
	switch {
	case sc.Contiguous < sc.MaxSeen && midStale:
		sc.Status = StatusMiddleGap
	case sc.Contiguous < sc.ClientAllocated && tailStale:
		sc.Status = StatusTailGap
	case silent:
		sc.Status = StatusSilent
	default:
		sc.Status = StatusOK
	}

	// Promotable: any unaccounted seq above contiguous, aged past the (longer)
	// known-loss timeout → the projector ledgers it. Independent of Status (which
	// uses the shorter alarm thresholds).
	sc.Promotable = sc.Contiguous < maxKnown && promoteStale
}

// middleHoleCount returns the exact number of UNACCOUNTED seqs in (contiguous,
// max_seen] for one source: span width minus how many of that span landed in
// ODS minus how many are already in the known-loss ledger (stage D1: a ledgered
// seq is accounted, not a gap). Uses idx_ods_source_seq. On query error it falls
// back to the span width (an upper bound) — over-counting is safer than
// under-reporting a gap.
func (s *Scanner) middleHoleCount(ctx context.Context, sc *SourceCompleteness) int64 {
	span := sc.MaxSeen - sc.Contiguous
	if span <= 0 {
		return 0
	}
	var received int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_event_ods
		   WHERE org_id = ? AND source_id = ? AND source_seq > ? AND source_seq <= ?`,
		sc.OrgID, sc.SourceID, sc.Contiguous, sc.MaxSeen,
	).Scan(&received); err != nil {
		return span // upper bound on failure
	}
	// Count ledgered seqs in range that are NOT also in ODS. The NOT EXISTS guards
	// the rare "resurrection" (a seq confirmed-lost, then its real event arrives —
	// or a direct-abuse double-write): ODS presence is authoritative, so such a seq
	// counts as received, never double-subtracted here. Keeps received + ledgered
	// disjoint so `missing` stays correct.
	var ledgered int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_known_loss_ledger l
		   WHERE l.org_id = ? AND l.source_id = ? AND l.seq > ? AND l.seq <= ?
		     AND NOT EXISTS (SELECT 1 FROM usage_event_ods o
		                       WHERE o.org_id = l.org_id AND o.source_id = l.source_id AND o.source_seq = l.seq)`,
		sc.OrgID, sc.SourceID, sc.Contiguous, sc.MaxSeen,
	).Scan(&ledgered); err != nil {
		ledgered = 0 // ledger unreadable → treat as no accounted losses (over-count)
	}
	if missing := span - received - ledgered; missing > 0 {
		return missing
	}
	return 0
}

// knownLossCounts returns the number of ledgered (confirmed-lost) seqs per
// (org, source). The ledger PK (org_id, source_id, seq) makes this a cheap
// grouped count. Feeds SourceCompleteness.KnownLoss (stage D1).
func (s *Scanner) knownLossCounts(ctx context.Context) (map[srcKey]int64, error) {
	// Only count GENUINELY-lost seqs: ledgered AND not present in ODS. The
	// NOT EXISTS makes ODS presence authoritative, so a "resurrected" seq (in both
	// tables) is counted as received, not as a loss — keeping known_loss honest.
	rows, err := s.db.QueryContext(ctx,
		`SELECT l.org_id, l.source_id, COUNT(*) FROM usage_known_loss_ledger l
		   WHERE NOT EXISTS (SELECT 1 FROM usage_event_ods o
		                       WHERE o.org_id = l.org_id AND o.source_id = l.source_id AND o.source_seq = l.seq)
		   GROUP BY l.org_id, l.source_id`)
	if err != nil {
		return nil, fmt.Errorf("scan known-loss counts: %w", err)
	}
	defer rows.Close()
	out := map[srcKey]int64{}
	for rows.Next() {
		var k srcKey
		var n int64
		if err := rows.Scan(&k.org, &k.src, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
