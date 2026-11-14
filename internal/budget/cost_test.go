package budget

import (
	"math"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var defaultWeights = Weights{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 0.1}

func detailFrom(uncached, cacheRead, cacheWrite, output int64) usage.Detail {
	total := uncached + cacheRead + cacheWrite + output
	return usage.Detail{
		TotalTokens:    total,
		TokenBreakdown: usage.NewIndependentTokenBreakdown(uncached, cacheRead, cacheWrite, output, 0, total),
	}
}

func TestCostMilli(t *testing.T) {
	tests := []struct {
		name     string
		detail   usage.Detail
		provider string
		weights  Weights
		want     int64
	}{
		{"default weights", detailFrom(1000, 5000, 200, 300), "claude", defaultWeights, 2_000_000},
		{"custom weights", detailFrom(1000, 5000, 200, 300), "claude", Weights{Input: 2, Output: 4, CacheWrite: 1.25, CacheRead: 0}, 3_450_000},
		{"cache read only", detailFrom(0, 10000, 0, 0), "claude", defaultWeights, 1_000_000},
		{"fractional tokens round to the milli-token", detailFrom(0, 3, 0, 0), "claude", defaultWeights, 300},
		{"zero detail", usage.Detail{}, "claude", defaultWeights, 0},
		{"unclassified counts at weight 1", usage.Detail{TokenBreakdown: usage.NewUnclassifiedTokenBreakdown(500)}, "unknown", defaultWeights, 500_000},
		{"cached tokens field is ignored", func() usage.Detail {
			d := detailFrom(100, 0, 0, 0)
			d.CachedTokens = 5000
			return d
		}(), "claude", defaultWeights, 100_000},
		{"missing breakdown is rebuilt from legacy fields", usage.Detail{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}, "claude", defaultWeights, 150_000},
		{"invalid breakdown is rebuilt", usage.Detail{
			InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			TokenBreakdown: usage.TokenBreakdown{SchemaVersion: usage.TokenAccountingSchemaVersion, Quality: usage.TokenAccountingQualityComplete, TotalTokens: 999},
		}, "claude", defaultWeights, 150_000},
		{"rebuilt legacy cache read is discounted", usage.Detail{InputTokens: 1000, CacheReadTokens: 4000, OutputTokens: 0, TotalTokens: 5000}, "claude", defaultWeights, 1_400_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CostMilli(tt.detail, tt.provider, "", tt.weights); got != tt.want {
				t.Fatalf("CostMilli() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCostMilliNeverNegativeOrNonFinite(t *testing.T) {
	for _, w := range []Weights{
		{Input: -1, Output: -1, CacheWrite: -1, CacheRead: -1},
		{Input: math.NaN(), Output: math.Inf(1)},
	} {
		if got := CostMilli(detailFrom(100, 100, 100, 100), "claude", "", w); got < 0 {
			t.Fatalf("CostMilli(%+v) = %d, want >= 0", w, got)
		}
	}
	huge := detailFrom(math.MaxInt64/8, 0, 0, math.MaxInt64/8)
	if got := CostMilli(huge, "claude", "", Weights{Input: 1e6, Output: 1e6}); got <= 0 || got > math.MaxInt64/2 {
		t.Fatalf("CostMilli(huge) = %d, want clamped into (0, MaxInt64/2]", got)
	}
}
