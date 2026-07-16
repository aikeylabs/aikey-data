// Package projector handles ODS → DWD projection with control-plane enrichment.
package projector

import (
	"database/sql"

	"github.com/AiKeyLabs/pkg/aikeytime"
)

// QualityStatus indicates data completeness of a DWD fact.
type QualityStatus string

const (
	QualityExact                    QualityStatus = "exact"
	QualityCompletedFromControlEvent QualityStatus = "completed_from_control_event"
	QualityPartial                  QualityStatus = "partial"
	QualityInvalid                  QualityStatus = "invalid"
)

// AnomalyType classifies anomalies (D8: MVP only valid / late_report / pending_review).
type AnomalyType string

const (
	AnomalyNone                 AnomalyType = ""
	AnomalyLateReportAbnormal   AnomalyType = "late_report_abnormal_charge"
	AnomalyPendingReview        AnomalyType = "pending_review"
)

// BillingScope determines who is billed.
type BillingScope string

const (
	BillOrgAndUser BillingScope = "org_and_user"
	BillOrgOnly    BillingScope = "org_only"
	BillHoldReview BillingScope = "hold_for_review"
)

// UserUsageScope determines if the event appears in user stats.
type UserUsageScope string

const (
	UsageScopeNormal   UserUsageScope = "normal"
	UsageScopeExcluded UserUsageScope = "excluded"
	UsageScopeAbnormal UserUsageScope = "abnormal"
	// UsageScopeNonGeneration (2026-07-15 非生成流量不进用量审计): the request
	// was a non-generation call — health polls, model-list fetches
	// (GET /v1/models), and similar client-side probing that produces no
	// tokens. Deliberately a NEW value, not a reuse of "excluded": excluded
	// means "ownership unverifiable" (pending review) and those rows MUST
	// stay visible on the usage-audit page, while non_generation rows are
	// filtered from audit + stats. Classified by classifyGenerationScope.
	UsageScopeNonGeneration UserUsageScope = "non_generation"
)

// ODSRecord represents a row read from usage_event_ods for projection.
type ODSRecord struct {
	OdsID                      int64
	EventID                    string
	EventTime                  aikeytime.Millis
	OccurredAt                 aikeytime.Millis

	OrgID                      string
	AccountID                  sql.NullString
	SeatID                     sql.NullString
	AccountStatusSnapshot      sql.NullString

	VirtualKeyID               sql.NullString
	VirtualKeyRevision         sql.NullString
	VirtualKeyHash             sql.NullString
	// VirtualKeyAlias — v1.0.0-rc.11. Populated at ingest from the
	// wire `key_label` field; the enricher copies it verbatim into
	// DWD's `virtual_key_alias` so usage-ledger renders the friendly
	// name instead of the raw vk_id UUID (the gap that made team-key
	// rows in the by-key chart appear as `5f9758a2-...` before this
	// column was carried through).
	VirtualKeyAlias            sql.NullString
	BindingID                  sql.NullString
	CredentialID               sql.NullString
	CredentialRevision         sql.NullString
	RealKeyHash                sql.NullString
	CredentialFingerprint      sql.NullString
	ProviderAccountFingerprint sql.NullString

	ProviderID                 sql.NullString
	ProviderCode               sql.NullString
	ProtocolType               sql.NullString
	RouteSource                sql.NullString

	Model                      sql.NullString
	RequestCount               int
	InputTokens                sql.NullInt64
	OutputTokens               sql.NullInt64
	CachedInputTokens          sql.NullInt64 // = Anthropic cache_read_input_tokens
	CacheCreationInputTokens   sql.NullInt64 // Anthropic cache_creation_input_tokens (1.25× billing)
	ReasoningTokens            sql.NullInt64
	TotalTokens                sql.NullInt64
	BillableAmount             sql.NullString
	Currency                   sql.NullString

	RequestStatus              string
	HTTPStatusCode             sql.NullInt32
	UpstreamRequestID          sql.NullString

	DwdRetryCount              int

	// Phase 4 Connected Apps (v1.0.0-rc.5): per-app scoping.
	AppSlug                    sql.NullString

	// Performance dashboard session dimension (v1.0.0-rc.6).
	// Generic per-conversation aggregation key, populated by proxy
	// via aikey-proxy/internal/proxy/sessionid/fingerprint.yaml
	// extraction (Claude Code header, Kimi prompt_cache_key body,
	// OpenAI conversation_id body, or the X-Aikey-Session-Id
	// convention header for any client). NULL when the request
	// carried no session marker.
	SessionID                  sql.NullString

	// Cost-pricing audit (v1.0.0-rc.8): upstream region + endpoint reported by
	// the proxy. Copied verbatim to the DWD for cost auditing (Bedrock/Vertex
	// price by region; endpoint for forensics).
	Region                     sql.NullString
	EndpointURL                sql.NullString

	// OAuth identity (v1.0.1-alpha.1): email behind an OAuth-direct route.
	// Already on ODS; carried into DWD so the read model can group/filter by
	// identity WITHOUT joining back to ODS (CQRS — reads stay on the read model).
	OAuthIdentity              sql.NullString

	// Delivery-integrity columns (v1.0.1-alpha.3): on ODS since rc.7, now also
	// projected into DWD so the enterprise usage-audit export can carry
	// tamper/gap evidence without joining ODS. ContentHash = sha256 over the
	// metering tuple (tamper evidence). SourceID = which client source (vault).
	// SourceSeq = per-source dense seq (a gap = a dropped event); nullable
	// because old-proxy events carry no seq.
	ContentHash                sql.NullString
	SourceID                   sql.NullString
	SourceSeq                  sql.NullInt64

	// RequestPath (2026-07-15 非生成流量不进用量审计): the inbound request's
	// URL path, extracted from raw_event_json (additive wire field — no ODS
	// column, no DDL). NULL for events from older proxies; the enricher then
	// leaves classification unchanged. Feeds the generation/non-generation
	// scope rule (see classifyGenerationScope).
	RequestPath                sql.NullString
}

