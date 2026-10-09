package auth

import (
	"context"
	"math"
	mathrand "math/rand/v2"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
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
	randFunc        func() float64
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
// preferences before pacing healthy candidates by real headroom. Unknown data
// delegates the whole usable pool to the configured strategy.
func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	fallback := s.fallback()
	auths = selectorWeightCandidates(fallback, auths)
	available, errAvailable := getSelectorAvailableAuthsWithQuota(ctx, auths, provider, model, now, true, s)
	if errAvailable != nil {
		return nil, errAvailable
	}
	views := make([]quotaRoutingView, len(available))
	knownCount := 0
	knownScoreTotal := 0.0
	for i, candidate := range available {
		views[i] = quotaViewForAuth(candidate, now, s.maxAge())
		if views[i].known {
			knownCount++
			knownScoreTotal += views[i].score
		}
	}
	if knownCount > 0 {
		// Unknown snapshots are neutral, not a synthetic best score. Cap the
		// neutral weight at the unit baseline so a stale credential cannot
		// outrank a known cool account; when all known accounts are hot, use
		// their mean so the unknown account remains an unbiased fallback.
		neutralScore := math.Min(1, knownScoreTotal/float64(knownCount))
		for i := range views {
			if views[i].known {
				continue
			}
			views[i].score = neutralScore
			views[i].reason = "unknown"
		}
	}
	for i, candidate := range available {
		if !views[i].known {
			// Missing or stale telemetry is neutral: it must not look like free
			// capacity, and it must not discard known headroom from other accounts.
			// If every candidate is unknown, preserve the configured strategy exactly.
			if knownCount == 0 {
				views[i].score = 1
				views[i].reason = "unknown"
			}
		}
		log.WithFields(log.Fields{
			"auth":     candidate.ID,
			"priority": authPriority(candidate),
			"score":    views[i].score,
			"hot":      views[i].hot,
			"reason":   views[i].reason,
		}).Debug("quota-aware candidate")
	}
	if knownCount == 0 {
		available = preferWebsocketAuths(ctx, provider, available)
		return fallback.Pick(ctx, provider, model, opts, available)
	}
	beforeTier := available
	beforeTierViews := views
	available, views = pacedPriorityTier(available, views)
	selectedIDs := make(map[string]struct{}, len(available))
	for _, candidate := range available {
		selectedIDs[candidate.ID] = struct{}{}
	}
	for i, candidate := range beforeTier {
		if _, selected := selectedIDs[candidate.ID]; selected {
			continue
		}
		reason := beforeTierViews[i].reason
		if reason == "" {
			reason = "lower-priority"
		}
		log.WithFields(log.Fields{
			"auth":     candidate.ID,
			"priority": authPriority(candidate),
			"score":    beforeTierViews[i].score,
			"hot":      beforeTierViews[i].hot,
			"reason":   reason,
		}).Debug("quota-aware candidate skipped")
	}
	preferred := preferWebsocketAuths(ctx, provider, available)
	if len(preferred) != len(available) {
		byID := make(map[string]quotaRoutingView, len(views))
		for i, candidate := range available {
			byID[candidate.ID] = views[i]
		}
		filteredViews := make([]quotaRoutingView, 0, len(preferred))
		for _, candidate := range preferred {
			filteredViews = append(filteredViews, byID[candidate.ID])
		}
		views = filteredViews
	}
	available = preferred
	selected, okSelected := s.pickWeighted(available, views, fallback)
	if !okSelected {
		return fallback.Pick(ctx, provider, model, opts, available)
	}
	selectedView := quotaRoutingView{}
	for i, candidate := range available {
		if candidate.ID == selected.ID {
			selectedView = views[i]
			break
		}
	}
	log.WithFields(log.Fields{
		"auth":   selected.ID,
		"score":  selectedView.score,
		"hot":    selectedView.hot,
		"reason": selectedView.reason,
	}).Debug("quota-aware selected credential")
	return selected, nil
}

