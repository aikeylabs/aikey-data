// Package quota is the collector-side enterprise-quota materializer (Phase 2
// Stage 5 — design §5.4/§5.6). As the projector writes each new usage fact into
// usage_fact_dwd, it accumulates the seat's (and its groups') used_amount into
// quota_counter and records any newly-crossed thresholds in triggered_thresholds
// (per-period dedup). This is server-side because the proxy is a client-side
// component with no notification infra; here we MATERIALIZE + RECORD only —
// actual delivery (email/webhook) is a later step.
//
// Runs on the same PG (aikey_control) as control's quota_subject/quota_counter.
// Strictly best-effort: a failure here must never break DWD projection (the
// caller swallows errors), so quota is a pure bystander to the usage pipeline.
package quota

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
)

// Subject kinds + metrics (mirror control / proxy — kept minimal).
const (
	KindSeat     = "seat"
	KindGroup    = "group"
	MetricTokens = "tokens"
	MetricUSD    = "usd"
	// ActionHardBlock is the threshold action that, when newly crossed, triggers
	// the P5 push (bump the affected seats' sync_version so the proxy re-pulls the
	// over-limit baseline promptly). "warn" crossings don't push (they don't block).
	ActionHardBlock = "hard_block"
)

// Subject is the config root as read from quota_subject (only the fields the
// materializer needs).
type Subject struct {
	SubjectID   string
	SubjectKind string
	Members     []string
	Rules       []Rule
}

// Rule is one (metric, period) limit with tiered thresholds.
type Rule struct {
	Metric      string      `json:"metric"`
	Period      string      `json:"period"`
	LimitAmount float64     `json:"limit_amount"`
	Thresholds  []Threshold `json:"thresholds"`
}

// Threshold is one tier (pct of limit) with an action. Stage 5 records crossings
// regardless of action; the action drives delivery (deferred).
type Threshold struct {
	Pct    int    `json:"pct"`
	Action string `json:"action"`
}

// Storage reads quota_subject and read-modify-writes quota_counter on the
// collector's PG handle.
type Storage struct{ db *shared.DB }

func NewStorage(db *shared.DB) *Storage { return &Storage{db: db} }

// LoadEnabledSubjects returns all enabled subjects. Tolerant: a DB without the
// quota tables (pre-Phase-2 / Personal) yields an empty slice, not an error.
func (s *Storage) LoadEnabledSubjects(ctx context.Context) ([]Subject, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT subject_id, subject_kind, members, rules FROM quota_subject WHERE enabled = 1`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		var (
			sub      Subject
			members  sql.NullString
			rulesStr string
		)
		if err := rows.Scan(&sub.SubjectID, &sub.SubjectKind, &members, &rulesStr); err != nil {
			return nil, err
		}
		if rulesStr != "" {
			if err := json.Unmarshal([]byte(rulesStr), &sub.Rules); err != nil {
				return nil, fmt.Errorf("quota: parse rules %q: %w", sub.SubjectID, err)
			}
		}
		if members.Valid && members.String != "" {
			if err := json.Unmarshal([]byte(members.String), &sub.Members); err != nil {
				return nil, fmt.Errorf("quota: parse members %q: %w", sub.SubjectID, err)
			}
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// RecomputeFromDWD sets used_amount to the AUTHORITATIVE SUM over usage_fact_dwd
// for the subject's seats in the period, making quota_counter a true materialized
// view of current-period usage. Returns the new used + the period's already
// triggered thresholds.
//
// Why recompute (absolute SET) instead of a forward "+= delta" tally: a forward
// tally only sees facts projected since the collector last started, so it
// UNDERCOUNTS after any restart / mid-period deploy — the web overview then
// shows less than reality AND threshold detection can miss a crossing. Reading
// SUM(DWD) each time the projector lands a fact keeps the counter == DWD truth:
// restart-safe, drift-free, no double-count. The SUM is indexed by
// (seat_id, billing_period); at this feature's scale the per-fact cost is
// negligible. If fact throughput grows, debounce per (subject, period) per
// projection batch. See
// workflow/CI/bugfix/2026-06-03-team-usage-refresh-contract-mismatch.md.
func (s *Storage) RecomputeFromDWD(ctx context.Context, subjectID, metric, periodKey string, seatIDs []string) (float64, []int, error) {
	if len(seatIDs) == 0 {
		return 0, nil, nil
	}
	used, err := s.sumFromDWD(ctx, metric, periodKey, seatIDs)
	if err != nil {
		return 0, nil, err
	}
	// Absolute SET (materialized view). Preserve triggered_thresholds across the
	// UPSERT (only the caller's MarkTriggered ever rewrites it).
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_counter (subject_id, metric, period_key, used_amount, triggered_thresholds, updated_at)
		VALUES (?, ?, ?, ?, '[]', `+s.db.NowEpochMillis()+`)
		ON CONFLICT (subject_id, metric, period_key) DO UPDATE
		    SET used_amount = excluded.used_amount,
		        updated_at  = excluded.updated_at`,
		subjectID, metric, periodKey, used); err != nil {
		return 0, nil, fmt.Errorf("quota: write counter %s/%s/%s: %w", subjectID, metric, periodKey, err)
	}
	var tt sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT triggered_thresholds FROM quota_counter
		 WHERE subject_id = ? AND metric = ? AND period_key = ?`,
		subjectID, metric, periodKey).Scan(&tt); err != nil {
		return 0, nil, fmt.Errorf("quota: read counter %s/%s/%s: %w", subjectID, metric, periodKey, err)
	}
	var triggered []int
	if tt.Valid && tt.String != "" {
		_ = json.Unmarshal([]byte(tt.String), &triggered)
	}
	return used, triggered, nil
}

// sumFromDWD returns Σ(metric) over usage_fact_dwd for the given seats in the
// period (used by RecomputeFromDWD to materialize the counter). Tolerant: a
// missing usage_fact_dwd (pre-Phase-2 / Personal without the data component)
// yields 0, not an error.
func (s *Storage) sumFromDWD(ctx context.Context, metric, periodKey string, seatIDs []string) (float64, error) {
	if len(seatIDs) == 0 {
		return 0, nil
	}
	sumExpr, ok := dwdMetricSumExpr(metric)
	if !ok {
		return 0, fmt.Errorf("quota: unknown metric %q", metric)
	}
	// seat_id IN (?,?,…) + period filter; shared.DB rewrites ? → $n for PG.
	ph := make([]string, len(seatIDs))
	args := make([]any, 0, len(seatIDs)+1)
	for i, id := range seatIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	where := "seat_id IN (" + strings.Join(ph, ",") + ")"
	if pf, parg, ok := dwdPeriodFilter(s.db, periodKey); ok {
		where += " AND " + pf
		args = append(args, parg)
	}
	var used float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT `+sumExpr+` FROM usage_fact_dwd WHERE `+where, args...).Scan(&used); err != nil {
		if isMissingTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("quota: sum dwd %s/%s: %w", metric, periodKey, err)
	}
	return used, nil
}

