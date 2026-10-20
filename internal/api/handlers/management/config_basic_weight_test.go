package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyHeadroom(t *testing.T) {
	for _, input := range []string{"headroom", " Headroom "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "headroom" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want headroom, true", input, got, ok)
		}
	}
}
