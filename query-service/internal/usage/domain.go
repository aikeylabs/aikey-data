// Package usage provides usage query types and repository interfaces.
package usage

import "time"

// TimelinePoint is a single data point on a usage curve.
type TimelinePoint struct {
	Date        string `json:"date"` // YYYY-MM-DD
	TotalTokens int64  `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// ProtocolTimelinePoint adds provider dimension to a timeline point.
// JSON field is "protocol_type" for backward compatibility, but the value
// is provider_code (e.g. "kimi", "anthropic") not the wire protocol.
type ProtocolTimelinePoint struct {
	Date         string `json:"date"`
	ProtocolType string `json:"protocol_type"` // actually provider_code
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// ProtocolTotal is a single slice of a provider pie chart.
type ProtocolTotal struct {
	ProtocolType string `json:"protocol_type"` // actually provider_code
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// KeyTotal is a single entry in the per-key usage breakdown.
type KeyTotal struct {
	VirtualKeyID string `json:"virtual_key_id"`
	Alias        string `json:"alias,omitempty"` // human-readable key alias
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// UserRanking is a single entry in the per-user ranking.
type UserRanking struct {
	AccountID   string `json:"account_id"`
	SeatID      string `json:"seat_id"`
	TotalTokens int64  `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// QueryParams holds common query filters.
// Personal queries accept either SeatID or AccountID (fallback for personal/BYOK keys).
type QueryParams struct {
	OrgID     string
	SeatID    string
	AccountID string // used when SeatID is empty (personal key users without org seat)
	StartDate time.Time // inclusive
	EndDate   time.Time // inclusive
	Limit     int       // for ranking, default 50
}

// Defaults fills in zero-value defaults.
func (q *QueryParams) Defaults() {
	if q.EndDate.IsZero() {
		q.EndDate = time.Now().UTC().Truncate(24 * time.Hour)
	}
	if q.StartDate.IsZero() {
		q.StartDate = q.EndDate.AddDate(0, 0, -30)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
}
