package conversation

import "context"

// Repository is the conversation-audit read model. All methods are tenant-scoped
// by QueryParams.OrgID.
type Repository interface {
	// SeatSummaries aggregates conversation_records per owner_account_id (the
	// seat-list view), ordered by turn_count DESC (activity).
	SeatSummaries(ctx context.Context, p QueryParams) ([]SeatSummary, error)
	// SessionSummaries aggregates one seat's records per session_id (the
	// session-list view), ordered by first_seen_at DESC.
	SessionSummaries(ctx context.Context, p QueryParams) ([]SessionSummary, error)
	// ThreadDetail returns one session's system prompt (from conversation_sessions)
	// plus every turn (from conversation_records) in (created_at, event_id) order.
	ThreadDetail(ctx context.Context, p QueryParams) (*ThreadDetail, error)

	// --- export (streaming, NO row cap — a full conversation must never be
	// silently truncated on export) ---

	// SessionSystemText returns the once-per-session system prompt, "" if absent.
	SessionSystemText(ctx context.Context, p QueryParams) (string, error)
	// StreamSessionIDs invokes fn for each DISTINCT session_id of a seat (oldest
	// first), honoring the conv_date range. Used to enumerate a seat's sessions
	// for the .zip export without buffering them all.
	StreamSessionIDs(ctx context.Context, p QueryParams, fn func(sessionID string) error) error
	// StreamSessionTurns invokes fn for every turn of one session in
	// (created_at, event_id) order, with NO LIMIT — the export must carry the
	// whole conversation.
	StreamSessionTurns(ctx context.Context, p QueryParams, fn func(*ThreadTurn) error) error
}
