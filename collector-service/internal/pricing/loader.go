package pricing

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Load builds the process-wide Resolver from the embedded pricing files. Called
// once at collector startup. On ANY parse failure it returns an error; the
// caller MUST fail startup (panic/exit) rather than continue with an empty
// table — an empty table would silently mark every event unpriced, hiding a
// real misconfiguration (design §2.3 fail-fast).
func Load() (*Resolver, error) {
	return load(litellmJSON, historyJSON, overridesJSON)
}

// load is the testable core of Load: identical logic over explicit bytes.
func load(litellmBytes, historyBytes, overridesBytes []byte) (*Resolver, error) {
	litellm, err := parseLiteLLM(litellmBytes)
	if err != nil {
		return nil, fmt.Errorf("pricing: parse litellm: %w", err)
	}
	history, err := parseOverlayHistory(historyBytes)
	if err != nil {
		return nil, fmt.Errorf("pricing: parse history: %w", err)
	}
	overrides, err := parseOverlayOverrides(overridesBytes)
	if err != nil {
		return nil, fmt.Errorf("pricing: parse overrides: %w", err)
	}
	snap := BuildSnapshot(litellmBytes, historyBytes, overridesBytes)
	return newResolver(litellm, history, overrides, snap), nil
}

// litellmEntry mirrors one model object in LiteLLM's
// model_prices_and_context_window.json. Unknown fields are ignored, so upstream
// adding fields won't break parsing (design §2.6 lax unmarshal).
type litellmEntry struct {
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	InputCostAbove128k          float64 `json:"input_cost_per_token_above_128k_tokens"`
	OutputCostAbove128k         float64 `json:"output_cost_per_token_above_128k_tokens"`
	LiteLLMProvider             string  `json:"litellm_provider"`
}

func parseLiteLLM(b []byte) (map[modelKey]UnitPrices, error) {
	var raw map[string]litellmEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make(map[modelKey]UnitPrices, len(raw))
	for model, e := range raw {
		// Entries without a provider (e.g. the _meta block, or upstream's
		// "sample_spec") are not real models — skip them.
		if e.LiteLLMProvider == "" {
			continue
		}
		out[modelKey{provider: e.LiteLLMProvider, model: model}] = UnitPrices{
			InputPerToken:              e.InputCostPerToken,
			OutputPerToken:             e.OutputCostPerToken,
			CacheCreationInputPerToken: e.CacheCreationInputTokenCost,
			CacheReadInputPerToken:     e.CacheReadInputTokenCost,
			// LiteLLM has no separate reasoning rate; reasoning tokens bill at
			// the output rate unless an overlay overrides it.
			ReasoningPerToken:       e.OutputCostPerToken,
			InputPerTokenAbove128k:  e.InputCostAbove128k,
			OutputPerTokenAbove128k: e.OutputCostAbove128k,
		}
	}
	return out, nil
}

// overlayFile is the shape of pricing-history.json / pricing-overrides.json.
type overlayFile struct {
	SchemaVersion int            `json:"schema_version"`
	Entries       []overlayEntry `json:"entries"`
}

type overlayEntry struct {
	OrgID         string  `json:"org_id"`         // overrides only
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	EffectiveFrom int64   `json:"effective_from"` // history only, unix ms
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheCreation float64 `json:"cache_creation"`
	CacheRead     float64 `json:"cache_read"`
	Reasoning     float64 `json:"reasoning"` // optional; defaults to output
}

func (e overlayEntry) toUnitPrices() UnitPrices {
	reasoning := e.Reasoning
	if reasoning == 0 {
		reasoning = e.Output
	}
	return UnitPrices{
		InputPerToken:              e.Input,
		OutputPerToken:             e.Output,
		CacheCreationInputPerToken: e.CacheCreation,
		CacheReadInputPerToken:     e.CacheRead,
		ReasoningPerToken:          reasoning,
	}
}

func parseOverlayHistory(b []byte) (map[modelKey][]historyEntry, error) {
	var f overlayFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	out := map[modelKey][]historyEntry{}
	for _, e := range f.Entries {
		k := modelKey{provider: e.Provider, model: e.Model}
		out[k] = append(out[k], historyEntry{effectiveFrom: e.EffectiveFrom, prices: e.toUnitPrices()})
	}
	// Resolver expects each model's entries sorted by effectiveFrom DESC.
	for k := range out {
		entries := out[k]
		sort.Slice(entries, func(i, j int) bool { return entries[i].effectiveFrom > entries[j].effectiveFrom })
	}
	return out, nil
}

func parseOverlayOverrides(b []byte) (map[overrideKey]UnitPrices, error) {
	var f overlayFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	out := map[overrideKey]UnitPrices{}
	for _, e := range f.Entries {
		out[overrideKey{orgID: e.OrgID, provider: e.Provider, model: e.Model}] = e.toUnitPrices()
	}
	return out, nil
}
