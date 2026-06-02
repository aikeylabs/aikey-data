package pricing

import (
	"math"
	"testing"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// Anthropic-style: Input is already uncached; cache read/creation billed on own
// counts. cost = 100*P_in + 20*P_cc + 50*P_cr + 200*P_out (reasoning 0).
func TestComputeCost_AnthropicStyle(t *testing.T) {
	up := UnitPrices{
		InputPerToken:              0.000003,
		OutputPerToken:             0.000015,
		CacheCreationInputPerToken: 0.00000375,
		CacheReadInputPerToken:     0.0000003,
		ReasoningPerToken:          0.000015,
	}
	tk := TokenCounts{Input: 100, Output: 200, CacheRead: 50, CacheCreation: 20}

	got := ComputeCost(up, tk, false /* input already uncached */)
	want := 100*0.000003 + 20*0.00000375 + 50*0.0000003 + 200*0.000015
	if !approxEq(got, want) {
		t.Fatalf("anthropic cost = %v, want %v", got, want)
	}
}

// OpenAI-style: prompt_tokens (Input=100) INCLUDES cached; uncached = 100-30-10=60.
func TestComputeCost_OpenAIStyle(t *testing.T) {
	up := UnitPrices{
		InputPerToken:          0.0000025,
		OutputPerToken:         0.00001,
		CacheReadInputPerToken: 0.00000125,
		ReasoningPerToken:      0.00001,
	}
	tk := TokenCounts{Input: 100, Output: 50, CacheRead: 30, CacheCreation: 10}

	got := ComputeCost(up, tk, true /* prompt_tokens includes cache */)
	uncached := float64(100 - 30 - 10)
	want := uncached*0.0000025 + 10*0 + 30*0.00000125 + 50*0.00001
	if !approxEq(got, want) {
		t.Fatalf("openai cost = %v, want %v", got, want)
	}
}

// Reasoning tokens (o-series) bill at the reasoning rate (defaults to output).
func TestComputeCost_Reasoning(t *testing.T) {
	up := UnitPrices{
		InputPerToken:     0.000002,
		OutputPerToken:    0.000008,
		ReasoningPerToken: 0.000008,
	}
	tk := TokenCounts{Input: 1000, Output: 100, Reasoning: 500}

	got := ComputeCost(up, tk, false)
	want := 1000*0.000002 + 100*0.000008 + 500*0.000008
	if !approxEq(got, want) {
		t.Fatalf("reasoning cost = %v, want %v", got, want)
	}
}

// OpenAI-style with cache >= input must clamp uncached at 0, never negative.
func TestComputeCost_UncachedClampZero(t *testing.T) {
	up := UnitPrices{InputPerToken: 0.001, CacheReadInputPerToken: 0.0001, OutputPerToken: 0.002}
	tk := TokenCounts{Input: 40, CacheRead: 50, Output: 10} // cache > input (odd but possible)

	got := ComputeCost(up, tk, true)
	// uncached clamped to 0 → only cache_read + output contribute
	want := 0*0.001 + 50*0.0001 + 10*0.002
	if !approxEq(got, want) {
		t.Fatalf("clamp cost = %v, want %v", got, want)
	}
}

// FormatCostUSD: 8-decimal fixed string for the NUMERIC(20,8) column.
func TestFormatCostUSD(t *testing.T) {
	cases := map[float64]string{
		0.0:        "0.00000000",
		0.000003:   "0.00000300",
		1.5:        "1.50000000",
		0.42000001: "0.42000001",
	}
	for in, want := range cases {
		if got := FormatCostUSD(in); got != want {
			t.Errorf("FormatCostUSD(%v) = %q, want %q", in, got, want)
		}
	}
}
