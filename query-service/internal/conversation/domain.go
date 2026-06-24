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
}