func pacedPriorityTier(auths []*Auth, views []quotaRoutingView) ([]*Auth, []quotaRoutingView) {
	if len(auths) == 0 || len(auths) != len(views) {
		return auths, views
	}
	maxPriority := authPriority(auths[0])
	minPriority := maxPriority
	for _, auth := range auths[1:] {
		priority := authPriority(auth)
		if priority > maxPriority {
			maxPriority = priority
		}
		if priority < minPriority {
			minPriority = priority
		}
	}
	chosenPriority := maxPriority
	for priority := maxPriority; priority >= minPriority; priority-- {
		for i, auth := range auths {
			if authPriority(auth) == priority && !views[i].hot {
				chosenPriority = priority
				return filterPriorityTier(auths, views, chosenPriority)
			}
		}
	}
	return filterPriorityTier(auths, views, chosenPriority)
}

func filterPriorityTier(auths []*Auth, views []quotaRoutingView, priority int) ([]*Auth, []quotaRoutingView) {
	selectedAuths := make([]*Auth, 0, len(auths))
	selectedViews := make([]quotaRoutingView, 0, len(views))
	for i, auth := range auths {
		if authPriority(auth) == priority {
			selectedAuths = append(selectedAuths, auth)
			selectedViews = append(selectedViews, views[i])
		}
	}
	return selectedAuths, selectedViews
}

func (s *QuotaAwareSelector) randomFloat64() float64 {
	if s != nil && s.randFunc != nil {
		value := s.randFunc()
		if value >= 0 && value < 1 {
			return value
		}
	}
	return mathrand.Float64()
}

func (s *QuotaAwareSelector) pickWeighted(auths []*Auth, views []quotaRoutingView, fallback Selector) (*Auth, bool) {
	if len(auths) == 0 || len(auths) != len(views) {
		return nil, false
	}
	weighted := make([]float64, len(auths))
	var total float64
	_, configuredWeights := fallback.(*WeightedRoundRobinSelector)
	for i, view := range views {
		weight := view.score
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			weight = 0.01
		}
		if configuredWeights {
			weight *= float64(authWeight(auths[i]))
		}
		// Plan type only breaks near-equal quota scores; it cannot override a
		// materially hotter account or a higher priority tier.
		weight *= 1 + float64(view.planTypeRank)*0.02
		weighted[i] = weight
		total += weight
	}
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return nil, false
	}
	target := s.randomFloat64() * total
	for i, weight := range weighted {
		if target < weight {
			return auths[i], true
		}
		target -= weight
	}
	return auths[len(auths)-1], true
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

// schedulerCandidates applies the same priority and pacing gate used by Pick
// before an external scheduler is allowed to choose an auth. Unknown telemetry
// remains neutral, but still respects the highest configured priority tier.
func (s *QuotaAwareSelector) schedulerCandidates(ctx context.Context, provider, model string, auths []*Auth) []*Auth {
	if s == nil || len(auths) == 0 {
		return auths
	}
	now := s.now()
	auths = selectorWeightCandidates(s, auths)
	available, errAvailable := getSelectorAvailableAuthsWithQuota(ctx, auths, provider, model, now, true, s)
	if errAvailable != nil || len(available) == 0 {
		return available
	}
	views := make([]quotaRoutingView, len(available))
	knownCount := 0
	for i, candidate := range available {
		views[i] = quotaViewForAuth(candidate, now, s.maxAge())
		if !views[i].known {
			views[i].score = 1
			views[i].reason = "unknown"
		} else {
			knownCount++
		}
	}
	if knownCount == 0 {
		return highestPriorityAuths(available)
	}
	available, _ = pacedPriorityTier(available, views)
	return preferWebsocketAuths(ctx, provider, available)
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
	score        float64
	hot          bool
	reason       string
	planTypeRank int
	paceFactor   float64
	credits      bool
	resetsLeft   int64
	resetExpiry  time.Time
}

