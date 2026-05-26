// Package ingest handles usage event ingestion and ODS persistence.
package ingest

import (
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// SupportedSchemaVersions lists the event schema versions this collector can process.
// Exposed via GET /version so proxy can check compatibility.
// When the event schema evolves (new fields, changed semantics), bump MaxSchemaVersion
// and add backward-compatible handling in ingestOne().
var SupportedSchemaVersions = []int{1}

// MaxSchemaVersion is the highest schema version this collector understands.
const MaxSchemaVersion = 1

// UsageEvent represents a single usage event reported by Local Proxy.
type UsageEvent struct {
	// identifiers
	EventID         string `json:"event_id"`
	RequestID       string `json:"request_id,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
	ProxyInstanceID string `json:"proxy_instance_id,omitempty"`
	DeviceID        string `json:"device_id,omitempty"`

	// schema + source metadata
	SchemaVersion        int    `json:"schema_version"`
	SourceVersion        string `json:"source_version,omitempty"`
	ClientVersion        string `json:"client_version,omitempty"`
	ProxyConfigVersion   string `json:"proxy_config_version,omitempty"`
	ProxyLoadedControlSeq *int64 `json:"proxy_loaded_control_seq,omitempty"`

	// timestamps: int64 Unix epoch milliseconds (UTC). Wire format switched
	// from RFC3339 in v1.0.3-alpha — see design doc
	// roadmap20260320/技术实现/update/20260424-时间戳统一为int64毫秒-data-service.md.
	// event_time is the authoritative proxy-reported instant (D4).
	EventTime  aikeytime.Millis  `json:"event_time"`
	OccurredAt aikeytime.Millis  `json:"occurred_at"`
	StartedAt  *aikeytime.Millis `json:"started_at,omitempty"`
	FinishedAt *aikeytime.Millis `json:"finished_at,omitempty"`

	// ownership
	OrgID                 string `json:"org_id"`
	AccountID             string `json:"account_id,omitempty"`
	SeatID                string `json:"seat_id,omitempty"`
	AccountStatusSnapshot string `json:"account_status_snapshot,omitempty"`

	// routing
	VirtualKeyID               string `json:"virtual_key_id,omitempty"`
	VirtualKeyRevision         string `json:"virtual_key_revision,omitempty"`
	VirtualKeyHash             string `json:"virtual_key_hash,omitempty"`
	BindingID                  string `json:"binding_id,omitempty"`
	CredentialID               string `json:"credential_id,omitempty"`
	CredentialRevision         string `json:"credential_revision,omitempty"`
	RealKeyHash                string `json:"real_key_hash,omitempty"`
	CredentialFingerprint      string `json:"credential_fingerprint,omitempty"`
	ProviderAccountFingerprint string `json:"provider_account_fingerprint,omitempty"`

	// provider / protocol
	ProviderID   string `json:"provider_id,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
	ProtocolType string `json:"protocol_type,omitempty"`
	RouteSource    string `json:"route_source,omitempty"`
	OAuthIdentity  string `json:"oauth_identity,omitempty"` // Email/display name for OAuth accounts

	// app — Phase 4 Connected Apps (v1.0.0-rc.5). Proxy attaches the
	// registered app slug (e.g. "degrade-detector") to events that
	// flowed through `/apps/<slug>/v1/...` so query-service can scope
	// `WHERE app_slug = ?` for the per-app dashboard. Empty for events
	// without an app context (CLI direct calls, virtual keys, etc).
	AppSlug string `json:"app_slug,omitempty"`

	// session — Performance dashboard session dimension (v1.0.0-rc.6).
	// Proxy extracts via aikey-proxy/internal/proxy/sessionid/
	// fingerprint.yaml (Claude Code header / Kimi prompt_cache_key /
	// OpenAI conversation_id / X-Aikey-Session-Id convention). Empty
	// for events without a session marker. Used by the per-session
	// usage chart and as an optional filter on by-key / by-model.
	SessionID string `json:"session_id,omitempty"`

	// usage — Anthropic prompt-caching tuple (input / cache_creation /
	// cache_read / output). For non-Anthropic providers the cache fields
	// are nil and serialise out as `omitempty`.
	//
	// Wire alignment (2026-04-29): `CachedInputTokens` Go field receives
	// the wire field `cache_read_input_tokens` (Anthropic's name, what
	// the proxy actually emits). The DB column it's persisted to is
	// `cached_input_tokens` (legacy storage name pre-dating Anthropic's
	// split). The struct field name keeps `Cached*` for affinity with
	// the column it writes; the JSON tag bridges to the canonical
	// upstream name. Renaming the column to `cache_read_input_tokens`
	// for total consistency is deferred to the next baseline
	// consolidation — see v1_0_5_alpha.go for the rationale.
	Model                    string  `json:"model,omitempty"`
	RequestCount             int     `json:"request_count"`
	InputTokens              *int64  `json:"input_tokens,omitempty"`
	OutputTokens             *int64  `json:"output_tokens,omitempty"`
	CachedInputTokens        *int64  `json:"cache_read_input_tokens,omitempty"`     // Anthropic cache_read_input_tokens → DB col cached_input_tokens
	CacheCreationInputTokens *int64  `json:"cache_creation_input_tokens,omitempty"` // Anthropic cache_creation_input_tokens
	ReasoningTokens          *int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens              *int64  `json:"total_tokens,omitempty"`
	BillableAmount           *string `json:"billable_amount,omitempty"` // NUMERIC as string
	Currency                 string  `json:"currency,omitempty"`

	// result
	RequestStatus     string `json:"request_status"`
	HTTPStatusCode    *int   `json:"http_status_code,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`

	// raw payload
	RawUsageJSON   any `json:"raw_usage_json,omitempty"`
	RawHeadersJSON any `json:"raw_headers_json,omitempty"`
	ExtJSON        any `json:"ext_json,omitempty"`
}

// BatchRequest is the ingest API request body.
type BatchRequest struct {
	Source          string       `json:"source"`
	SourceVersion   string       `json:"source_version"`
	ProxyInstanceID string       `json:"proxy_instance_id"`
	Events          []UsageEvent `json:"events"`
}

// BatchResponse is the ingest API response body.
type BatchResponse struct {
	Accepted   int `json:"accepted"`
	Duplicated int `json:"duplicated"`
	Rejected   int `json:"rejected"`
}

// EventResult tracks per-event ingest outcome.
type EventResult struct {
	EventID string
	Status  string // "accepted", "duplicated", "rejected"
	Reason  string
}
