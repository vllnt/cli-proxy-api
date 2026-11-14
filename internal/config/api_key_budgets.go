package config

import (
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // budget windows need IANA zones even on hosts without a system tz database
)

// Budget enforcement modes.
const (
	APIKeyBudgetModeObserve = "observe"
	APIKeyBudgetModeEnforce = "enforce"
)

// Budget window names.
const (
	APIKeyBudgetWindowDay   = "day"
	APIKeyBudgetWindowWeek  = "week"
	APIKeyBudgetWindowMonth = "month"
)

// Default weights applied to each token class when computing a request's budget cost.
const (
	defaultBudgetWeightInput      = 1.0
	defaultBudgetWeightOutput     = 1.0
	defaultBudgetWeightCacheWrite = 1.0
	defaultBudgetWeightCacheRead  = 0.1
)

// budgetMilliPerToken is the fixed-point scale of the engine's counters (milli-tokens).
const budgetMilliPerToken = 1000

// APIKeyBudgetWeights scales each token class before it is added to a budget counter.
// An unset weight takes its default; an explicit 0 is kept and makes that class free.
type APIKeyBudgetWeights struct {
	Input      *float64 `yaml:"input,omitempty" json:"input,omitempty"`
	Output     *float64 `yaml:"output,omitempty" json:"output,omitempty"`
	CacheWrite *float64 `yaml:"cache-write,omitempty" json:"cache-write,omitempty"`
	CacheRead  *float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
}

// Resolved returns the four weights with defaults applied to unset values.
func (w APIKeyBudgetWeights) Resolved() (input, output, cacheWrite, cacheRead float64) {
	pick := func(v *float64, def float64) float64 {
		if v == nil {
			return def
		}
		return *v
	}
	return pick(w.Input, defaultBudgetWeightInput),
		pick(w.Output, defaultBudgetWeightOutput),
		pick(w.CacheWrite, defaultBudgetWeightCacheWrite),
		pick(w.CacheRead, defaultBudgetWeightCacheRead)
}

// APIKeyBudgetRule limits the weighted tokens a label may use on matching models within a window.
// Limit is in whole weighted tokens.
type APIKeyBudgetRule struct {
	Models string `yaml:"models" json:"models"`
	Window string `yaml:"window" json:"window"`
	Limit  int64  `yaml:"limit" json:"limit"`
	Mode   string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// EffectiveMode returns the rule's mode, falling back to the global mode.
func (r APIKeyBudgetRule) EffectiveMode(global string) string {
	if mode := normalizeBudgetMode(r.Mode); mode != "" {
		return mode
	}
	if mode := normalizeBudgetMode(global); mode != "" {
		return mode
	}
	return APIKeyBudgetModeObserve
}

// APIKeyBudgets configures per-label token budgets (access.api-key-budgets).
// Labels refer to access.api-key-labels, so counters and logs never carry a key value.
type APIKeyBudgets struct {
	// Mode is the default for rules without their own mode: observe (default) or enforce.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Timezone is the IANA zone whose midnight starts day, week (Monday) and month windows. Default UTC.
	Timezone string              `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Weights  APIKeyBudgetWeights `yaml:"weights,omitempty" json:"weights,omitempty"`
	// Labels maps a label to its rules.
	Labels map[string][]APIKeyBudgetRule `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// EffectiveMode returns the global mode with the default applied.
func (b *APIKeyBudgets) EffectiveMode() string {
	if b != nil {
		if mode := normalizeBudgetMode(b.Mode); mode != "" {
			return mode
		}
	}
	return APIKeyBudgetModeObserve
}

// Location returns the configured time zone, UTC when unset.
func (b *APIKeyBudgets) Location() (*time.Location, error) {
	if b == nil || strings.TrimSpace(b.Timezone) == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(strings.TrimSpace(b.Timezone))
}

func normalizeBudgetMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

func validBudgetMode(mode string) bool {
	switch normalizeBudgetMode(mode) {
	case "", APIKeyBudgetModeObserve, APIKeyBudgetModeEnforce:
		return true
	}
	return false
}

// ValidateAPIKeyBudgets checks access.api-key-budgets. Errors name labels only and never contain a key value.
func (c *SDKConfig) ValidateAPIKeyBudgets() error {
	if c == nil || c.APIKeyBudgets == nil {
		return nil
	}
	b := c.APIKeyBudgets
	const path = "access.api-key-budgets"
	if !validBudgetMode(b.Mode) {
		return fmt.Errorf("%s.mode must be observe or enforce", path)
	}
	if _, err := b.Location(); err != nil {
		return fmt.Errorf("%s.timezone %q is not a known IANA time zone", path, b.Timezone)
	}
	for _, w := range []struct {
		name  string
		value *float64
	}{
		{"input", b.Weights.Input}, {"output", b.Weights.Output},
		{"cache-write", b.Weights.CacheWrite}, {"cache-read", b.Weights.CacheRead},
	} {
		if w.value != nil && (math.IsNaN(*w.value) || math.IsInf(*w.value, 0) || *w.value < 0) {
			return fmt.Errorf("%s.weights.%s must be a finite number >= 0", path, w.name)
		}
	}
	for _, label := range sortedKeys(b.Labels) {
		if _, ok := c.APIKeyLabels[label]; !ok {
			return fmt.Errorf("%s.labels.%s: label is not defined in access.api-key-labels", path, label)
		}
		seen := make(map[string]int, len(b.Labels[label]))
		for i, rule := range b.Labels[label] {
			where := fmt.Sprintf("%s.labels.%s[%d]", path, label, i)
			pattern := strings.ToLower(strings.TrimSpace(rule.Models))
			if pattern == "" {
				return fmt.Errorf("%s: models must not be empty", where)
			}
			switch rule.Window {
			case APIKeyBudgetWindowDay, APIKeyBudgetWindowWeek, APIKeyBudgetWindowMonth:
			default:
				return fmt.Errorf("%s: window must be one of day, week, month", where)
			}
			if rule.Limit <= 0 {
				return fmt.Errorf("%s: limit must be greater than 0", where)
			}
			if rule.Limit > math.MaxInt64/budgetMilliPerToken {
				return fmt.Errorf("%s: limit is too large", where)
			}
			if !validBudgetMode(rule.Mode) {
				return fmt.Errorf("%s: mode must be observe or enforce", where)
			}
			id := pattern + "|" + rule.Window
			if first, dup := seen[id]; dup {
				return fmt.Errorf("%s: duplicates rule [%d] (same models and window)", where, first)
			}
			seen[id] = i
		}
	}
	return nil
}

// ValidateHomeAPIKeyBudgets rejects api-key-budgets in Home mode: requests are routed and
// accounted by the Home control plane, so local budgets cannot be enforced.
func (cfg *Config) ValidateHomeAPIKeyBudgets() error {
	if cfg != nil && cfg.Home.Enabled && cfg.APIKeyBudgets != nil && len(cfg.APIKeyBudgets.Labels) > 0 {
		return fmt.Errorf("access.api-key-budgets is not supported in Home mode; remove it or disable Home")
	}
	return nil
}