// addWindow turns raw remaining quota into a paced score. A score near 1 means
// the account is consuming at its window pace; below 1 is hot and above 1 is
// cool. Reset urgency is deliberately a modest bonus so "use it or lose it"
// capacity wins only when headroom is otherwise comparable.
func (v *quotaRoutingView) addWindow(remaining float64, valid bool, reset, now time.Time, exhausted bool, duration time.Duration) {
	if !reset.After(now) {
		return
	}
	if exhausted && reset.After(v.blockedUntil) {
		v.blockedUntil = reset
	}
	if !valid {
		return
	}
	remaining = math.Max(0, math.Min(1, remaining))
	elapsed := 0.0
	if duration > 0 {
		elapsed = math.Max(0, math.Min(1, now.Sub(reset.Add(-duration)).Seconds()/duration.Seconds()))
	}
	remainingTime := math.Max(1-elapsed, 0.05)
	paced := remaining / remainingTime
	urgency := 1 + 0.2*elapsed
	score := remaining * paced * urgency
	if !v.known || score < v.score {
		v.score = score
	}
	if !v.known || paced < v.paceFactor {
		v.paceFactor = paced
	}
	if !v.known || remaining < v.remaining {
		v.remaining = remaining
	}
	if !v.known || reset.Before(v.reset) {
		v.reset = reset
	}
	v.known = true
	if paced < 1 && !v.hot {
		v.hot = true
		v.reason = "hot"
	} else if v.reason == "" {
		v.reason = "cool"
	}
}

func (v *quotaRoutingView) addCredits(score float64, reset, now time.Time) {
	if !v.known || score <= 0 {
		return
	}
	v.credits = true
	if v.score < score {
		v.score = score
	}
	if !reset.IsZero() && (v.reset.IsZero() || reset.Before(v.reset)) {
		v.reset = reset
	}
	if v.hot && score >= 0.5 {
		v.reason = "credits-overflow"
		v.hot = false
	}
}

func (v *quotaRoutingView) addResetMetadata(left int64, expiry, now time.Time) {
	if left >= 0 {
		v.resetsLeft = left
	}
	if expiry.After(now) {
		v.resetExpiry = expiry
		if !v.reset.IsZero() && expiry.Before(v.reset) {
			v.reset = expiry
		}
	}
	if left > 0 && v.known {
		v.score *= 1 + math.Min(float64(left), 5)*0.03
	}
}

func codexPlanTypeRank(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enterprise":
		return 4
	case "team":
		return 3
	case "pro":
		return 2
	case "plus":
		return 1
	default:
		return 0
	}
}

func parseQuotaInt(raw string) (int64, bool) {
	value, errParse := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return value, errParse == nil && value >= 0
}

func quotaDuration(provider, window string, signals map[string]string) time.Duration {
	if provider == "claude" {
		if window == "5h" {
			return 5 * time.Hour
		}
		if window == "7d" {
			return 7 * 24 * time.Hour
		}
	}
	if provider == "codex" {
		if minutes, ok := parseQuotaInt(signals["x-codex-"+window+"-window-minutes"]); ok && minutes > 0 {
			return time.Duration(minutes) * time.Minute
		}
		// Current Codex usage events identify the primary window as 7d and
		// secondary as 5h; the header remains authoritative when present.
		if window == "primary" {
			return 7 * 24 * time.Hour
		}
		if window == "secondary" {
			return 5 * time.Hour
		}
	}
	return 0
}

