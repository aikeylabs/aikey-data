package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// PriceSummary is the tiny per-model unit-price table delivered to the edge
// (aikey-proxy) for LOCAL usd enforcement (design D-U8). It is deliberately NOT
// the full ~2700-row LiteLLM table: only the models a Claude-routing deployment
// actually needs, so it can ride the quota snapshot to every proxy.
//
// Why a summary instead of shipping the full table or letting the proxy price:
// the full table stays server-side as the SINGLE billing authority (D-U1); the
// proxy uses this summary only to estimate/enforce usd in the gap between server
// baselines (in-flight + offline), and every running total is later overwritten
// by the server's exact value on reconnect (D-U8 reconnect reconciliation). So
// the summary needs to be small, stable, and version-stamped — not exhaustive.
type PriceSummary struct {
	// Version fingerprints the extracted price set; it changes only when a
	// summarized model's prices change, so the edge can tell summaries apart and
	// detect staleness without diffing the whole map.
	Version string `json:"version"`
	// Models maps the bare model name (exactly as the proxy sees it on a request,
	// e.g. "claude-opus-4-8") to its per-token unit prices. Same JSON shape as the
	// per-event unit_prices_snapshot column, so every layer parses it identically.
	Models map[string]UnitPrices `json:"models"`
}

// BuildSummary extracts the edge price summary from the embedded LiteLLM table.
// Used by the gen-pricing-summary build tool; the produced JSON is embedded by
// aikey-control-master and inlined into the quota snapshot.
func BuildSummary() (*PriceSummary, error) { return buildSummaryFrom(litellmJSON) }

// buildSummaryFrom is the testable core: it keeps the claude + codex set,
// dropping everything else. Reuses parseLiteLLM so the summary applies the exact
// same field mapping the authoritative resolver does (including reasoning :=
// output default) — no second, drift-prone parser.
//
// Version is the sha256 prefix of the canonical (sorted-key) models JSON, so it
// is deterministic and only moves when a kept model's price moves.
func buildSummaryFrom(litellmBytes []byte) (*PriceSummary, error) {
	m, err := parseLiteLLM(litellmBytes)
	if err != nil {
		return nil, err
	}
	models := make(map[string]UnitPrices)
	for k, up := range m {
		// Claude pools route anthropic-direct claude-* models; Codex pools route
		// OpenAI gpt-5* / *codex* models. Price BOTH from the same authoritative
		// LiteLLM table (dropping bedrock/vertex/azure re-listings and every other
		// provider) so pooled codex usage is not silently unpriced (usd=0). Was
		// claude-only until 2026-07-06 (codex-into-pool); the hand-added codex rows
		// in aikey-control-master's committed summary were wiped on every regen —
		// generator is now the single source of truth for codex prices too.
		// Bugfix: workflow/CI/bugfix/2026-07-06-codex-pricing-summary-claude-only.md.
		//
		// gpt-5* (not just *codex*): the live Codex CLI actually sends plain
		// gpt-5.N-mini models, which the earlier narrow `contains "codex"` policy
		// under-priced (usd=0). Kept broad here (supersedes summaryKeepsModel, merged
		// 2026-07-06). Size stays well under the snapshot budget.
		keepClaude := k.provider == "anthropic" && strings.HasPrefix(k.model, "claude")
		keepCodex := k.provider == "openai" &&
			(strings.HasPrefix(k.model, "gpt-5") || strings.Contains(k.model, "codex"))
		if !keepClaude && !keepCodex {
			continue
		}
		up.Source = SourceLiteLLM
		models[k.model] = up
	}
	// encoding/json marshals map keys in sorted order, so this is deterministic.
	b, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &PriceSummary{Version: hex.EncodeToString(sum[:])[:16], Models: models}, nil
}
