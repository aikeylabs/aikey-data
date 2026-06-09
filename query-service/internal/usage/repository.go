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

	// PersonalByProtocolHourly is the intra-day counterpart of
	// PersonalByProtocolTimeline — returns hourly total_tokens grouped
	// by (hour, provider_code) for the single day at QueryParams.StartDate.
	// Powers the "1D" range option on /user/usage-ledger's stacked
	// protocol chart. Added 2026-05-28.
	PersonalByProtocolHourly(ctx context.Context, p QueryParams) ([]ProtocolHourlyPoint, error)

	// PersonalByProtocolTimeline returns daily total_tokens grouped by protocol.
	// Filters: user_usage_scope = 'normal'
	PersonalByProtocolTimeline(ctx context.Context, p QueryParams) ([]ProtocolTimelinePoint, error)

	// PersonalByProtocolTotal returns total_tokens per protocol (pie chart).
	PersonalByProtocolTotal(ctx context.Context, p QueryParams) ([]ProtocolTotal, error)

	// PersonalByKeyTotal returns total_tokens per virtual_key_id.
	PersonalByKeyTotal(ctx context.Context, p QueryParams) ([]KeyTotal, error)

	// PersonalByAppTotal returns total_tokens per (app_slug, provider_code)
	// pair. Rows with app_slug="" represent direct /v1/... traffic without
	// any app context (frontend remaps to friendly tool name via
	// provider_code). Added 2026-05-25 for /user/usage-ledger "Usage By
	// App" ranking chart.
	PersonalByAppTotal(ctx context.Context, p QueryParams) ([]AppTotal, error)

	// PersonalBySessionTotal returns total_tokens per session_id, sorted
	// by total_tokens DESC and capped at QueryParams.Limit (default 10
	// for the Performance Top N chart). Empty session_id is coalesced to
	// "" and represented as one bucket — clients without a session header
	// (curl / generic SDKs / legacy events) aggregate together so users
	// can see how much of their traffic carries no session dimension.
	//
	// SessionID query param on QueryParams is IGNORED here (selecting a
	// session shouldn't shrink the session ranking to one row). Added
	// 2026-05-26 for /user/performance "Top N sessions" chart.
	PersonalBySessionTotal(ctx context.Context, p QueryParams) ([]SessionTotal, error)

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

	// PersonalUsageDetail returns per-request rows for the Usage Detail page
	// (last 7 days window via StartDate/EndDate, optional drill-down filters:
	// Unpriced / Model / VirtualKeyID / SessionID / AppSlug). Reads
	// usage_event_ods (per-event source) — full token breakdown + cost +
	// failure reason so the "未计价" rows explain themselves.
	PersonalUsageDetail(ctx context.Context, p QueryParams) ([]UsageDetailRow, error)

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

	// MasterUsageDetail returns per-event audit rows for one org within
	// [p.StartDate, p.EndDate] (inclusive, by usage_date — the DWD partition
	// key, so the scan prunes to the relevant months). Ordered event_time DESC,
	// capped at p.Limit. Powers the enterprise usage-audit page's last-3-days
	// table. v1.0.1-alpha.4.
	MasterUsageDetail(ctx context.Context, p QueryParams) ([]MasterUsageAuditRow, error)

	// StreamMasterUsageExport streams every audit row for one org within
	// [p.StartDate, p.EndDate] (inclusive, by usage_date) to fn, one row at a
	// time via a DB cursor — memory stays O(1) regardless of range size so a
	// full year exports without materialising. fn is the CSV writer; a non-nil
	// fn error aborts the stream. The handler enforces the ≤366-day cap.
	StreamMasterUsageExport(ctx context.Context, p QueryParams, fn func(*MasterUsageAuditRow) error) error

	// --- Admin (cost-pricing Stage 3) ---

	// ListUnpricedModels returns rows from the pending-pricing queue
	// (unpriced_models), sorted by event_count DESC. status "" or "all"
	// returns every row; otherwise it filters to that status
	// (pending / acknowledged / fixed).
	ListUnpricedModels(ctx context.Context, status string) ([]UnpricedModel, error)

	// UpdateUnpricedModelStatus sets the status of one (provider, model)
	// row. Returns ErrNotFound when no such row exists so the handler can
	// reply 404 rather than a silent 200 on a typo'd model name.
	UpdateUnpricedModelStatus(ctx context.Context, provider, model, status string) error

	// GetEventAudit returns the full cost-audit trail for one event,
	// JOINing usage_fact_dwd with pricing_snapshots. Returns ErrNotFound
	// when the event_id is unknown.
	GetEventAudit(ctx context.Context, eventID string) (*EventAudit, error)
}
