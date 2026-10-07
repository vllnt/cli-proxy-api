package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// DefaultQuotaMaxAge bounds how long a passive quota snapshot guides routing.
const DefaultQuotaMaxAge = 5 * time.Minute

// QuotaAwareSelector narrows new-session candidates using existing passive Claude
// and Codex quota observations, then delegates ties and unknown data to the configured strategy.
// Put session affinity outside this selector so a better score never moves a
// usable binding. No observations or responses are cached here.
type QuotaAwareSelector struct {
	Fallback        Selector
	MaxAge          time.Duration
	nowFunc         func() time.Time
	defaultFallback RoundRobinSelector
}

func (s *QuotaAwareSelector) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

func (s *QuotaAwareSelector) maxAge() time.Duration {
	if s.MaxAge > 0 {
		return s.MaxAge
	}
	return DefaultQuotaMaxAge
}

func (s *QuotaAwareSelector) fallback() Selector {
	if s.Fallback != nil {
		return s.Fallback
	}
	return &s.defaultFallback
}

// Pick preserves eligibility, priority, weight exclusion and Codex transport
// preferences before ranking by the earliest valid future reset across shared
// windows, then most headroom in the tightest observed window. If any
// candidate has unknown data, delegate the whole usable pool rather than assume
// that account has either full or empty quota.
func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	fallback := s.fallback()
	auths = selectorWeightCandidates(fallback, auths)
	available, errAvailable := getSelectorAvailableAuthsWithQuota(ctx, auths, provider, model, now, true, s)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferWebsocketAuths(ctx, provider, highestPriorityAuths(available))

	best := quotaRoutingView{}
	var ranked []*Auth
	for _, candidate := range available {
		view := quotaViewForAuth(candidate, now, s.maxAge())
		if !view.known {
			return fallback.Pick(ctx, provider, model, opts, available)
		}
		if len(ranked) == 0 || view.reset.Before(best.reset) || (view.reset.Equal(best.reset) && view.remaining > best.remaining) {
			best = view
			ranked = []*Auth{candidate}
		} else if view.remaining == best.remaining && view.reset.Equal(best.reset) {
			ranked = append(ranked, candidate)
		}
	}
	return fallback.Pick(ctx, provider, model, opts, ranked)
}

// usable excludes only fresh, explicitly exhausted windows with a future
// recovery time. An account needs its latest exhausted reset, but an observation
// must stop blocking once it becomes stale. The earliest account recovery wins.
func (s *QuotaAwareSelector) usable(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	usable := make([]*Auth, 0, len(auths))
	var earliest time.Time
	for _, candidate := range auths {
		recovery := s.recoveryForAuth(candidate, now)
		if recovery.IsZero() {
			usable = append(usable, candidate)
		} else if earliest.IsZero() || recovery.Before(earliest) {
			earliest = recovery
		}
	}
	if len(usable) == 0 && !earliest.IsZero() {
		if provider == "mixed" {
			provider = ""
		}
		return nil, newModelCooldownError(model, provider, earliest.Sub(now))
	}
	return usable, nil
}

// recoveryForAuth bounds only the passive block by snapshot expiry. An ordinary
// model/credential cooldown may last longer and must never be shortened by it.
func (s *QuotaAwareSelector) recoveryForAuth(auth *Auth, now time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	recovery := quotaViewForAuth(auth, now, s.maxAge()).blockedUntil
	if !recovery.IsZero() {
		if expires := auth.Quota.ObservedAt.Add(s.maxAge()); expires.Before(recovery) {
			recovery = expires
		}
	}
	return recovery
}

// authBlockWithQuota combines both availability owners before candidates are
// removed. An account must clear every block (max); selection/retry can then
// choose the earliest account (min). Permanent ordinary failures stay permanent.
func authBlockWithQuota(auth *Auth, model string, now time.Time, aware *QuotaAwareSelector) (bool, blockReason, time.Time) {
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if recovery := aware.recoveryForAuth(auth, now); !recovery.IsZero() {
		if !blocked {
			return true, blockReasonCooldown, recovery
		}
		if !next.IsZero() && reason != blockReasonDisabled && recovery.After(next) {
			next = recovery
		}
	}
	return blocked, reason, next
}

func quotaSelector(selector Selector) *QuotaAwareSelector {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	aware, _ := selector.(*QuotaAwareSelector)
	return aware
}

func selectorWeightCandidates(selector Selector, auths []*Auth) []*Auth {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	if aware, ok := selector.(*QuotaAwareSelector); ok {
		selector = aware.fallback()
	}
	if _, weighted := selector.(*WeightedRoundRobinSelector); weighted {
		return positiveWeightAuths(auths)
	}
	return auths
}

// selectorUsableAuths is shared by explicit and LCP affinity paths. Reset order
// and headroom affect only fallback picks; exhaustion affects binding membership.
func selectorUsableAuths(ctx context.Context, selector Selector, auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	return getSelectorAvailableAuthsWithQuota(ctx, selectorWeightCandidates(selector, auths), provider, model, now, true, quotaSelector(selector))
}

