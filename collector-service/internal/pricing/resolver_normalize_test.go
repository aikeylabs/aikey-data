package pricing

import "testing"

// canonicalModel normalization correctness — the core of the cross-provider
// fallback. Mirrors the litellm key conventions surveyed in deep-research 2026-06-10.
func TestCanonicalModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"anthropic.claude-3-5-haiku-20241022-v1:0", "claude-3-5-haiku-20241022"},
		{"us.anthropic.claude-3-5-haiku-20241022-v1:0", "claude-3-5-haiku-20241022"},
		{"eu.anthropic.claude-3-5-haiku-20241022-v1:0", "claude-3-5-haiku-20241022"},
		{"bedrock/us.anthropic.claude-3-5-haiku-20241022-v1:0", "claude-3-5-haiku-20241022"},
		{"vertex_ai/claude-3-sonnet@20240229", "vertex_ai/claude-3-sonnet-20240229"}, // non-bedrock host kept (cross-host prices differ)
		{"claude-3-5-haiku-20241022", "claude-3-5-haiku-20241022"}, // already bare: no-op
		{"claude-3-haiku-20240307", "claude-3-haiku-20240307"},
		{"gpt-4o", "gpt-4o"},
	}
	for _, c := range cases {
		if got := canonicalModel(c.in); got != c.want {
			t.Errorf("canonicalModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

const fixtureLiteLLM = `{
  "anthropic.claude-3-5-haiku-20241022-v1:0": {"input_cost_per_token": 8e-07, "output_cost_per_token": 4e-06, "litellm_provider": "bedrock"},
  "us.anthropic.claude-3-5-haiku-20241022-v1:0": {"input_cost_per_token": 8e-07, "output_cost_per_token": 4e-06, "litellm_provider": "bedrock"},
  "eu.anthropic.claude-3-5-haiku-20241022-v1:0": {"input_cost_per_token": 9e-07, "output_cost_per_token": 4.4e-06, "litellm_provider": "bedrock"},
  "claude-3-haiku-20240307": {"input_cost_per_token": 2.5e-07, "output_cost_per_token": 1.25e-06, "litellm_provider": "anthropic"},
  "anthropic.claude-3-haiku-20240307-v1:0": {"input_cost_per_token": 9.9e-07, "output_cost_per_token": 9.9e-06, "litellm_provider": "bedrock"}
}`

const emptyOverlay = `{"schema_version":1,"entries":[]}`

func newNormResolver(t *testing.T) *Resolver {
	t.Helper()
	r, err := LoadFrom([]byte(fixtureLiteLLM), []byte(emptyOverlay), []byte(emptyOverlay))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The user-reported case: claude-3-5-haiku-20241022 has NO plain anthropic key,
// only the Bedrock entry — must price via the canonical fallback at the Bedrock
// rates, tagged normalized.
func TestResolve_BedrockKeyedAnthropic_NormalizedFallback(t *testing.T) {
	r := newNormResolver(t)
	up, err := r.Lookup("anthropic", "claude-3-5-haiku-20241022", 0, 0, "")
	if err != nil {
		t.Fatalf("expected price via fallback, got %v", err)
	}
	if up.Source != SourceLiteLLMNormalized {
		t.Errorf("Source = %q, want %q", up.Source, SourceLiteLLMNormalized)
	}
	if up.InputPerToken != 8e-07 || up.OutputPerToken != 4e-06 {
		t.Errorf("prices = in %v out %v, want 8e-07 / 4e-06", up.InputPerToken, up.OutputPerToken)
	}
}

// Regression guard: a model with BOTH a native anthropic key AND a bedrock
// variant at a DIFFERENT price must resolve via the exact native key, never the
// fallback — the fast path is unchanged for already-mapped models.
func TestResolve_ExactWins_NativePlainKey(t *testing.T) {
	r := newNormResolver(t)
	up, err := r.Lookup("anthropic", "claude-3-haiku-20240307", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if up.Source != SourceLiteLLM {
		t.Errorf("Source = %q, want %q (exact)", up.Source, SourceLiteLLM)
	}
	if up.InputPerToken != 2.5e-07 {
		t.Errorf("input = %v, want native 2.5e-07 (not bedrock 9.9e-07)", up.InputPerToken)
	}
	// And the canonical index itself must hold the native price, not the bedrock one.
	if cp, ok := r.byCanonical["claude-3-haiku-20240307"]; !ok || cp.InputPerToken != 2.5e-07 {
		t.Errorf("byCanonical native-wins broken: %+v ok=%v", cp, ok)
	}
}

func TestResolve_Unknown_StillUnpriced(t *testing.T) {
	r := newNormResolver(t)
	if _, err := r.Lookup("anthropic", "totally-made-up-model-xyz", 0, 0, ""); err != ErrUnknownModel {
		t.Errorf("want ErrUnknownModel, got %v", err)
	}
}

// Runs against the REAL embedded snapshot: the user-reported model must now
// resolve (exact or fallback) with non-zero rates. Tolerant if upstream later
// adds a plain key (would simply hit the exact path).
func TestEmbedded_Claude35Haiku_NowPriced(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.Lookup("anthropic", "claude-3-5-haiku-20241022", 0, 0, "")
	if err != nil {
		t.Fatalf("claude-3-5-haiku-20241022 still unpriced: %v", err)
	}
	if up.InputPerToken <= 0 || up.OutputPerToken <= 0 {
		t.Errorf("priced but zero rates: %+v", up)
	}
	t.Logf("claude-3-5-haiku-20241022 → in=%v out=%v src=%s", up.InputPerToken, up.OutputPerToken, up.Source)
}

// Fence over the real snapshot: a canonical name derived ONLY from prefixed
// entries (no native plain key) must not have 2+ DISTINCT prices among its
// sources — that would mean the fallback silently picks one and could mis-price.
// Native-keyed canonicals are exempt (native always wins). Logs offenders; a
// generous ceiling guards against a normalization regression that over-merges.
func TestEmbedded_NoBadCanonicalCollisions(t *testing.T) {
	lit, err := parseLiteLLM(litellmJSON)
	if err != nil {
		t.Fatal(err)
	}
	type pp struct{ in, out float64 }
	groups := map[string]map[pp]bool{}
	native := map[string]bool{}
	for k := range lit {
		c := canonicalModel(k.model)
		if c == k.model {
			native[c] = true
		}
		if groups[c] == nil {
			groups[c] = map[pp]bool{}
		}
		groups[c][pp{lit[k].InputPerToken, lit[k].OutputPerToken}] = true
	}
	bad := 0
	for c, prices := range groups {
		if native[c] || len(prices) <= 1 {
			continue
		}
		bad++
		if bad <= 10 {
			t.Logf("fallback canonical collision (no native key): %q has %d distinct prices", c, len(prices))
		}
	}
	if bad > 40 {
		t.Errorf("too many fallback canonical collisions: %d — normalization may be over-merging distinct models", bad)
	}
}
