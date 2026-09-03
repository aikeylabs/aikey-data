// Package conversation is the query-service read model for the enterprise
// conversation-audit feature. It serves the master admin console's three views
// over conversation_records (+ conversation_sessions) — seat list → session
// list → thread drawer — all tenant-scoped by org_id.
//
// All timestamps are projected as INT64 epoch milliseconds (via shared.DB
// EpochMillis) so a single Scan works on both PostgreSQL (TIMESTAMPTZ) and
// SQLite (INTEGER millis). Queries filter by conv_date ('YYYY-MM-DD') to prune
// PG monthly partitions.
package conversation

import "encoding/json"

// SeatSummary is one row of the seat-list aggregation (per developer / owner).
//
// Default sort is ACTIVITY (turn_count), not token. TotalTokens is a best-effort
// SUM of the per-turn display snapshot the proxy now captures (cache + input +
// output, folded from the stream); usage_fact_dwd remains the billing authority.
// 0 for turns captured without a usage frame (partial/interrupted streams).
type SeatSummary struct {
	OwnerAccountID string `json:"owner_account_id"`
	SessionCount   int64  `json:"session_count"`
	TurnCount      int64  `json:"turn_count"`
	ContentBytes   int64  `json:"content_bytes"`
	TotalTokens    int64  `json:"total_tokens"`
	LastActivityAt int64  `json:"last_activity_at"` // epoch ms, MAX(created_at)
}

// SessionSummary is one row of the per-seat session list (GROUP BY session_id).
// FirstSeenAt = MIN(created_at) is the "会话创建时间" the UI sorts by (DESC).
type SessionSummary struct {
	SessionID      string `json:"session_id"`
	FirstSeenAt    int64  `json:"first_seen_at"`    // epoch ms, MIN(created_at)
	LastActivityAt int64  `json:"last_activity_at"` // epoch ms, MAX(created_at)
	TurnCount      int64  `json:"turn_count"`
	TotalTokens    int64  `json:"total_tokens"`
}

// ThreadTurn is one turn (record) within a session, in conversation order.
// system_text is NOT here — it is stored once per session in
// conversation_sessions (first-wins) and returned on ThreadDetail.
type ThreadTurn struct {
	EventID       string `json:"event_id"`
	CreatedAt     int64  `json:"created_at"` // epoch ms
	Model         string `json:"model,omitempty"`
	ProviderCode  string `json:"provider_code,omitempty"`
	UserText      string `json:"user_text"`
	AssistantText string `json:"assistant_text"`
	DurationMs    int64  `json:"duration_ms"`
	RequestStatus string `json:"request_status"`
	// Per-turn token snapshot for the drawer (display only; usage_fact_dwd is the
	// billing authority). cached_input_tokens = cache READ, cache_creation_input_tokens
	// = cache WRITE — mirrors the conversation_records columns the proxy populates.
	// 0 when the turn was captured without a usage frame.
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	// ToolCalls is the JSON array of tool calls the model requested in this turn
	// (阶段8 P13 leg A), passed through VERBATIM as captured.
	//
	// 🔴 THREE renderings, and the console must keep them apart:
	//
	//	null / absent   the proxy that captured this turn does not collect tool
	//	                calls. The drawer says so. 🚫 NEVER "no tool calls" —
	//	                that would report a fact nobody established (task 13.8).
	//	[]              a collecting proxy looked and the model called nothing.
	//	[{...}]         the calls, in the order the model asked for them.
	//
	// 🔴 json.RawMessage so the bytes reach the console unchanged. Decoding here
	// would let this service normalise what an auditor is shown.
	ToolCalls json.RawMessage `json:"tool_calls"`

	// AppSlug is WHICH AGENT produced this turn ("claude-code", "cursor",
	// "unknown-app"), read at query time by joining usage_event_ods on
	// event_id (阶段8 P13 task 13.4, decision D-17).
	//
	// 🔴 A POINTER, and the nil case is the point (task 13.4a):
	//
	//	nil            the usage channel has not delivered this turn's event yet.
	//	               The console shows "attribution pending" and it resolves on
	//	               a later view. 🚫 NEVER blank, 🚫 never guessed.
	//	"unknown-app"  the usage channel DID arrive and the UA matched nothing.
	//	               A settled fact, not a pending one — 🔴 the two must not be
	//	               rendered the same (R-conversation-tool-visibility-2.S2),
	//	               because one changes by itself and the other never will.
	//	"claude-code"  resolved.
	//
	// 🔴 NO app_slug COLUMN is added to conversation_records (D-17). The usage
	// pipeline already owns this fact; a second copy would drift, and the two
	// arrive through different channels so the copy would be wrong precisely
	// while the difference mattered.
	//
	// 🔴 It is a WEAK signal — derived from a client-supplied User-Agent, so it
	// is forgeable. It may be displayed and 🚫 must never reach authorisation,
	// routing or billing.
	AppSlug *string `json:"app_slug,omitempty"`

	// CacheEnabled (decision B): 1 = the client requested prompt caching this turn,
	// 0 = off (or not captured). The drawer renders it as an explicit caching ON/OFF
	// switch — distinct from the cache token COUNTS, which read 0 in both cases.
	CacheEnabled int64 `json:"cache_enabled"`
}

