// Command gen-pricing-summary emits the edge price summary (design D-U8) as JSON
// to stdout, extracted from the embedded LiteLLM table via pricing.BuildSummary.
//
// It is a BUILD-TIME tool, not a runtime service: sync-litellm-prices.sh (and
// release.sh / the make targets) run it after refreshing litellm-prices.json and
// redirect its output into aikey-control-master's embed dir, which then inlines
// the summary into the quota snapshot delivered to every proxy. Keeping the
// generator here (next to the authoritative price parser) guarantees the summary
// applies the exact same field mapping the resolver does — no second parser.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/pricing"
)

func main() {
	s, err := pricing.BuildSummary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-pricing-summary: build summary:", err)
		os.Exit(1)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-pricing-summary: marshal:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "gen-pricing-summary: write:", err)
		os.Exit(1)
	}
}
