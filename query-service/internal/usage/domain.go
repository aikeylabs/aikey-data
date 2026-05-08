// Package usage provides usage query types and repository interfaces.
package usage

import (
	"log/slog"
	"time"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// TimelinePoint is a single data point on a usage curve.
type TimelinePoint struct {
	Date        string `json:"date"` // YYYY-MM-DD
	TotalTokens int64  `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// HourlyPoint is an intra-day usage bucket — one slot per hour for a
// given calendar date. Used by the overview "Today used" card to
// render a 24-bar intra-day distribution.
//
// Timezone: post v1.0.3-alpha / bugfix 20260424, the `hour` field is
// 0..23 in the **caller's local timezone** (per the `?tz=<IANA>`
// query param; defaults to UTC when absent). Clients should render
// `hour` verbatim — no further local conversion on the client side
// (double-converting was an early-round bug; see overview's prior
// chart that mapped UTC hour 4 onto the user's local hour slot,
// producing a 4 AM peak for a local-noon request).
type HourlyPoint struct {
	Hour         int   `json:"hour"`          // 0..23 in the caller's local tz
	TotalTokens  int64 `json:"total_tokens"`
	RequestCount int64 `json:"request_count"`
}

// ProtocolTimelinePoint adds provider dimension to a timeline point.
// JSON field is "protocol_type" for backward compatibility, but the value
// is provider_code (e.g. "kimi_code", "moonshot", "anthropic") not the wire protocol.
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
//
// Priority for display labels on the client (see web/src/pages/user/
// usage-ledger): `Alias` (personal/team BYOK) → `Identity` (OAuth email)
// → stripped `VirtualKeyID`. Identity was added 2026-04-22 so OAuth
// sessions stop surfacing as raw `session_<hex>` in the "Usage by Key"
// chart.
type KeyTotal struct {
	VirtualKeyID             string `json:"virtual_key_id"`
	Alias                    string `json:"alias,omitempty"`    // human-readable key alias (personal/team BYOK)
	Identity                 string `json:"identity,omitempty"` // email / display_identity (OAuth sessions)
	InputTokens              int64  `json:"input_tokens"`                // Anthropic: total prompt input (incl. cache_read + cache_creation)
	CachedInputTokens        int64  `json:"cached_input_tokens"`         // = Anthropic cache_read_input_tokens (legacy column name)
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"` // Anthropic cache_creation_input_tokens
	OutputTokens             int64  `json:"output_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	RequestCount             int64  `json:"request_count"`
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
	AccountID string    // used when SeatID is empty (personal key users without org seat)
	StartDate time.Time // inclusive; interpreted in the user's local TZ
	EndDate   time.Time // inclusive; interpreted in the user's local TZ
	Limit     int       // for ranking, default 50

	// TZ is the IANA name (e.g. "Asia/Shanghai") of the caller's
	// local time zone. Empty = UTC. Used to bucket per-day / per-hour
	// aggregates so a user in +08:00 sees their 12:00 request at
	// hour 12, not UTC hour 4. See bugfix 20260424 tz-local round.
	TZ string

	// TZLocation is the parsed *time.Location for TZ. Populated by
	// Defaults(); repo code uses this to compute local day boundaries.
	TZLocation *time.Location

	// TZOffsetMs is the current offset (UTC→local) in milliseconds at
	// the query window start. Used by the SQLite query path which
	// cannot load IANA databases. For Postgres repo code ignores this
	// and uses the IANA name directly via AT TIME ZONE.
	TZOffsetMs int64
}

// Defaults fills in zero-value defaults.
func (q *QueryParams) Defaults() {
	if q.TZ == "" {
		q.TZ = "UTC"
	}
	if q.TZLocation == nil {
		loc, err := time.LoadLocation(q.TZ)
		if err != nil {
			// Don't silently drop bad input — a typo like
			// "?tz=Asia/Shangai" (missing 'h') would otherwise render
			// a correct-looking UTC chart, hiding the client's bug.
			slog.Warn("query-service: unknown IANA TZ, falling back to UTC",
				"tz", q.TZ, "error", err)
			loc = time.UTC
			q.TZ = "UTC"
		}
		q.TZLocation = loc
	}
	if q.TZOffsetMs == 0 && q.TZ != "UTC" {
		// Offset at the **query window's start**, not "now". For a
		// January query run in April from America/New_York, now() is
		// EDT (-04:00) but January was EST (-05:00) — using now()
		// would mis-bucket the entire historical window by one hour,
		// not just the DST transition day. Computing via StartDate.In(loc)
		// picks the offset that applies to most of the window; the
		// remaining DST-crossing-within-window edge stays bounded to
		// the single transition day. Postgres isn't affected (uses
		// AT TIME ZONE <iana> per-row). See code review finding #2
		// on bugfix 20260424.
		anchor := q.StartDate
		if anchor.IsZero() {
			anchor = time.Now()
		}
		_, offsetSec := anchor.In(q.TZLocation).Zone()
		q.TZOffsetMs = int64(offsetSec) * 1000
	}
	// Re-interpret StartDate / EndDate as local midnight: the parser
	// read YYYY-MM-DD naïve (UTC), but the client meant "my local
	// calendar day". After this step, StartDate/EndDate are the
	// instant of local midnight so SQL range filters on event_time
	// millis compare against the correct UTC window.
	if !q.StartDate.IsZero() {
		q.StartDate = toLocalMidnight(q.StartDate, q.TZLocation)
	}
	if !q.EndDate.IsZero() {
		q.EndDate = toLocalMidnight(q.EndDate, q.TZLocation)
	}
	if q.EndDate.IsZero() {
		// "Today" in the user's local zone.
		now := time.Now().In(q.TZLocation)
		q.EndDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, q.TZLocation)
	}
	if q.StartDate.IsZero() {
		q.StartDate = q.EndDate.AddDate(0, 0, -30)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
}

// toLocalMidnight takes a naïve date (e.g. parsed from "YYYY-MM-DD" as
// UTC midnight) and returns the same calendar day at midnight in loc.
func toLocalMidnight(d time.Time, loc *time.Location) time.Time {
	y, m, day := d.Date()
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

// LocalWindowMs returns the half-open UTC-millis range [start, end)
// that covers the caller's [StartDate, EndDate] calendar window —
// `end` is the instant at local midnight of EndDate + 1 day so
// "2026-04-24 through 2026-04-24" is a single 24h (or 23/25h on DST
// boundaries) window. Repo code passes both to event_time range
// filters.
//
// StartDate / EndDate are already local-midnight after Defaults()
// shifts them into TZLocation, so UnixMilli() yields the correct UTC
// instant.
func (q QueryParams) LocalWindowMs() (startMs, endMs aikeytime.Millis) {
	startMs = aikeytime.FromTime(q.StartDate)
	endMs = aikeytime.FromTime(q.EndDate.AddDate(0, 0, 1))
	return
}
