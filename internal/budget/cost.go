package budget

import (
	"math"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// milliPerToken is the fixed-point scale of every counter: 1000 milli-tokens = one weighted token.
const milliPerToken = 1000

// maxCostMilli caps one request's cost so a corrupt usage record cannot overflow a counter.
const maxCostMilli = math.MaxInt64 / 4

// Weights scales each token class into budget units.
type Weights struct{ Input, Output, CacheWrite, CacheRead float64 }

// CostMilli returns the weighted cost of one usage detail in milli-tokens (1000 = one weighted token).
// It reads the canonical token breakdown, rebuilding it for the provider when the detail has none or an
// invalid one; Detail.CachedTokens is never used because it overlaps the other input buckets.
// Tokens the breakdown could not classify count at weight 1 so they are never free.
func CostMilli(detail usage.Detail, provider, executorType string, w Weights) int64 {
	b := usage.EnsureTokenBreakdownForProvider(detail, provider, executorType).TokenBreakdown
	if !b.Valid() {
		return 0
	}
	tokens := float64(b.Input.UncachedTokens)*sanitizeWeight(w.Input) +
		float64(b.Input.CacheWriteTokens)*sanitizeWeight(w.CacheWrite) +
		float64(b.Input.CacheReadTokens)*sanitizeWeight(w.CacheRead) +
		float64(b.Output.TotalTokens)*sanitizeWeight(w.Output) +
		float64(b.UnclassifiedTokens)
	milli := tokens * milliPerToken
	switch {
	case math.IsNaN(milli) || milli <= 0:
		return 0
	case milli >= maxCostMilli:
		return maxCostMilli
	}
	return int64(math.Round(milli))
}

func sanitizeWeight(w float64) float64 {
	if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
		return 0
	}
	return w
}
