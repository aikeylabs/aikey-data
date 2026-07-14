package conversation

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/AiKeyLabs/aikey-data/query-service/internal/shared"
)

type sqlRepo struct{ db *shared.DB }

// NewSQLRepository returns a Repository backed by PostgreSQL or SQLite (dialect
// handled by shared.DB). Mirrors usage.NewSQLRepository.
func NewSQLRepository(db *shared.DB) Repository { return &sqlRepo{db: db} }

const (
	defaultListLimit = 20   // matches the UI page size (decision 12)
	maxListLimit     = 1000 // hard cap so a bad ?limit= can't scan a whole partition
	defaultTurnLimit = 1000 // a single session's turns (= cap; whole thread by default)
)

func clampLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// Sortable column whitelists for the clickable list headers. The key is the
// stable API token sent by the UI; the value is the SQL ORDER BY expression.
// SECURITY: only these mapped expressions ever reach ORDER BY — the raw client
// key is never interpolated, so an arbitrary ?sort= cannot inject SQL. An
// unknown/empty key falls back to the per-list default. NOTE: the "席位" (seat
// owner) column is intentionally NOT sortable — its display label is the seat
// alias resolved client-side (not in conversation_records), so a server-side
// sort by owner_account_id wouldn't match the visible order (decision b,
// 2026-06-17).
var seatSortCols = map[string]string{
	"sessions": "COUNT(DISTINCT COALESCE(session_id, event_id))", // 会话数
	"turns":    "COUNT(*)",                                       // 轮数
	"tokens":   "COALESCE(SUM(total_tokens), 0)",                 // Token
	"activity": "MAX(created_at)",                                // 最近活动
}

var sessionSortCols = map[string]string{
	"created":  "MIN(created_at)",               // 创建时间
	"turns":    "COUNT(*)",                       // 轮数
	"tokens":   "COALESCE(SUM(total_tokens), 0)", // Token
	"activity": "MAX(created_at)",                // 最近活动
}

// orderByClause builds a safe "ORDER BY <expr> <dir>, <tiebreak>" from a column
// whitelist. dir defaults to DESC; the tiebreak keeps pagination stable when the
// primary key ties (same token count etc.). When sortBy is unknown/empty it uses
// defaultExpr + DESC (the list's default sort).
func orderByClause(cols map[string]string, sortBy, sortDir, defaultExpr, tiebreak string) string {
	expr, ok := cols[sortBy]
	if !ok {
		return defaultExpr + " DESC, " + tiebreak
	}
	dir := "DESC"
	if strings.EqualFold(sortDir, "asc") {
		dir = "ASC"
	}
	return expr + " " + dir + ", " + tiebreak
}

// appendDateRange adds inclusive conv_date bounds (partition pruning) when set.
// conv_date is DATE (PG) / TEXT (SQLite) holding 'YYYY-MM-DD'; both compare
// correctly against a 'YYYY-MM-DD' string bind.
func appendDateRange(where string, args []any, p QueryParams) (string, []any) {
	if p.StartDate != "" {
		where += " AND conv_date >= ?"
		args = append(args, p.StartDate)
	}
	if p.EndDate != "" {
		where += " AND conv_date <= ?"
		args = append(args, p.EndDate)
	}
	return where, args
}

// seatKeyExpr is the seat-dimension attribution key (2026-07-07, alpha.4):
// prefer the explicit seat_id stamped by >=alpha.4 proxies (the same
// route.SeatID usage events carry), fall back to owner_account_id for legacy
// rows. Why: owner_account_id is the VK OWNER — for shared OAuth-pool VKs
// that's the pool creator, and keying the audit on it filed employee turns
// under a stranger seat row while the usage UI (seat-keyed) attributed
// correctly. NULLIF covers both NULL and '' storage shapes. Every
// seat-scoped query below MUST use this expression, never raw
// owner_account_id, or the two views diverge again.
const seatKeyExpr = "COALESCE(NULLIF(seat_id, ''), owner_account_id)"

