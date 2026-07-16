package projector

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"testing"
)

// nsInt64 builds a valid sql.NullInt64 for test brevity.
func nsInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func nsStr(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func TestIsGenerationPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Generation shapes across route prefixes.
		{"/v1/messages", true},
		{"/anthropic/v1/messages", true},
		{"/apps/degrade-detector/v1/messages", true},
		{"/probe/my-alias/v1/messages", true},
		{"/v1/chat/completions", true},
		{"/openai/v1/chat/completions", true},
		{"/v1/completions", true}, // legacy text completions
		{"/v1/responses", true},
		{"/backend-api/codex/responses", true},
		{"/v1/Responses/", true}, // case + trailing slash tolerated
		// Non-generation shapes (the polluters).
		{"/v1/models", false},
		{"/openai/v1/models", false},
		{"/v1/models/gpt-4o", false},
		{"/v1/messages/count_tokens", false},
		{"/v1/responses/resp_abc123", false}, // Responses API single-object GET
		{"", false},
	}
	for _, c := range cases {
		if got := isGenerationPath(c.path); got != c.want {
			t.Errorf("isGenerationPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestApplyGenerationScope_RuleMatrix pins the four classification rules from
// update/20260715-非生成流量不进用量审计与统计.md.
func TestApplyGenerationScope_RuleMatrix(t *testing.T) {
	cases := []struct {
		name      string
		path      sql.NullString
		tokens    sql.NullInt64
		status    string
		wantScope UserUsageScope
	}{
		{
			// C 拍板: no backfill — legacy events without the wire field keep
			// their classification untouched.
			name: "legacy_no_path_unchanged", path: sql.NullString{},
			tokens: nsInt64(0), status: "success", wantScope: UsageScopeNormal,
		},
		{
			// The polluter: zero-token model-list poll.
			name: "models_poll_zero_tokens", path: nsStr("/openai/v1/models"),
			tokens: nsInt64(0), status: "success", wantScope: UsageScopeNonGeneration,
		},
		{
			// A 401 on a non-generation path is still probe traffic.
			name: "models_poll_401", path: nsStr("/openai/v1/models"),
			tokens: nsInt64(0), status: "error", wantScope: UsageScopeNonGeneration,
		},
		{
			// Failed GENERATION attempt (401 on /v1/messages) must stay
			// visible — audit shows failed generation.
			name: "failed_generation_stays_normal", path: nsStr("/v1/messages"),
			tokens: nsInt64(0), status: "error", wantScope: UsageScopeNormal,
		},
		{
			// Happy-path generation.
			name: "generation_with_tokens", path: nsStr("/v1/chat/completions"),
			tokens: nsInt64(1234), status: "success", wantScope: UsageScopeNormal,
		},
		{
			// Tokens are ground truth: unlisted path with tokens must NOT be
			// hidden (stale suffix table) — it stays normal and WARNs.
			name: "tokens_on_unlisted_path_stays_normal", path: nsStr("/v2/newfangled-generate"),
			tokens: nsInt64(50), status: "success", wantScope: UsageScopeNormal,
		},
		{
			// NULL total_tokens behaves like zero.
			name: "null_tokens_nongen_path", path: nsStr("/v1/models"),
			tokens: sql.NullInt64{}, status: "success", wantScope: UsageScopeNonGeneration,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &ODSRecord{
				EventID:       "evt-" + c.name,
				RequestPath:   c.path,
				TotalTokens:   c.tokens,
				RequestStatus: c.status,
			}
			fact := &DWDFact{UserUsageScope: UsageScopeNormal}
			applyGenerationScope(fact, rec)
			if fact.UserUsageScope != c.wantScope {
				t.Errorf("scope = %q, want %q", fact.UserUsageScope, c.wantScope)
			}
		})
	}
}

// TestApplyGenerationScope_PreservesOwnershipFields: the override only narrows
// UserUsageScope; ownership forensics (completion_source / anomaly) survive so
// a probe event that ALSO failed control-event lookup keeps its evidence.
func TestApplyGenerationScope_PreservesOwnershipFields(t *testing.T) {
	rec := &ODSRecord{
		EventID:     "evt-mixed",
		RequestPath: nsStr("/v1/models"),
		TotalTokens: nsInt64(0),
	}
	fact := &DWDFact{
		UserUsageScope:   UsageScopeExcluded,
		CompletionSource: "no_control_event",
		AnomalyType:      AnomalyPendingReview,
		AnomalyReason:    "no control event found for virtual_key_id at event_time",
	}
	applyGenerationScope(fact, rec)
	if fact.UserUsageScope != UsageScopeNonGeneration {
		t.Errorf("scope = %q, want non_generation", fact.UserUsageScope)
	}
	if fact.CompletionSource != "no_control_event" || fact.AnomalyType != AnomalyPendingReview {
		t.Errorf("ownership fields clobbered: %+v", fact)
	}
}

// TestApplyGenerationScope_WarnOnUnlistedGenerationPath asserts the WARN path
// (logging 规范: fallback branches must be loud, and the WARN itself must be
// tested). Captured via a scoped slog handler.
func TestApplyGenerationScope_WarnOnUnlistedGenerationPath(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	rec := &ODSRecord{
		EventID:     "evt-warn",
		RequestPath: nsStr("/v2/newfangled-generate"),
		TotalTokens: nsInt64(99),
	}
	fact := &DWDFact{UserUsageScope: UsageScopeNormal}
	applyGenerationScope(fact, rec)
	if !bytes.Contains(buf.Bytes(), []byte("projector.scope.unlisted_generation_path")) {
		t.Errorf("expected WARN projector.scope.unlisted_generation_path, log output: %s", buf.String())
	}
}

// TestEnrich_NonGenerationEndToEnd drives the REAL Enrich entrypoint (not the
// helper) with a vault-origin VK so no control reader is needed, pinning that
// the final override actually runs after the ownership chain.
func TestEnrich_NonGenerationEndToEnd(t *testing.T) {
	e := NewEnricher(nil, nil)
	rec := &ODSRecord{
		EventID:       "evt-e2e",
		OrgID:         "personal",
		VirtualKeyID:  nsStr("personal:my-key"),
		RequestPath:   nsStr("/openai/v1/models"),
		TotalTokens:   nsInt64(0),
		RequestStatus: "success",
		RequestCount:  1,
	}
	fact, err := e.Enrich(context.Background(), rec)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if fact.UserUsageScope != UsageScopeNonGeneration {
		t.Errorf("scope = %q, want non_generation", fact.UserUsageScope)
	}
	// Ownership classification from the vault-origin branch survives.
	if fact.CompletionSource != "personal_vault_key" {
		t.Errorf("completion_source = %q, want personal_vault_key", fact.CompletionSource)
	}
}