// dwdMetricSumExpr maps a quota metric to its usage_fact_dwd SUM expression.
// Hardcoded per metric (never interpolate caller input into SQL).
//
// 方案 A (2026-06-04): input_tokens is now the PURE (uncached) input, so the token
// SUM (input + output + cached + cache_creation + reasoning) is the TRUE token
// count — no cache double-count. (Pre-方案-A input_tokens was the total incl. cache,
// so this same sum double-counted the cache buckets, consistently on both proxy and
// server but inflated.) See update/20260604-token-input-纯输入语义治本-方案A.md.
func dwdMetricSumExpr(metric string) (string, bool) {
	switch metric {
	case MetricTokens:
		return "COALESCE(SUM(input_tokens + output_tokens + cached_input_tokens + cache_creation_input_tokens + reasoning_tokens), 0)", true
	case MetricUSD:
		return "COALESCE(SUM(billable_amount), 0)", true
	default:
		return "", false
	}
}

// dwdPeriodFilter maps a periodKey ("monthly:2026-06" / "daily:2026-06-03") to a
// usage_fact_dwd WHERE fragment + bind arg. monthly uses billing_period (DWD's
// native 'YYYY-MM' column); daily dates event_time. Unknown shape → no filter
// (ok=false), summing the whole table — defensive, shouldn't happen.
func dwdPeriodFilter(db *shared.DB, periodKey string) (string, any, bool) {
	kind, val, found := strings.Cut(periodKey, ":")
	if !found || val == "" {
		return "", nil, false
	}
	switch kind {
	case "monthly":
		return "billing_period = ?", val, true
	case "daily":
		return db.DateOf("event_time") + " = ?", val, true
	default:
		return "", nil, false
	}
}

// MarkTriggered overwrites the period's triggered_thresholds (the caller passes
// the full updated set, so this stays a single idempotent write).
func (s *Storage) MarkTriggered(ctx context.Context, subjectID, metric, periodKey string, triggered []int) error {
	b, err := json.Marshal(triggered)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE quota_counter SET triggered_thresholds = ?, updated_at = `+s.db.NowEpochMillis()+`
		WHERE subject_id = ? AND metric = ? AND period_key = ?`,
		string(b), subjectID, metric, periodKey)
	return err
}

// BumpSyncVersionForSeats nudges each seat's account sync_version so the CLI
// re-pulls the snapshot promptly — the P5 "push": after a hard_block crossing the
// proxy should learn the new over-limit baseline without waiting for the periodic
// pull. Mirrors control's snapshot.Service.BumpForSeat (seat→account→bump) but runs
// on the collector's shared PG, consistent with the materializer already writing
// quota_counter there (avoids a control-master module dependency).
//
// Best-effort + tolerant (this is a TIMELINESS signal, never a correctness
// dependency — usd/token are already enforced from the baseline + P7 local pricing):
//   - missing org_seats/account_sync_versions (Personal — no org schema) → skip silently;
//   - unclaimed seat (no account_id) / no row → skip;
//   - a real write error → WARN, continue with the other seats.
// The bump SQL is dialect-handled by shared.DB (? rewrite + Now()); the ON CONFLICT
// increment matches snapshot.{postgres,sqlite}.BumpSyncVersion.
func (s *Storage) BumpSyncVersionForSeats(ctx context.Context, seatIDs []string) {
	now := s.db.Now()
	for _, seatID := range seatIDs {
		if seatID == "" {
			continue
		}
		var accountID string
		err := s.db.QueryRowContext(ctx,
			`SELECT account_id FROM org_seats WHERE seat_id = ? AND account_id IS NOT NULL AND account_id != ''`,
			seatID).Scan(&accountID)
		if err == sql.ErrNoRows || isMissingTable(err) {
			continue // unclaimed seat, or Personal without org schema — expected
		}
		if err != nil {
			slog.Warn("quota.sync_version.resolve_failed", "seat_id", seatID, "error", err.Error())
			continue
		}
		if accountID == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO account_sync_versions (account_id, sync_version, updated_at)
			 VALUES (?, 1, `+now+`)
			 ON CONFLICT (account_id) DO UPDATE
			     SET sync_version = account_sync_versions.sync_version + 1,
			         updated_at   = `+now+``,
			accountID); err != nil {
			slog.Warn("quota.sync_version.bump_failed", "account_id", accountID, "error", err.Error())
		}
	}
}

func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "no such table") ||
		strings.Contains(m, "does not exist") ||
		strings.Contains(m, "undefined table")
}
