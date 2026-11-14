package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const budgetTestBase = `
access:
  api-keys: ["test-key-a", "test-key-b"]
  api-key-labels:
    team-x: "test-key-a"
    team-y: "test-key-b"
`

func parseBudgetConfig(t *testing.T, budgets string) *Config {
	t.Helper()
	cfg, err := ParseConfigBytes([]byte(budgetTestBase + budgets))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	return cfg
}

func TestAPIKeyBudgetsParseV8Path(t *testing.T) {
	cfg := parseBudgetConfig(t, `
  api-key-budgets:
    mode: enforce
    timezone: Europe/Paris
    weights: {input: 2, output: 4, cache-write: 1.25, cache-read: 0}
    labels:
      team-x:
        - {models: "claude-fable-*", window: week, limit: 2000000}
        - {models: "*", window: day, limit: 20000000, mode: observe}
`)
	b := cfg.APIKeyBudgets
	if b == nil {
		t.Fatal("APIKeyBudgets must be set")
	}
	if b.Mode != "enforce" || b.Timezone != "Europe/Paris" {
		t.Fatalf("mode/timezone = %q/%q", b.Mode, b.Timezone)
	}
	in, out, cw, cr := b.Weights.Resolved()
	if in != 2 || out != 4 || cw != 1.25 || cr != 0 {
		t.Fatalf("weights = %v %v %v %v; an explicit 0 must stay 0", in, out, cw, cr)
	}
	rules := b.Labels["team-x"]
	if len(rules) != 2 {
		t.Fatalf("rules = %+v", rules)
	}
	if rules[0] != (APIKeyBudgetRule{Models: "claude-fable-*", Window: "week", Limit: 2000000}) {
		t.Fatalf("rule[0] = %+v", rules[0])
	}
	if rules[1] != (APIKeyBudgetRule{Models: "*", Window: "day", Limit: 20000000, Mode: "observe"}) {
		t.Fatalf("rule[1] = %+v", rules[1])
	}
}

func TestAPIKeyBudgetsParseV7FlatPath(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
api-keys: ["test-key-a"]
api-key-labels: {team-x: "test-key-a"}
api-key-budgets:
  labels:
    team-x:
      - {models: "*", window: month, limit: 5}
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.APIKeyBudgets == nil || len(cfg.APIKeyBudgets.Labels["team-x"]) != 1 {
		t.Fatalf("v7 flat api-key-budgets did not load: %+v", cfg.APIKeyBudgets)
	}
}

func TestAPIKeyBudgetsUnsetIsNil(t *testing.T) {
	cfg := parseBudgetConfig(t, "")
	if cfg.APIKeyBudgets != nil {
		t.Fatalf("unset must stay nil, got %+v", cfg.APIKeyBudgets)
	}
}

func TestAPIKeyBudgetsDefaults(t *testing.T) {
	cfg := parseBudgetConfig(t, `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 1}
`)
	b := cfg.APIKeyBudgets
	if got := b.EffectiveMode(); got != APIKeyBudgetModeObserve {
		t.Fatalf("default mode = %q, want observe", got)
	}
	if got := b.Labels["team-x"][0].EffectiveMode(b.EffectiveMode()); got != APIKeyBudgetModeObserve {
		t.Fatalf("rule mode inherits the global default, got %q", got)
	}
	loc, err := b.Location()
	if err != nil || loc.String() != "UTC" {
		t.Fatalf("default location = %v, %v; want UTC", loc, err)
	}
	in, out, cw, cr := b.Weights.Resolved()
	if in != 1 || out != 1 || cw != 1 || cr != 0.1 {
		t.Fatalf("default weights = %v %v %v %v; want 1 1 1 0.1", in, out, cw, cr)
	}
}

func TestAPIKeyBudgetsRuleModeOverridesGlobal(t *testing.T) {
	cfg := parseBudgetConfig(t, `
  api-key-budgets:
    mode: observe
    labels:
      team-x:
        - {models: "*", window: day, limit: 1, mode: enforce}
        - {models: "x", window: day, limit: 1}
`)
	b := cfg.APIKeyBudgets
	rules := b.Labels["team-x"]
	if got := rules[0].EffectiveMode(b.EffectiveMode()); got != APIKeyBudgetModeEnforce {
		t.Fatalf("rule override = %q, want enforce", got)
	}
	if got := rules[1].EffectiveMode(b.EffectiveMode()); got != APIKeyBudgetModeObserve {
		t.Fatalf("inherited = %q, want observe", got)
	}
}

func TestAPIKeyBudgetsValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		budgets string
		wantErr string
	}{
		{"label not defined", `
  api-key-budgets:
    labels:
      ghost:
        - {models: "*", window: day, limit: 1}
`, "ghost: label is not defined in access.api-key-labels"},
		{"bad window", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: hour, limit: 1}
`, "window must be one of day, week, month"},
		{"missing window", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", limit: 1}
`, "window must be one of day, week, month"},
		{"zero limit", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 0}
`, "limit must be greater than 0"},
		{"negative limit", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: -5}
`, "limit must be greater than 0"},
		{"limit overflows milli-tokens", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 9223372036854775807}
`, "limit is too large"},
		{"empty models", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "  ", window: day, limit: 1}
`, "models must not be empty"},
		{"bad rule mode", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 1, mode: block}