func quotaExpiry(signals map[string]string, prefix string) time.Time {
	for _, key := range []string{prefix + "reset-expiry", prefix + "expires-at", prefix + "expiry"} {
		if value := quotaReset(signals[key]); !value.IsZero() {
			return value
		}
	}
	return time.Time{}
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
			if duration := quotaDuration("claude", window, signals); duration == 0 || now.Sub(auth.Quota.ObservedAt) < duration {
				view.addWindow(1-used, valid, reset, now, exhausted, duration)
			}
		}
		// Older Claude responses may expose only the aggregate status/reset.
		// Ignore an aggregate rejection only when the existing Claude classifier
		// can prove that the rejection is overage/Fable-only and a shared window
		// is explicitly healthy.
		overageOnly := claudeOverageOnly(signals)
		if !sharedWindowRejected && strings.EqualFold(signals["anthropic-ratelimit-unified-status"], "rejected") && !overageOnly {
			view.addWindow(0, false, quotaReset(signals["anthropic-ratelimit-unified-reset"]), now, true, 0)
		}
	case "codex":
		explicitlyAllowed := strings.EqualFold(signals["x-codex-allowed"], "true")
		explicitlyRejected := strings.EqualFold(signals["x-codex-allowed"], "false") || strings.EqualFold(signals["x-codex-limit-reached"], "true")
		creditsAvailable, creditScore := codexCredits(signals)
		planType := signals["x-codex-plan-type"]
		if planType == "" && auth.Attributes != nil {
			planType = auth.Attributes["plan_type"]
		}
		if planType == "" && auth.Metadata != nil {
			if rawPlan, okPlan := auth.Metadata["plan_type"].(string); okPlan {
				planType = rawPlan
			}
		}
		view.planTypeRank = codexPlanTypeRank(planType)
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
			duration := quotaDuration("codex", window, signals)
			if duration == 0 || now.Sub(q.ObservedAt) < duration {
				view.addWindow(1-used/100, valid, reset, now, explicitlyRejected && !explicitlyAllowed && !creditsAvailable, duration)
			}
			if left, ok := parseQuotaInt(signals[prefix+"resets-left"]); ok {
				view.addResetMetadata(left, quotaExpiry(signals, prefix), now)
			}
		}
		if creditsAvailable {
			view.addCredits(creditScore, view.reset, now)
		}
	}
	return view
}

func claudeOverageOnly(signals map[string]string) bool {
	status5h := strings.ToLower(strings.TrimSpace(signals["anthropic-ratelimit-unified-5h-status"]))
	status7d := strings.ToLower(strings.TrimSpace(signals["anthropic-ratelimit-unified-7d-status"]))
	if status5h == "rejected" || status7d == "rejected" {
		return false
	}
	status7dOI := strings.ToLower(strings.TrimSpace(signals["anthropic-ratelimit-unified-7d_oi-status"]))
	overageRejected := status7dOI == "rejected" ||
		strings.EqualFold(signals["anthropic-ratelimit-unified-overage-status"], "rejected") ||
		strings.TrimSpace(signals["anthropic-ratelimit-unified-overage-disabled-reason"]) != "" ||
		strings.Contains(strings.ToLower(signals["anthropic-ratelimit-unified-representative-claim"]), "overage")
	if !overageRejected {
		return false
	}
	isAllowed := func(status string) bool { return status == "allowed" || status == "allowed_warning" }
	if isAllowed(status5h) && isAllowed(status7d) {
		return true
	}
	// Anthropic sometimes omits one status while reporting healthy utilization.
	if isAllowed(status7d) && status5h == "" {
		if utilization, ok := quotaNumber(signals["anthropic-ratelimit-unified-5h-utilization"], 1); ok && utilization < 1 {
			return true
		}
	}
	if isAllowed(status5h) && status7d == "" {
		if utilization, ok := quotaNumber(signals["anthropic-ratelimit-unified-7d-utilization"], 1); ok && utilization < 1 {
			return true
		}
	}
	return false
}

func codexCredits(signals map[string]string) (bool, float64) {
	if strings.EqualFold(signals["x-codex-credits-unlimited"], "true") {
		return true, 1
	}
	if balance, errParse := strconv.ParseFloat(signals["x-codex-credits-balance"], 64); errParse == nil && !math.IsNaN(balance) && !math.IsInf(balance, 0) {
		if balance > 0 {
			return true, 1
		}
		return false, 0
	}
	if strings.EqualFold(signals["x-codex-credits-has-credits"], "true") {
		return true, 1
	}
	return false, 0
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
