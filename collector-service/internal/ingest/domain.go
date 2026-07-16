// Package ingest handles usage event ingestion and ODS persistence.
package ingest

import (
	"github.com/AiKeyLabs/pkg/aikeytime"
)

// SupportedSchemaVersions lists the event schema versions this collector can process.
// Exposed via GET /version so proxy can check compatibility.
// When the event schema evolves (new fields, changed semantics), bump MaxSchemaVersion
// and add backward-compatible handling in ingestOne().
//
// v2 (delivery integrity, 2026-05-30): adds source_id + source_seq — a
// per-source (= one vault) monotonic, never-reused sequence enabling the
// server to prove completeness and pinpoint missing events. v1 events (no
// source_seq) are still accepted and stored; they simply don't participate
// in gap detection (their source_seq is NULL). See design doc
// roadmap20260320/技术实现/阶段6-企业定制/20260530-财务对账级用量审计-完整技术方案.md.
var SupportedSchemaVersions = []int{1, 2}

// MaxSchemaVersion is the highest schema version this collector understands.
const MaxSchemaVersion = 2

// UsageEvent represents a single usage event reported by Local Proxy.
type UsageEvent struct {
	// identifiers
	EventID         string `json:"event_id"`
	RequestID       string `json:"request_id,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
	ProxyInstanceID string `json:"proxy_instance_id,omitempty"`
	DeviceID        string `json:"device_id,omitempty"`

	// delivery integrity (schema v2, 2026-05-30). SourceID identifies the
	// client source = one vault (the proxy carries it in the device_id wire
	// field, sourced from vault config runtime.source_identity — note it is a
	// vault-scoped install identity, NOT a hardware fingerprint). SourceSeq is
	// that source's monotonic, never-reused sequence number. Together they let
	// the server detect gaps (missing seq) and prove completeness. Nil/empty on
	// v1 events from older proxies — such events skip gap tracking but are
	// still stored. SourceSeq is a pointer so "absent" (v1) is distinguishable
	// from a legitimate 0.
	SourceID  string `json:"source_id,omitempty"`
	SourceSeq *int64 `json:"source_seq,omitempty"`

	// ContentHash (schema v2, stage C) is the client-stamped tamper/corruption
	// fingerprint "sha256:<scheme>:<hex>" over the metering tuple (pkg/usagehash).
	// The ingest service RECOMPUTES it from this event's fields and compares: a
	// mismatch quarantines the event (ingest_status='quarantined') instead of
	// billing a silently-corrupted value. Empty (older proxy / pre-C) → validation
	// skipped, event accepted normally. See validateContentHash in service.go.
	ContentHash string `json:"content_hash,omitempty"`

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
	// KeyLabel — user-friendly name for the credential the proxy resolved
	// (account-scoped alias for personal, OAuth email, team-side
	// `key-<id>-<alias>` for team keys, app slug for app routes, etc.).
	// Wire field: `key_label`. Persisted to ODS column `virtual_key_alias`
	// (naming matches DWD where it's been carried since rc.5 baseline).
	// v1.0.0-rc.11 closed the gap where this field was silently dropped
	// at ingest — the proxy had been emitting it for months but the
	// collector struct didn't carry it, so DWD's `virtual_key_alias` was
	// permanently empty and the team-key row in usage-ledger fell back
	// to rendering the bare vk_id UUID.
	KeyLabel                   string `json:"key_label,omitempty"`
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

	// Cost-pricing audit (v1.0.0-rc.8): upstream region + endpoint reported by
	// proxy, persisted to ODS then projected to DWD for cost auditing
	// (Bedrock/Vertex price by region; endpoint for forensics).
	Region      string `json:"region,omitempty"`
	EndpointURL string `json:"endpoint_url,omitempty"`

	// RequestPath (2026-07-15 非生成流量不进用量审计): the inbound request's URL
	// path as reported by the proxy. MUST be declared here even though no ODS
	// column exists for it: raw_event_json is json.Marshal of THIS struct (not
	// the verbatim wire bytes — see service.go), so an undeclared wire field is
	// silently dropped before storage. The projector json-extracts it from
	// raw_event_json to classify generation vs non-generation scope.
	RequestPath string `json:"request_path,omitempty"`

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

	// AllocatedSeq is the client's per-source allocator high-water mark — the
	// highest source_seq the proxy has ever handed out (reserve-ahead), which
	// may sit ABOVE the highest seq actually delivered when events are still
	// in-flight or were lost before WAL append. Recorded server-side as
	// client_allocated_seq so tail-gap detection can fire: client_allocated >
	// contiguous past a timeout ⇒ the (contiguous, client_allocated] span is a
	// suspected tail loss. nil/omitted on v1 / older proxies → server leaves
	// client_allocated_seq untouched (no tail detection, but no false positive).
	// One proxy = one source identity, so this single scalar applies to every
	// (org,source) pair the batch touches. Added v1.0.0-rc.7 (B').
	AllocatedSeq *int64 `json:"allocated_seq,omitempty"`
}

// BatchResponse is the ingest API response body.
//
// ContiguousSeq (delivery integrity, 2026-05-30): per-source map of
// source_id → the server's connected-no-gap high-water mark after this batch.
// The client advances its WAL-cleanup cursor to this value and prunes rotated
// WAL files whose max seq ≤ contiguous. It is a POINTER MAP entry semantic on
// the wire (omitempty): an older collector omits the field entirely, so a new
// client MUST treat "field absent" as "don't advance / don't prune" (conserve
// — never lose), distinct from "present with value 0". One map (not a scalar)
// because a batch may legitimately mix multiple sources (rare, but a proxy
// generation swap or shared collector can interleave). Keyed by source_id;
// only sources present in THIS batch appear.
type BatchResponse struct {
	Accepted   int `json:"accepted"`
	Duplicated int `json:"duplicated"`
	Rejected   int `json:"rejected"`
	// Quarantined (stage C): events stored but flagged content_hash-mismatch —
	// NOT billed, held for review. omitempty so pre-C batches don't carry it.
	Quarantined   int              `json:"quarantined,omitempty"`
	ContiguousSeq map[string]int64 `json:"contiguous_seq,omitempty"`
}

// EventResult tracks per-event ingest outcome.
type EventResult struct {
	EventID string
	Status  string // "accepted", "duplicated", "rejected"
	Reason  string
}
