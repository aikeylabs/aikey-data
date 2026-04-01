// Package ingest handles usage event ingestion and ODS persistence.
package ingest

import (
	"time"
)

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

	// timestamps — event_time is the authoritative local client time (D4)
	EventTime  time.Time  `json:"event_time"`
	OccurredAt time.Time  `json:"occurred_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

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
	RouteSource  string `json:"route_source,omitempty"`

	// usage
	Model             string  `json:"model,omitempty"`
	RequestCount      int     `json:"request_count"`
	InputTokens       *int64  `json:"input_tokens,omitempty"`
	OutputTokens      *int64  `json:"output_tokens,omitempty"`
	CachedInputTokens *int64  `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   *int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens       *int64  `json:"total_tokens,omitempty"`
	BillableAmount    *string `json:"billable_amount,omitempty"` // NUMERIC as string
	Currency          string  `json:"currency,omitempty"`

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