type quotaRoutingView struct {
	known        bool
	remaining    float64
	reset        time.Time
	blockedUntil time.Time
}

// Ranking uses the earliest observed replenishment and the minimum headroom
// independently. Exhaustion instead waits for the latest rejected-window reset,
// so a short-window reset never makes a rejected weekly window usable.
func (v *quotaRoutingView) addWindow(remaining float64, valid bool, reset, now time.Time, exhausted bool) {
	if !reset.After(now) {
		return
	}
	if exhausted && reset.After(v.blockedUntil) {
		v.blockedUntil = reset
	}
	if valid {
		if !v.known || remaining < v.remaining {
			v.remaining = remaining
		}
		if !v.known || reset.Before(v.reset) {
			v.reset = reset
		}
		v.known = true
	}
}

// Only shared subscription windows are comparable. Model-specific Claude
// overage/Fable and Codex additional/code-review limits must not drain unrelated
// models. Existing model cooldowns continue to own those failures.
func quotaViewForAuth(auth *Auth, now time.Time, maxAge time.Duration) quotaRoutingView {
	var view quotaRoutingView
	if auth == nil || auth.Quota.ObservedAt.IsZero() || auth.Quota.ObservedAt.After(now) || now.Sub(auth.Quota.ObservedAt) >= maxAge {
		return view
	}
	q := auth.Quota
	// Observations are normally canonical headers; normalize once for direct SDK
	// callers as well.
	signals := make(map[string]string, len(q.Signals))
	for key, value := range q.Signals {
		name, normalized := strings.ToLower(key), strings.TrimSpace(value)
		if previous, exists := signals[name]; exists && previous != normalized {
			return view // Conflicting case variants are not trustworthy.
		}
		signals[name] = normalized
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		sharedWindowRejected := false
		for _, window := range []string{"5h", "7d"} {
			prefix := "anthropic-ratelimit-unified-" + window + "-"
			used, valid := quotaNumber(signals[prefix+"utilization"], 1)
			reset := quotaReset(signals[prefix+"reset"])
			exhausted := strings.EqualFold(signals[prefix+"status"], "rejected")
			if exhausted {
				sharedWindowRejected = true
			}
			view.addWindow(1-used, valid, reset, now, exhausted)
		}
		// Older Claude responses may expose only the aggregate status/reset.
		// Treat that as a shared-window rejection unless the provider explicitly
		// identifies an overage/Fable-only rejection.
		overageOnly := strings.EqualFold(signals["anthropic-ratelimit-unified-7d_oi-status"], "rejected") ||
			strings.EqualFold(signals["anthropic-ratelimit-unified-overage-status"], "rejected") ||
			strings.TrimSpace(signals["anthropic-ratelimit-unified-overage-disabled-reason"]) != "" ||
			strings.Contains(strings.ToLower(signals["anthropic-ratelimit-unified-representative-claim"]), "overage")
		if !sharedWindowRejected && strings.EqualFold(signals["anthropic-ratelimit-unified-status"], "rejected") && !overageOnly {
			view.addWindow(0, false, quotaReset(signals["anthropic-ratelimit-unified-reset"]), now, true)
		}
	case "codex":
		explicitlyAllowed := strings.EqualFold(signals["x-codex-allowed"], "true")
		explicitlyRejected := strings.EqualFold(signals["x-codex-allowed"], "false") || strings.EqualFold(signals["x-codex-limit-reached"], "true")
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window + "-"
			used, valid := quotaNumber(signals[prefix+"used-percent"], 100)
			reset := quotaReset(signals[prefix+"reset-at"])
			// Prefer the absolute timestamp only while it is usable. Some Codex
			// responses carry an old reset-at together with a fresh relative
			// reset-after-seconds; the latter is the actionable recovery hint.
			if !reset.After(now) {
				reset = time.Time{}
			}
			if reset.IsZero() {
				if seconds, errParse := strconv.ParseInt(signals[prefix+"reset-after-seconds"], 10, 64); errParse == nil && seconds >= 0 && seconds <= math.MaxInt64/int64(time.Second) {
					reset = q.ObservedAt.Add(time.Duration(seconds) * time.Second)
				}
			}
			// Allowed/credits may permit continued use at 100%. Only an explicit
			// rejection blocks; a percentage alone is a ranking hint. A rejected
			// window remains actionable even when its usage percentage is omitted.
			view.addWindow(1-used/100, valid, reset, now, explicitlyRejected && !explicitlyAllowed)
		}
	}
	return view
}

func quotaNumber(raw string, max float64) (float64, bool) {
	value, errParse := strconv.ParseFloat(raw, 64)
	return value, errParse == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= max
}

func quotaReset(raw string) time.Time {
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
		// Limit Unix timestamps to the RFC3339 range and reject millisecond epochs.
		if seconds <= 253402300799 {
			return time.Unix(seconds, 0)
		}
		return time.Time{}
	}
	reset, _ := time.Parse(time.RFC3339, raw)
	return reset
}
