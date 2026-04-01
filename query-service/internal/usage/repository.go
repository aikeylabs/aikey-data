package usage

import "context"

// Repository reads aggregated usage data from the DWD layer.
type Repository interface {
	// --- Personal page ---

	// PersonalTimeline returns daily total_tokens for a seat within a date range.
	// Filters: user_usage_scope = 'normal'
	PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error)

	// PersonalByProtocolTimeline returns daily total_tokens grouped by protocol.
	// Filters: user_usage_scope = 'normal'
	PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error)

	// PersonalByProtocolTotal returns total_tokens per protocol (pie chart).
	// Filters: user_usage_scope = 'normal'
	PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error)

	// --- Master page ---

	// MasterUserRanking returns top users by total_tokens within an org.
	// Filters: user_usage_scope = 'normal'
	MasterUserRanking(ctx context.Context, p QueryParams) ([]UserRanking, error)

	// MasterByProtocolTotal returns total_tokens per protocol for the org.
	// Filters: billing_scope IN ('org_only','org_and_user')
	MasterByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error)

	// MasterTimeline returns daily total_tokens for the entire org.
	// Filters: billing_scope IN ('org_only','org_and_user')
	MasterTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error)
}
