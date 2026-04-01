// Package usage provides usage query types and repository interfaces.
package usage

import "time"

// TimelinePoint is a single data point on a usage curve.
type TimelinePoint struct {
	Date        string `json:"date"` // YYYY-MM-DD
	TotalTokens int64  `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// ProtocolTimelinePoint adds protocol_type dimension to a timeline point.
type ProtocolTimelinePoint struct {
	Date         string `json:"date"`
	ProtocolType string `json:"protocol_type"`
	TotalTokens  int64  `json:"total_tokens"`
	RequestCount int64  `json:"request_count"`
}

// ProtocolTotal is a single slice of a protocol pie chart.
type ProtocolTotal struct {
	ProtocolType string `json:"protocol_type"`
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
type QueryParams struct {
	OrgID     string
	SeatID    string
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
