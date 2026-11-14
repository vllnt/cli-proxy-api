package budget

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCanonicalModel(t *testing.T) {
	tests := map[string]string{
		"claude-fable-5":              "claude-fable-5",
		"Claude-Fable-5":              "claude-fable-5",
		"  claude-fable-5  ":          "claude-fable-5",
		"claude-fable-5(high)":        "claude-fable-5",
		"claude-fable-5(8192)":        "claude-fable-5",
		"Claude-Fable-5(HIGH)":        "claude-fable-5",
		"antigravity/claude-fable-5":  "antigravity/claude-fable-5",
		"gemini-3-pro(low)":           "gemini-3-pro",
		"":                            "",
		"   ":                         "",
		"claude-fable-5(unterminated": "claude-fable-5(unterminated",
	}
	for raw, want := range tests {
		if got := CanonicalModel(raw); got != want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	tests := []struct {
		pattern string
		model   string
		want    bool
	}{
		{"claude-fable-*", "claude-fable-5", true},
		{"claude-fable-*", "claude-fable-5-20260101", true},
		{"claude-fable-*", "claude-sonnet-5", false},
		{"Claude-FABLE-*", "claude-fable-5", true},
		{"*", "anything", true},
		{"*", "", true},
		{"claude-fable-*", "antigravity/claude-fable-5", true},
		{"antigravity/*", "antigravity/claude-fable-5", true},
		{"antigravity/claude-*", "antigravity/claude-fable-5", true},
		{"claude-fable-*", "a/b/claude-fable-5", false},
		{"b/claude-fable-*", "a/b/claude-fable-5", true},
		{"gpt-6", "gpt-6", true},
		{"gpt-6", "gpt-6-mini", false},
		{"gpt-6", "openai/gpt-6", true},
	}
	for _, tt := range tests {
		r := &rule{pattern: strings.ToLower(tt.pattern)}
		if got := r.matches(CanonicalModel(tt.model)); got != tt.want {
			t.Errorf("pattern %q vs %q = %v, want %v", tt.pattern, tt.model, got, tt.want)
		}
	}
}

func TestRuleID(t *testing.T) {
	base := ruleID("claude-fable-*", "week")
	if len(base) != 12 {
		t.Fatalf("ruleID length = %d, want 12 (%q)", len(base), base)
	}
	if strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("ruleID must be lowercase hex, got %q", base)
	}
	if ruleID("Claude-Fable-*", "week") != base {
		t.Fatal("ruleID must ignore pattern case")
	}
	if ruleID("claude-fable-*", "day") == base {
		t.Fatal("ruleID must depend on the window")
	}
	if ruleID("claude-*", "week") == base {
		t.Fatal("ruleID must depend on the pattern")
	}
}

func compileTestConfig(t *testing.T, budgets *config.APIKeyBudgets) *policy {
	t.Helper()
	cfg := &config.SDKConfig{
		APIKeys:       []string{"test-key-a", "test-key-b", "test-key-c"},
		APIKeyLabels:  map[string]string{"team-x": "test-key-a", "team-y": "  test-key-b "},
		APIKeyBudgets: budgets,
	}
	pol, err := compilePolicy(cfg)
	if err != nil {
		t.Fatalf("compilePolicy() error = %v", err)
	}
	return pol
}

func TestCompilePolicy(t *testing.T) {
	cacheRead := 0.5
	pol := compileTestConfig(t, &config.APIKeyBudgets{
		Mode:     "enforce",
		Timezone: "Europe/Paris",
		Weights:  config.APIKeyBudgetWeights{CacheRead: &cacheRead},
		Labels: map[string][]config.APIKeyBudgetRule{
			"team-x": {
				{Models: " Claude-Fable-* ", Window: "week", Limit: 2000},
				{Models: "*", Window: "day", Limit: 5, Mode: "observe"},
			},
		},
	})
	if pol == nil {
		t.Fatal("policy must not be nil")
	}
	if pol.loc.String() != "Europe/Paris" {
		t.Fatalf("loc = %v", pol.loc)
	}
	if pol.weights != (Weights{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 0.5}) {
		t.Fatalf("weights = %+v", pol.weights)
	}
	if label := pol.labelByKey["test-key-a"]; label != "team-x" {
		t.Fatalf("label for key a = %q", label)
	}
	if _, ok := pol.labelByKey["test-key-c"]; ok {
		t.Fatal("an unlabelled key must have no label")
	}
	rules := pol.rules["team-x"]
	if len(rules) != 2 {
		t.Fatalf("rules = %d", len(rules))
	}
	if rules[0].pattern != "claude-fable-*" || rules[0].window != "week" || rules[0].limitMilli != 2_000_000 || !rules[0].enforce {
		t.Fatalf("rule[0] = %+v", *rules[0])
	}
	if rules[1].enforce || rules[1].limitMilli != 5_000 {
		t.Fatalf("rule[1] = %+v", *rules[1])
	}
	if rules[0].id != ruleID("claude-fable-*", "week") {
		t.Fatalf("rule[0].id = %q", rules[0].id)
	}
	if _, ok := pol.rules["team-y"]; ok {
		t.Fatal("a label without rules must not appear")
	}
}

func TestCompilePolicyNilWhenNothingToEnforce(t *testing.T) {
	if pol, err := compilePolicy(nil); pol != nil || err != nil {
		t.Fatalf("nil config = %v, %v", pol, err)
	}
	if pol, err := compilePolicy(&config.SDKConfig{}); pol != nil || err != nil {
		t.Fatalf("no budgets = %v, %v", pol, err)
	}
	empty := &config.SDKConfig{APIKeyBudgets: &config.APIKeyBudgets{Mode: "enforce"}}
	if pol, err := compilePolicy(empty); pol != nil || err != nil {
		t.Fatalf("no label rules = %v, %v", pol, err)
	}
}

func TestCompilePolicyRejectsInvalid(t *testing.T) {
	cfg := &config.SDKConfig{
		APIKeyLabels: map[string]string{"team-x": "test-key-a"},
		APIKeys:      []string{"test-key-a"},
		APIKeyBudgets: &config.APIKeyBudgets{Labels: map[string][]config.APIKeyBudgetRule{
			"team-x": {{Models: "*", Window: "hour", Limit: 1}},
		}},
	}
	if _, err := compilePolicy(cfg); err == nil || strings.Contains(err.Error(), "test-key-") {
		t.Fatalf("invalid config must fail without leaking a key, got %v", err)
	}
}