// ThreadDetail is the full session thread the drawer renders: the once-per-session
// system prompt plus every turn in read-time-derived order (created_at, event_id).
type ThreadDetail struct {
	SessionID  string       `json:"session_id"`
	SystemText string       `json:"system_text,omitempty"`
	Turns      []ThreadTurn `json:"turns"`
}

// QueryParams scopes a conversation-audit query. OrgID is REQUIRED (tenant
// isolation — every query filters by it). OwnerAccountID / SessionID narrow to a
// seat / session (r2 #4: session_id is not globally unique, so it is always read
// with org + owner). StartDate / EndDate are inclusive 'YYYY-MM-DD' conv_date
// bounds for PG partition pruning; "" = unbounded.
type QueryParams struct {
	OrgID          string
	OwnerAccountID string
	SessionID      string
	StartDate      string
	EndDate        string
	Limit          int
	Offset         int
	// Server-side sort for the seat/session lists (clickable column headers).
	// SortBy is a stable column key (whitelisted in repository_sql.go — never
	// interpolated raw); SortDir is "asc"/"desc". Empty → the list's default
	// (seats: tokens DESC; sessions: last-activity DESC). Sorting must be
	// server-side because the lists are paginated (LIMIT/OFFSET): a client-side
	// sort would only reorder the current page, not the whole result set.
	SortBy  string
	SortDir string
	// Seat-list search filter (2026-07-31). Two inputs, OR'd together, because a
	// seat row's DISPLAY LABEL has two possible origins and the search must match
	// whichever the admin actually sees ("what you see is what you can search"):
	//
	//   SeatKeys    — exact seat keys the master facade resolved from a human name
	//                 (alias / invited email) in ITS org_seats directory. Names
	//                 never reach query-service; the alias truth source lives in
	//                 control-master. Both ids of a matched seat are passed so
	//                 seat_id rows (>=alpha.4) and legacy owner rows both match.
	//   SeatKeyLike — the RAW search term, matched as a case-insensitive substring
	//                 of seatKeyExpr itself. Required because conversation_records
	//                 can carry a seat key that has NO org_seats row at all (demo
	//                 /imported/decommissioned seats): the console then renders the
	//                 raw key as the label, so directory-only matching would make a
	//                 visible row unsearchable — the exact bug reported 2026-07-31.
	//
	// SeatFilterSet distinguishes "no search at all" from "searched and matched
	// nothing" — collapsing them would turn a failed search into the full list.
	SeatKeys      []string
	SeatKeyLike   string
	SeatFilterSet bool
}
