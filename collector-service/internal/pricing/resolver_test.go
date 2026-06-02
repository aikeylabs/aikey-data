package pricing

import "testing"

// fixtureResolver builds a small resolver covering every lookup path:
// - claude: anthropic with full cache rates (litellm layer)
// - gemini: vertex_ai with long-context tiering
// - claude history: a past price for the same anthropic model
// - claude override: an org discount
func fixtureResolver() *Resolver {
	litellm := map[modelKey]UnitPrices{
		{provider: "anthropic", model: "claude-3-5-sonnet-20241022"}: {
			InputPerToken:              0.000003,
			OutputPerToken:             0.000015,
			CacheCreationInputPerToken: 0.00000375,
			CacheReadInputPerToken:     0.0000003,
			ReasoningPerToken:          0.000015, // defaults to output (set by loader)
		},
		{provider: "vertex_ai", model: "gemini-1.5-pro"}: {
			InputPerToken:           0.00000125,
			OutputPerToken:          0.000005,
			InputPerTokenAbove128k:  0.0000025,
			OutputPerTokenAbove128k: 0.00001,
		},
	}
	history := map[modelKey][]historyEntry{
		{provider: "anthropic", model: "claude-3-5-sonnet-20241022"}: {
			// DESC by effectiveFrom: a price that took effect at ts=1_000_000.
			{effectiveFrom: 1_000_000, prices: UnitPrices{InputPerToken: 0.0000028, OutputPerToken: 0.000014}},
		},
	}
	overrides := map[overrideKey]UnitPrices{
		{orgID: "org_vip", provider: "anthropic", model: "claude-3-5-sonnet-20241022"}: {
			InputPerToken:  0.0000024, // 20% off
			OutputPerToken: 0.000012,
		},
	}
	return newResolver(litellm, history, overrides, Snapshot{SnapshotID: "test000000000000"})
}

// litellm hit: no org, no history match for this model -> current list price.
func TestLookup_LiteLLMHit(t *testing.T) {
	r := fixtureResolver()
	// gemini has no history/override; request_ts irrelevant, small input.
	up, err := r.Lookup("vertex_ai", "gemini-1.5-pro", 9_999_999_999, 1000, "")
	if err != nil {
		t.Fatalf("expected hit, got %v", err)
	}
	if up.Source != SourceLiteLLM {
		t.Errorf("source = %q, want litellm", up.Source)
	}
	if up.InputPerToken != 0.00000125 {
		t.Errorf("input = %v, want flat 0.00000125", up.InputPerToken)
	}
}

// unknown model -> ErrUnknownModel (NOT a zero price).
func TestLookup_UnknownModel(t *testing.T) {
	r := fixtureResolver()
	up, err := r.Lookup("anthropic", "claude-fake-9000", 1, 1, "")
	if err != ErrUnknownModel {
		t.Fatalf("err = %v, want ErrUnknownModel", err)
	}
	if up != nil {
		t.Errorf("up = %+v, want nil", up)
	}
}

// override beats history beats litellm for the same model.
func TestLookup_OverrideWins(t *testing.T) {
	r := fixtureResolver()
	up, err := r.Lookup("anthropic", "claude-3-5-sonnet-20241022", 2_000_000, 1, "org_vip")
	if err != nil {
		t.Fatal(err)
	}
	if up.Source != SourceOverrides {
		t.Errorf("source = %q, want overrides (must beat history+litellm)", up.Source)
	}
	if up.InputPerToken != 0.0000024 {
		t.Errorf("input = %v, want discounted 0.0000024", up.InputPerToken)
	}
}

// history hit: request_ts at/after a change, no override -> historical price,
// with EffectiveFrom stamped for the audit snapshot.
func TestLookup_HistoryHit(t *testing.T) {
	r := fixtureResolver()
	up, err := r.Lookup("anthropic", "claude-3-5-sonnet-20241022", 1_500_000, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if up.Source != SourceHistory {
		t.Errorf("source = %q, want history", up.Source)
	}
	if up.InputPerToken != 0.0000028 {
		t.Errorf("input = %v, want historical 0.0000028", up.InputPerToken)
	}
	if up.EffectiveFrom != 1_000_000 {
		t.Errorf("effective_from = %d, want 1000000 (stamped for audit)", up.EffectiveFrom)
	}
}

// history miss when request_ts predates every change -> falls through to litellm.
func TestLookup_HistoryFallThroughToLiteLLM(t *testing.T) {
	r := fixtureResolver()
	up, err := r.Lookup("anthropic", "claude-3-5-sonnet-20241022", 500_000, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if up.Source != SourceLiteLLM {
		t.Errorf("source = %q, want litellm (request predates history)", up.Source)
	}
	if up.InputPerToken != 0.000003 {
		t.Errorf("input = %v, want list 0.000003", up.InputPerToken)
	}
}

// long-context tiering: gemini above 128k input uses the above_128k rates, and
// the tier fields are cleared so the snapshot shows the effective rate.
func TestLookup_LongContextTier(t *testing.T) {
	r := fixtureResolver()
	up, err := r.Lookup("vertex_ai", "gemini-1.5-pro", 1, 200_000, "")
	if err != nil {
		t.Fatal(err)
	}
	if up.InputPerToken != 0.0000025 {
		t.Errorf("input = %v, want above_128k 0.0000025", up.InputPerToken)
	}
	if up.OutputPerToken != 0.00001 {
		t.Errorf("output = %v, want above_128k 0.00001", up.OutputPerToken)
	}
	if up.InputPerTokenAbove128k != 0 || up.OutputPerTokenAbove128k != 0 {
		t.Errorf("tier fields must be cleared after applying, got in=%v out=%v",
			up.InputPerTokenAbove128k, up.OutputPerTokenAbove128k)
	}
}
