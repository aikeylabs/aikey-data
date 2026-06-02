package pricing

import _ "embed"

// The three pricing sources are embedded into the binary at build time, so a
// privatized deployment never fetches anything at runtime. They live under
// data/ because go:embed cannot reference paths outside the package directory
// (the design's config/pricing/ path is unreachable from internal/pricing/).
// The release.sh pre-build hook overwrites data/litellm-prices.json with the
// full upstream LiteLLM file before compiling.

//go:embed data/litellm-prices.json
var litellmJSON []byte

//go:embed data/pricing-history.json
var historyJSON []byte

//go:embed data/pricing-overrides.json
var overridesJSON []byte