func (r *sqlRepo) SeatSummaries(ctx context.Context, p QueryParams) ([]SeatSummary, error) {
	where := "org_id = ?"
	args := []any{p.OrgID}
	where, args = appendDateRange(where, args, p)
	q := fmt.Sprintf(`
		SELECT `+seatKeyExpr+`,
		       COUNT(DISTINCT COALESCE(session_id, event_id)),
		       COUNT(*),
		       COALESCE(SUM(content_bytes), 0),
		       COALESCE(SUM(total_tokens), 0),
		       %s
		FROM conversation_records
		WHERE %s
		GROUP BY `+seatKeyExpr+`
		ORDER BY %s
		LIMIT ? OFFSET ?`, r.db.EpochMillis("MAX(created_at)"), where,
		// Default: tokens DESC (per product — the seat list ranks by spend);
		// tiebreak by recency then id for stable pagination.
		orderByClause(seatSortCols, p.SortBy, p.SortDir, "COALESCE(SUM(total_tokens), 0)", "MAX(created_at) DESC, "+seatKeyExpr))
	args = append(args, clampLimit(p.Limit, defaultListLimit), p.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("seat summaries: %w", err)
	}
	defer rows.Close()
	var out []SeatSummary
	for rows.Next() {
		var s SeatSummary
		if err := rows.Scan(&s.OwnerAccountID, &s.SessionCount, &s.TurnCount, &s.ContentBytes, &s.TotalTokens, &s.LastActivityAt); err != nil {
			return nil, fmt.Errorf("scan seat summary: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionSummaries lists a seat's sessions. session_id may be NULL — a
// conversation captured without a session identifier (e.g. the OpenClaw chat
// gateway; resolveSessionID returns "" → stored NULL). Such turns CANNOT be
// reconstructed into multi-turn sessions (no grouping key was captured), so each
// sessionless record stands as its OWN single-turn session keyed by its event_id:
// every conversation_records query here groups/filters by COALESCE(session_id,
// event_id) — real turns group by their shared session_id, sessionless turns split
// by the always-present unique event_id (PK). This (a) avoids "converting NULL to
// string is unsupported" on scan and (b) replaces the earlier single "" pseudo-
// session bucket, which lumped every sessionless conversation into one thread
// (decision B, 2026-06-17). LIMITATION: SessionSystemText reads conversation_sessions,
// which has NO event_id column (system_text is deduped once-per-session keyed by
// session_id; sessionless traffic collapses into the "" session row), so sessionless
// threads carry no per-conversation system prompt. Bugfix:
// 2026-06-17-conversation-audit-query-null-session-id.md
func (r *sqlRepo) SessionSummaries(ctx context.Context, p QueryParams) ([]SessionSummary, error) {
	where := "org_id = ? AND "+seatKeyExpr+" = ?"
	args := []any{p.OrgID, p.OwnerAccountID}
	where, args = appendDateRange(where, args, p)
	q := fmt.Sprintf(`
		SELECT COALESCE(session_id, event_id), %s, %s, COUNT(*), COALESCE(SUM(total_tokens), 0)
		FROM conversation_records
		WHERE %s
		GROUP BY COALESCE(session_id, event_id)
		ORDER BY %s
		LIMIT ? OFFSET ?`,
		r.db.EpochMillis("MIN(created_at)"), r.db.EpochMillis("MAX(created_at)"), where,
		// Default: last-activity DESC (per product, 2026-06-17 — was created DESC);
		// tiebreak by created for stable pagination.
		orderByClause(sessionSortCols, p.SortBy, p.SortDir, "MAX(created_at)", "MIN(created_at) DESC"))
	args = append(args, clampLimit(p.Limit, defaultListLimit), p.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("session summaries: %w", err)
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(&s.SessionID, &s.FirstSeenAt, &s.LastActivityAt, &s.TurnCount, &s.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *sqlRepo) ThreadDetail(ctx context.Context, p QueryParams) (*ThreadDetail, error) {
	td := &ThreadDetail{SessionID: p.SessionID, Turns: []ThreadTurn{}}

	sys, err := r.SessionSystemText(ctx, p)
	if err != nil {
		return nil, err
	}
	td.SystemText = sys

	// Turns: read-time-derived order (created_at, event_id) — r2 #8 dropped turn_seq.
	q := fmt.Sprintf(`
		SELECT event_id, %s, COALESCE(model, ''), COALESCE(provider_code, ''),
		       COALESCE(user_text, ''), COALESCE(assistant_text, ''),
		       COALESCE(duration_ms, 0), request_status, COALESCE(total_tokens, 0),
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		       COALESCE(cached_input_tokens, 0), COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(reasoning_tokens, 0), COALESCE(cache_enabled, 0)
		FROM conversation_records
		WHERE org_id = ? AND `+seatKeyExpr+` = ? AND COALESCE(session_id, event_id) = ?
		ORDER BY created_at, event_id
		LIMIT ? OFFSET ?`, r.db.EpochMillis("created_at"))
	rows, err := r.db.QueryContext(ctx, q,
		p.OrgID, p.OwnerAccountID, p.SessionID, clampLimit(p.Limit, defaultTurnLimit), p.Offset)
	if err != nil {
		return nil, fmt.Errorf("thread turns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t ThreadTurn
		if err := rows.Scan(&t.EventID, &t.CreatedAt, &t.Model, &t.ProviderCode,
			&t.UserText, &t.AssistantText, &t.DurationMs, &t.RequestStatus, &t.TotalTokens,
			&t.InputTokens, &t.OutputTokens, &t.CachedInputTokens, &t.CacheCreationInputTokens,
			&t.ReasoningTokens, &t.CacheEnabled); err != nil {
			return nil, fmt.Errorf("scan thread turn: %w", err)
		}
		td.Turns = append(td.Turns, t)
	}
	return td, rows.Err()
}

// SessionSystemText returns the once-per-session system prompt (conversation_sessions,
// first-wins), "" when no session row exists.
//
// conversation_sessions has NO seat_id column and keeps the raw
// owner_account_id COLUMN NAME — but its VALUE is the seat key: the collector
// upserts sessions with the same seat-or-owner key (see collector service.go,
// 2026-07-07), and legacy rows carry owner, which is exactly what seatKeyExpr
// falls back to for their records. So binding p.OwnerAccountID (= the seat key
// the caller navigated with) matches both generations without a schema change.
func (r *sqlRepo) SessionSystemText(ctx context.Context, p QueryParams) (string, error) {
	var sys string
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(system_text, '') FROM conversation_sessions
		 WHERE org_id = ? AND owner_account_id = ? AND COALESCE(session_id, '') = ?`,
		p.OrgID, p.OwnerAccountID, p.SessionID).Scan(&sys)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("session system_text: %w", err)
	}
	return sys, nil
}

// StreamSessionIDs invokes fn for each DISTINCT session_id of a seat, oldest
// first (so the .zip reads chronologically), honoring the conv_date range. No
// cap — the export enumerates every session.
func (r *sqlRepo) StreamSessionIDs(ctx context.Context, p QueryParams, fn func(string) error) error {
	where := "org_id = ? AND "+seatKeyExpr+" = ?"
	args := []any{p.OrgID, p.OwnerAccountID}
	where, args = appendDateRange(where, args, p)
	q := "SELECT COALESCE(session_id, event_id) FROM conversation_records WHERE " + where +
		" GROUP BY COALESCE(session_id, event_id) ORDER BY MIN(created_at)"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("stream session ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return fmt.Errorf("scan session id: %w", err)
		}
		if err := fn(sid); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamSessionTurns invokes fn for every turn of one session in
// (created_at, event_id) order, NO LIMIT (full conversation for export).
func (r *sqlRepo) StreamSessionTurns(ctx context.Context, p QueryParams, fn func(*ThreadTurn) error) error {
	q := fmt.Sprintf(`
		SELECT event_id, %s, COALESCE(model, ''), COALESCE(provider_code, ''),
		       COALESCE(user_text, ''), COALESCE(assistant_text, ''),
		       COALESCE(duration_ms, 0), request_status, COALESCE(total_tokens, 0),
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		       COALESCE(cached_input_tokens, 0), COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(reasoning_tokens, 0), COALESCE(cache_enabled, 0)
		FROM conversation_records
		WHERE org_id = ? AND `+seatKeyExpr+` = ? AND COALESCE(session_id, event_id) = ?
		ORDER BY created_at, event_id`, r.db.EpochMillis("created_at"))
	rows, err := r.db.QueryContext(ctx, q, p.OrgID, p.OwnerAccountID, p.SessionID)
	if err != nil {
		return fmt.Errorf("stream session turns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t ThreadTurn
		if err := rows.Scan(&t.EventID, &t.CreatedAt, &t.Model, &t.ProviderCode,
			&t.UserText, &t.AssistantText, &t.DurationMs, &t.RequestStatus, &t.TotalTokens,
			&t.InputTokens, &t.OutputTokens, &t.CachedInputTokens, &t.CacheCreationInputTokens,
			&t.ReasoningTokens, &t.CacheEnabled); err != nil {
			return fmt.Errorf("scan turn: %w", err)
		}
		if err := fn(&t); err != nil {
			return err
		}
	}
	return rows.Err()
}
