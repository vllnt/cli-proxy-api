package budget

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// rule is one compiled budget rule. id is stable across limit and mode changes so a counter survives them.
type rule struct {
	id         string
	pattern    string // lower-case, trimmed
	window     string
	limitMilli int64
	enforce    bool
}

// policy is an immutable compiled snapshot of the budget config.
type policy struct {
	loc        *time.Location
	weights    Weights
	labelByKey map[string]string
	rules      map[string][]*rule
}

// CanonicalModel reduces a model name to the form rules are matched against:
// trimmed, lower-case, without a thinking suffix such as "(high)" or "(8192)".
func CanonicalModel(raw string) string {
	return strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(raw)).ModelName))
}

// ruleID identifies a rule by what it matches and how long it lasts, not by its limit.
func ruleID(pattern, window string) string {
	return kvstore.HashKeyPart(strings.ToLower(strings.TrimSpace(pattern)) + "|" + window)[:12]
}

// matches reports whether the canonical model matches the rule, either as written or without
// its first "prefix/" segment (so "claude-*" also covers "antigravity/claude-fable-5").
func (r *rule) matches(model string) bool {
	if util.MatchWildcard(r.pattern, model) {
		return true
	}
	if i := strings.IndexByte(model, '/'); i >= 0 && i+1 < len(model) {
		return util.MatchWildcard(r.pattern, model[i+1:])
	}
	return false
}

// compilePolicy builds a policy from the config. It returns nil when no label has a rule,
// which turns the engine into a no-op.
func compilePolicy(cfg *config.SDKConfig) (*policy, error) {
	if cfg == nil || cfg.APIKeyBudgets == nil {
		return nil, nil
	}
	if err := cfg.ValidateAPIKeyBudgets(); err != nil {
		return nil, err
	}
	b := cfg.APIKeyBudgets
	loc, err := b.Location()
	if err != nil {
		return nil, err
	}
	in, out, cacheWrite, cacheRead := b.Weights.Resolved()
	pol := &policy{
		loc:        loc,
		weights:    Weights{Input: in, Output: out, CacheWrite: cacheWrite, CacheRead: cacheRead},
		labelByKey: make(map[string]string, len(b.Labels)),
		rules:      make(map[string][]*rule, len(b.Labels)),
	}
	globalMode := b.EffectiveMode()
	for label, cfgRules := range b.Labels {
		if len(cfgRules) == 0 {
			continue
		}
		compiled := make([]*rule, 0, len(cfgRules))
		for _, r := range cfgRules {
			pattern := strings.ToLower(strings.TrimSpace(r.Models))
			compiled = append(compiled, &rule{
				id:         ruleID(pattern, r.Window),
				pattern:    pattern,
				window:     r.Window,
				limitMilli: r.Limit * milliPerToken,
				enforce:    r.EffectiveMode(globalMode) == config.APIKeyBudgetModeEnforce,
			})
		}
		key := strings.TrimSpace(cfg.APIKeyLabels[label])
		if key == "" {
			continue
		}
		pol.labelByKey[key] = label
		pol.rules[label] = compiled
	}
	if len(pol.rules) == 0 {
		return nil, nil
	}
	return pol, nil
}
