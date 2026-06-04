package pricing

import "testing"

// fixture: a minimal LiteLLM-shaped table with anthropic claude models + noise
// (a bedrock re-listing, a non-claude anthropic-ish entry, and a different
// provider). Only the anthropic-direct claude-* entries must survive.
const litellmFixture = `{
  "claude-opus-4-8": {"litellm_provider":"anthropic","input_cost_per_token":5e-6,"output_cost_per_token":2.5e-5,"cache_read_input_token_cost":5e-7,"cache_creation_input_token_cost":6.25e-6},
  "claude-haiku-4-5": {"litellm_provider":"anthropic","input_cost_per_token":1e-6,"output_cost_per_token":5e-6,"cache_read_input_token_cost":1e-7,"cache_creation_input_token_cost":1.25e-6},
  "anthropic.claude-3-5-haiku-20241022-v1:0": {"litellm_provider":"bedrock","input_cost_per_token":8e-7,"output_cost_per_token":4e-6},
  "gpt-4o-2024-08-06": {"litellm_provider":"openai","input_cost_per_token":2.5e-6,"output_cost_per_token":1e-5},
  "sample_spec": {"input_cost_per_token":1.0}
}`

func TestBuildSummary_KeepsOnlyAnthropicClaude(t *testing.T) {
	s, err := buildSummaryFrom([]byte(litellmFixture))
	if err != nil {
		t.Fatalf("buildSummaryFrom: %v", err)
	}
	if len(s.Models) != 2 {
		t.Fatalf("want 2 models (anthropic claude only), got %d: %v", len(s.Models), keys(s.Models))
	}
	op, ok := s.Models["claude-opus-4-8"]
	if !ok {
		t.Fatal("claude-opus-4-8 missing")
	}
	// exact per-type rates carried through (the source-of-truth for proxy pricing)
	if op.InputPerToken != 5e-6 || op.OutputPerToken != 2.5e-5 || op.CacheReadInputPerToken != 5e-7 || op.CacheCreationInputPerToken != 6.25e-6 {
		t.Errorf("opus-4-8 rates wrong: %+v", op)
	}
	// reasoning defaults to output (loader rule) — proxy must price reasoning right
	if op.ReasoningPerToken != 2.5e-5 {
		t.Errorf("opus-4-8 reasoning must default to output 2.5e-5, got %v", op.ReasoningPerToken)
	}
	if op.Source != SourceLiteLLM {
		t.Errorf("source must be litellm, got %q", op.Source)
	}
	// excluded: bedrock re-listing, openai, and the spec block
	for _, bad := range []string{"anthropic.claude-3-5-haiku-20241022-v1:0", "gpt-4o-2024-08-06", "sample_spec"} {
		if _, ok := s.Models[bad]; ok {
			t.Errorf("must exclude %q", bad)
		}
	}
}

func TestBuildSummary_VersionDeterministicAndPriceSensitive(t *testing.T) {
	a, _ := buildSummaryFrom([]byte(litellmFixture))
	b, _ := buildSummaryFrom([]byte(litellmFixture))
	if a.Version == "" || a.Version != b.Version {
		t.Fatalf("version must be non-empty and deterministic: %q vs %q", a.Version, b.Version)
	}
	// changing a kept model's price must move the version
	bumped := `{"claude-opus-4-8":{"litellm_provider":"anthropic","input_cost_per_token":9e-6,"output_cost_per_token":2.5e-5}}`
	c, _ := buildSummaryFrom([]byte(bumped))
	if c.Version == a.Version {
		t.Error("version must change when a kept model's price changes")
	}
}

func TestBuildSummary_RealEmbeddedTableCoversInUseModels(t *testing.T) {
	// Against the REAL embedded litellm table: the models the proxy actually
	// routes must be present with the known input rates (haiku 1 / sonnet 3 /
	// opus 5 per Mtok). Guards the filter against an upstream key rename.
	s, err := BuildSummary()
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	want := map[string]float64{
		"claude-haiku-4-5":  1e-6,
		"claude-sonnet-4-6": 3e-6,
		"claude-opus-4-8":   5e-6,
	}
	for m, in := range want {
		got, ok := s.Models[m]
		if !ok {
			t.Errorf("in-use model %q missing from real summary", m)
			continue
		}
		if got.InputPerToken != in {
			t.Errorf("%q input rate: want %v got %v", m, in, got.InputPerToken)
		}
	}
}

func keys(m map[string]UnitPrices) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