// ControlEvent is a read-only projection of managed_key_control_events.
type ControlEvent struct {
	EventID            string
	OrgID              string
	AccountID          sql.NullString
	ChangeType         string
	EntityType         string
	SeatID             string
	VirtualKeyID       string
	VirtualKeyRevision string
	BindingID          sql.NullString
	CredentialID       string
	CredentialRevision string
	Revision           string // event-level monotonic counter
	ProviderID         string
	EffectiveFrom      aikeytime.Millis
	EffectiveTo        *aikeytime.Millis
	AfterSnapshotJSON  []byte
}

// DWDFact is a row to be written to usage_fact_dwd.
type DWDFact struct {
	EventID                    string
	OdsID                      int64
	OccurredAt                 aikeytime.Millis
	EventTime                  aikeytime.Millis
	UsageDate                  string // ISO date "YYYY-MM-DD" in UTC

	OrgID                      string
	AccountID                  string
	SeatID                     string

	VirtualKeyID               string
	VirtualKeyRevision         string
	VirtualKeyAlias            string
	VirtualKeyHash             string

	BindingID                  string
	BindingAlias               string

	CredentialID               string
	CredentialRevision         string
	RealKeyHash                string
	CredentialFingerprint      string
	ProviderAccountFingerprint string

	ProviderID                 string
	ProviderCode               string
	ProviderDisplayName        string
	ProtocolType               string

	RouteSource                string
	Model                      string

	RequestCount               int
	InputTokens                int64
	OutputTokens               int64
	CachedInputTokens          int64 // = Anthropic cache_read_input_tokens
	CacheCreationInputTokens   int64 // Anthropic cache_creation_input_tokens
	ReasoningTokens            int64
	TotalTokens                int64
	BillableAmount             *string
	Currency                   string

	RequestStatus              string
	HTTPStatusCode             *int
	UpstreamRequestID          string

	CompletionSource           string
	QualityStatus              QualityStatus
	ValidationCode             string
	ValidationMessage          string
	AnomalyType                AnomalyType
	AnomalyReason              string
	BillingScope               BillingScope
	UserUsageScope             UserUsageScope

	ControlEventID             string
	ControlEventRevision       string

	ProjectorVersion           string

	// Phase 4 Connected Apps (v1.0.0-rc.5): projected from ODS row.
	AppSlug                    string

	// Performance dashboard session dimension (v1.0.0-rc.6): projected
	// verbatim from ODS row. Empty string for events without a session
	// marker (proxy wrote SQL NULL or "" — both surface as "" here).
	SessionID                  string

	// Cost-pricing audit (v1.0.0-rc.8). BillableAmount/Currency above are now
	// projector-COMPUTED (was passthrough). These five add the audit trail so a
	// cost stays reconstructable after price files change (design §3.6):
	Region             string  // upstream region (passthrough from ODS)
	EndpointURL        string  // upstream base URL (passthrough from ODS)
	BillingPeriod      string  // 'YYYY-MM' monthly bucket derived from occurred_at
	UnitPricesSnapshot *string // JSON of the unit prices used; NULL when unpriced
	PricingSnapshotID  string  // which global pricing_snapshots state was active

	// OAuth identity (v1.0.1-alpha.1): projected verbatim from ODS so the read
	// model can group/filter by email without an ODS join. "" when not OAuth.
	OAuthIdentity      string

	// Delivery-integrity passthrough (v1.0.1-alpha.3): copied verbatim from ODS
	// for the usage-audit export. ContentHash/SourceID are "" when absent.
	// SourceSeq is *int64 (not int64) to preserve SQL NULL — old-proxy events
	// have no seq, and the audit export must distinguish "no seq" from "seq 0".
	ContentHash        string
	SourceID           string
	SourceSeq          *int64
}
