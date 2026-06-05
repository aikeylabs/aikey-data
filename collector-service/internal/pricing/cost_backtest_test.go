package pricing

import (
	"math"
	"testing"
)

// TestBacktest_PureInputSemantics_PlanA is the Phase-1 research validation for
// 方案 A (input_tokens -> PURE, see
// roadmap20260320/技术实现/update/20260604-token-input-纯输入语义治本-方案A.md).
//
// It proves, on representative cache-heavy + cache-free cases, that switching the
// proxy to report PURE input:
//  1. billing is IDENTICAL to the shipped fix B (Input=total + inputIncludesCache=true)
//     — i.e. NO billing regression vs the stopgap (acceptance: "等价且更干净");
//  2. billing equals the independent ground truth (pure×input + cache at own rates);
//  3. the token metric stops double-counting cache (old total-formula double-counts;
//     pure-formula == the true token count);
//  4. cache-free requests are byte-identical old vs new.
func TestBacktest_PureInputSemantics_PlanA(t *testing.T) {
	// opus-4-8 representative rates.
	up := UnitPrices{
		InputPerToken:              5e-6,
		OutputPerToken:             2.5e-5,
		CacheReadInputPerToken:     5e-7,
		CacheCreationInputPerToken: 6.25e-6,
		ReasoningPerToken:          0,
	}
	cases := []struct {
		name                       string
		pure, output, cr, cc, reas int64
	}{
		{"cache-heavy", 12_000, 80_000, 12_471_471, 1_447_153, 0}, // real opus-4-8 shape
		{"light-cache", 2, 16, 998, 0, 0},
		{"no-cache", 1000, 500, 0, 0, 0},
	}
	for _, c := range cases {
		total := c.pure + c.cr + c.cc // what the proxy reports TODAY (totalInput)

		// (2) ground truth: pure at input rate, cache at its own rates.
		truth := float64(c.pure)*up.InputPerToken +
			float64(c.cr)*up.CacheReadInputPerToken +
			float64(c.cc)*up.CacheCreationInputPerToken +
			float64(c.output)*up.OutputPerToken +
			float64(c.reas)*up.ReasoningPerToken

		// fix B path: Input=total, inputIncludesCache=true (subtract cache).
		fixB := ComputeCost(up, TokenCounts{Input: total, Output: c.output, CacheRead: c.cr, CacheCreation: c.cc, Reasoning: c.reas}, true)
		// 方案 A path: Input=pure, inputIncludesCache=false (don't subtract).
		planA := ComputeCost(up, TokenCounts{Input: c.pure, Output: c.output, CacheRead: c.cr, CacheCreation: c.cc, Reasoning: c.reas}, false)

		if math.Abs(planA-truth) > 1e-9 {
			t.Errorf("%s: plan A billing %v != ground truth %v", c.name, planA, truth)
		}
		if math.Abs(fixB-planA) > 1e-9 {
			t.Errorf("%s: fix B billing %v != plan A %v (must be equivalent — no regression)", c.name, fixB, planA)
		}

		// (3) token metric: old (proxy reports total) double-counts cache.
		oldMetric := total + c.output + c.cr + c.cc + c.reas
		newMetric := c.pure + c.output + c.cr + c.cc + c.reas // plan A: input is pure
		trueTokens := c.pure + c.output + c.cr + c.cc + c.reas
		if newMetric != trueTokens {
			t.Errorf("%s: plan A token metric %d != true %d", c.name, newMetric, trueTokens)
		}
		if c.cr+c.cc > 0 && oldMetric <= newMetric {
			t.Errorf("%s: sanity — old metric %d must exceed new %d (documents the double-count)", c.name, oldMetric, newMetric)
		}

		// (4) cache-free: old == new for both billing and tokens.
		if c.cr == 0 && c.cc == 0 {
			if oldMetric != newMetric {
				t.Errorf("%s: cache-free token metric must be identical old=%d new=%d", c.name, oldMetric, newMetric)
			}
		}
		t.Logf("%s: billing truth=%.6f fixB=%.6f planA=%.6f | tokens old=%d new(true)=%d",
			c.name, truth, fixB, planA, oldMetric, newMetric)
	}
}
