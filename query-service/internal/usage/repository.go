package usage

import "context"

// Repository reads aggregated usage data from the DWD layer.
type Repository interface {
	// --- Personal page ---

	// PersonalTimeline returns daily total_tokens for a seat within a date range.
	// Filters: user_usage_scope = 'normal'
	PersonalTimeline(ctx context.Context, p QueryParams) ([]TimelinePoint, error)

	// PersonalHourlyTimeline returns hourly total_tokens for a single
	// UTC calendar date (p.StartDate). Returns 0..24 rows (hours with
	// activity); callers pad sparse hours client-side.
	PersonalHourlyTimeline(ctx context.Context, p QueryParams) ([]HourlyPoint, error)

	// PersonalByProtocolTimeline returns daily total_tokens grouped by protocol.
	// Filters: user_usage_scope = 'normal'
	PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error)

	// PersonalByProtocolTotal returns total_tokens per protocol (pie chart).
	PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error)

	// PersonalByKeyTotal returns total_tokens per virtual_key_id.
	PersonalByKeyTotal(ctx context.Context, p QueryParams) ([]KeyTotal, error)

	// PersonalByModelTotal returns per-model token & request totals,
	// sorted by total_tokens DESC and capped at 20 rows. Powers the
	// `/user/cost` "Usage by model" chart. NULL / empty `model` values
	// are coalesced to "unknown" so the SUM never silently drops them.
	PersonalByModelTotal(ctx context.Context, p QueryParams) ([]ModelTotal, error)

	// PersonalRecent returns the N most recent non-canary requests
	// straight from `usage_event_ods` (not DWD aggregates) so the
	// route_source filter excluding 'canary' is possible. Used by the
	// Overview "Recent Requests" card. Default/max N enforced at the
	// handler boundary so this layer trusts whatever Limit is set.
	PersonalRecent(ctx context.Context, p QueryParams) ([]RecentRequest, error)

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