`, "mode must be observe or enforce"},
		{"bad global mode", `
  api-key-budgets:
    mode: block
    labels:
      team-x:
        - {models: "*", window: day, limit: 1}
`, "mode must be observe or enforce"},
		{"bad timezone", `
  api-key-budgets:
    timezone: Mars/Olympus
    labels:
      team-x:
        - {models: "*", window: day, limit: 1}
`, "timezone"},
		{"negative weight", `
  api-key-budgets:
    weights: {output: -1}
    labels:
      team-x:
        - {models: "*", window: day, limit: 1}
`, "weights.output must be a finite number >= 0"},
		{"duplicate rule", `
  api-key-budgets:
    labels:
      team-x:
        - {models: "Claude-*", window: day, limit: 1}
        - {models: "claude-*", window: day, limit: 2}
`, "duplicates rule"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte(budgetTestBase + tt.budgets))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "test-key-") {
				t.Fatalf("error must not contain a key value: %v", err)
			}
		})
	}
}

func TestAPIKeyBudgetsRejectsNonFiniteWeights(t *testing.T) {
	for name, value := range map[string]float64{"nan": math.NaN(), "inf": math.Inf(1), "neg inf": math.Inf(-1)} {
		t.Run(name, func(t *testing.T) {
			v := value
			cfg := &SDKConfig{
				APIKeys:      []string{"test-key-a"},
				APIKeyLabels: map[string]string{"team-x": "test-key-a"},
				APIKeyBudgets: &APIKeyBudgets{
					Weights: APIKeyBudgetWeights{CacheRead: &v},
					Labels:  map[string][]APIKeyBudgetRule{"team-x": {{Models: "*", Window: "day", Limit: 1}}},
				},
			}
			err := cfg.ValidateAPIKeyBudgets()
			if err == nil || !strings.Contains(err.Error(), "weights.cache-read") {
				t.Fatalf("error = %v, want weights.cache-read", err)
			}
		})
	}
}

func TestAPIKeyBudgetsEmptyLabelsIsValid(t *testing.T) {
	parseBudgetConfig(t, `
  api-key-budgets:
    mode: observe
`)
}

func TestAPIKeyBudgetsValidateNilReceivers(t *testing.T) {
	var nilSDK *SDKConfig
	if err := nilSDK.ValidateAPIKeyBudgets(); err != nil {
		t.Fatalf("nil SDKConfig must pass, got %v", err)
	}
	var nilCfg *Config
	if err := nilCfg.ValidateHomeAPIKeyBudgets(); err != nil {
		t.Fatalf("nil Config must pass, got %v", err)
	}
	var nilBudgets *APIKeyBudgets
	if got := nilBudgets.EffectiveMode(); got != APIKeyBudgetModeObserve {
		t.Fatalf("nil budgets mode = %q", got)
	}
	if loc, err := nilBudgets.Location(); err != nil || loc.String() != "UTC" {
		t.Fatalf("nil budgets location = %v, %v", loc, err)
	}
}

func TestLoadConfigRejectsInvalidBudgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := budgetTestBase + `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 0}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, optional := range []bool{false, true} {
		if _, err := LoadConfigOptional(path, optional); err == nil {
			t.Fatalf("LoadConfigOptional(optional=%v) must reject invalid budgets", optional)
		}
	}
}

func TestAPIKeyBudgetsRejectedInHomeMode(t *testing.T) {
	cfg := parseBudgetConfig(t, `
  api-key-budgets:
    labels:
      team-x:
        - {models: "*", window: day, limit: 1}
`)
	if err := cfg.ValidateHomeAPIKeyBudgets(); err != nil {
		t.Fatalf("non-Home mode must accept budgets, got %v", err)
	}
	cfg.Home.Enabled = true
	err := cfg.ValidateHomeAPIKeyBudgets()
	if err == nil || !strings.Contains(err.Error(), "Home") || !strings.Contains(err.Error(), "api-key-budgets") {
		t.Fatalf("Home mode with budgets must fail, got %v", err)
	}
	if strings.Contains(err.Error(), "test-key-") {
		t.Fatalf("error must not contain a key value: %v", err)
	}
	cfg.APIKeyBudgets = nil
	if err = cfg.ValidateHomeAPIKeyBudgets(); err != nil {
		t.Fatalf("Home mode without budgets must pass, got %v", err)
	}
}

// Operators enable the feature by uncommenting the example block; it must load as written.
func TestAPIKeyBudgetsExampleUncommentsAndLoads(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(example), "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  # api-key-budgets:") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("config.example.yaml is missing the api-key-budgets example")
	}
	for i := start; i < len(lines) && strings.HasPrefix(lines[i], "  #"); i++ {
		lines[i] = "  " + strings.TrimPrefix(lines[i], "  # ")
	}
	// The budgets example references labels from the allowlist example above it.
	for i, line := range lines {
		if strings.HasPrefix(line, "  # api-key-labels:") {
			for j := i; j < len(lines) && strings.HasPrefix(lines[j], "  #"); j++ {
				lines[j] = "  " + strings.TrimPrefix(lines[j], "  # ")
			}
			break
		}
	}
	cfg, err := ParseConfigBytes([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("uncommented example must load: %v", err)
	}
	if cfg.APIKeyBudgets == nil || len(cfg.APIKeyBudgets.Labels) == 0 {
		t.Fatalf("example budgets did not reach the runtime config: %+v", cfg.APIKeyBudgets)
	}
}
