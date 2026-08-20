// Package conversation implements collector-side ingestion + storage of
// employee AI conversation records for the enterprise Conversation Audit
// feature (design: roadmap20260320/技术实现/阶段6-企业定制/20260616-企业对话审计-*.md).
//
// It is deliberately a SEPARATE package from internal/ingest (usage events):
// the two streams have independent WALs, independent source_seq spaces, and
// independent watermark/known-loss tables. Sharing the package would couple the
// financial-grade usage path to content churn. The wire/processing SHAPE mirrors
// internal/ingest (BatchRequest → per-record ingestOne → post-batch
// AdvanceWatermark → ContiguousSeq map) so the two read the same to maintainers.
//
// This file holds only the wire/domain types (no DB/HTTP deps) so it compiles
// standalone; repo + service + handler build on it.
package conversation

import "github.com/AiKeyLabs/pkg/aikeytime"

// MaxBatchSize bounds one upload batch (mirrors ingest.maxBatchSize).
const MaxBatchSize = 500

// ConversationRecord is one QA turn uploaded by the proxy's content channel.
//
// Increment-storage contract: user_text is ONLY this turn's new user message
// (proxy's extractLatestUserContent), assistant_text is this turn's reply — NOT
// the whole resent history. system_text is sent on every turn but stored once
// per session (collector first-wins upsert into conversation_sessions), so the
// big system prompt is not duplicated and the stateless proxy needn't know
// "is this the first turn".
//
// stamp-once: CreatedAt = request_started_at, stamped by the proxy when the
// request ARRIVES (not at completion) and carried verbatim across WAL retransmit.
// conv_date (the PG partition key) is derived server-side as date(CreatedAt) in
// UTC — deterministic from the stamped CreatedAt, so (event_id, conv_date) is a
// stable 1:1 dedup key. DurationMs carries completion latency separately.
type ConversationRecord struct {
	EventID        string `json:"event_id"`         // = the turn's usage event_id; idempotency key
	OrgID          string `json:"org_id"`           // tenant; all queries scoped by it
	SessionID      string `json:"session_id"`       // client session id; NOT globally unique → always read with org+owner
	OwnerAccountID string `json:"owner_account_id"` // VK owner — proxy-verified attribution; collector validates ∈ org, NEVER overwritten by JWT
	// SeatID: the org seat of the HUMAN at the terminal (route.SeatID, same
	// field usage events carry). Added 2026-07-07 because owner_account_id is
	// the VK OWNER — for shared-pool VKs that's the pool creator, and audit
	// views keyed on it filed employee turns under a stranger seat. Empty for
	// legacy proxies / personal keys; query views fall back to owner.
	SeatID       string `json:"seat_id,omitempty"`
	VirtualKeyID string `json:"virtual_key_id,omitempty"`

	// Delivery integrity (mirrors usage): SourceSeq is a pointer so "absent"
	// (older proxy) is distinguishable from 0. SourceID identifies the proxy/
	// vault source; the per-source contiguous watermark drives WAL pruning.
	SourceID  string `json:"source_id,omitempty"`
	SourceSeq *int64 `json:"source_seq,omitempty"`

	Model        string `json:"model,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`

	// Content. SystemText is per-session (deduped to conversation_sessions);
	// UserText/AssistantText are per-turn (stored on conversation_records).
	UserText      string `json:"user_text,omitempty"`
	AssistantText string `json:"assistant_text,omitempty"`
	SystemText    string `json:"system_text,omitempty"`

	// Token snapshot (display only; usage_fact_dwd stays the billing authority).
	// Pointers so an absent field is NULL, not 0. Names match usage columns.
	InputTokens              *int64 `json:"input_tokens,omitempty"`
	OutputTokens             *int64 `json:"output_tokens,omitempty"`
	CachedInputTokens        *int64 `json:"cached_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	ReasoningTokens          *int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens              *int64 `json:"total_tokens,omitempty"`
	// CacheEnabled (decision B): did the client request prompt caching this turn?
	// 1=on / 0=off / NULL=unknown. A display switch distinct from the cache COUNTS
	// above (which read 0 both when caching was off AND when requested-but-ineffective).
	CacheEnabled *int64 `json:"cache_enabled,omitempty"`

	DurationMs    *int64 `json:"duration_ms,omitempty"`   // request→response latency
	RequestStatus string `json:"request_status"`          // ok | partial | error
	ContentBytes  *int64 `json:"content_bytes,omitempty"` // captured size (post-cap)

	CreatedAt aikeytime.Millis `json:"created_at"` // = request_started_at (epoch ms), stamp-once
}

// ConversationBatchRequest is one upload from a proxy's content reporter.
// AllocatedSeq is the client's source_seq allocator high-water (reserve-ahead),
// used to advance the watermark / detect tail gaps — same role as the usage
// batch's allocated_seq. Pointer so a v1/older proxy omitting it reads as "no
// info" (watermark leaves client_allocated_seq unchanged).
type ConversationBatchRequest struct {
	Source          string               `json:"source"`
	SourceVersion   string               `json:"source_version,omitempty"`
	ProxyInstanceID string               `json:"proxy_instance_id,omitempty"`
	Records         []ConversationRecord `json:"records"`
	AllocatedSeq    *int64               `json:"allocated_seq,omitempty"`
}

// ConversationBatchResponse is the ingest result. ContiguousSeq maps source_id →
// the per-source contiguous high-water the proxy may safely prune its WAL up to.
// Omitted (nil) for batches that carried no v2 source identity (old-client safe).
type ConversationBatchResponse struct {
	Accepted      int              `json:"accepted"`
	Duplicated    int              `json:"duplicated"`
	Quarantined   int              `json:"quarantined"`
	Rejected      int              `json:"rejected"`
	ContiguousSeq map[string]int64 `json:"contiguous_seq,omitempty"`
}

// RecordResult is the per-record disposition (parallels ingest.EventResult).
//   - accepted    : stored fresh
//   - duplicated  : (event_id, conv_date) already present — its seq is accounted
//   - quarantined : stored but flagged — same (source_id, source_seq) already
//     maps to a DIFFERENT event_id (pollution; ingest_status='quarantined').
//     Its seq DID arrive (no gap), so it still counts for watermark advance.
//   - rejected    : validation failed / owner not ∈ org / internal error — NOT stored
type RecordResult struct {
	EventID string
	Status  string
	Reason  string
	// transient marks a rejection caused by a TransientStorageError — the
	// handler converts any such batch to a 503 so the proxy re-sends (P0-4
	// A-fix). Never serialized.
	transient bool `json:"-"`
}
