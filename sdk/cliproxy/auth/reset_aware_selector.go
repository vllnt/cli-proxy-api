package auth

import (
	"context"
	"math"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	// weekLength is the length of the 7d window, assumed for a credential whose
	// weekly window is unknown or has reset.
	weekLength = 7 * 24 * time.Hour
	// minResetHorizon bounds the score of a window about to reset: whatever is
	// left cannot be drained in less time than this.
	minResetHorizon = time.Hour
)

// ResetAwareSelector picks Claude subscription credentials whose weekly budget
// is closest to expiring. Budget left when the 7d window resets is lost, so the
// selector prefers the credential that must be used fastest to avoid that: the
// highest fraction of the weekly window left per hour until it resets. A
// credential with 80% left that resets in one hour (0.8 per hour) goes before
// one with 30% left that resets in five days (0.0025 per hour). Credentials
// converge on draining at the same relative rate, so all of them end the week
// near empty instead of the ones that happen to be picked first.
//
// The 5h window caps how fast a credential can be drained, so credentials near
// either limit (see claudeWindows) get no new picks while another is below, and
// a credential whose 5h window frees up again can lead again. When every
// candidate is near a limit the scores decide anyway: a real rejection cools the
// credential and takes it out of the candidates.
//
// Usage comes from the anthropic-ratelimit-unified-5h/7d headers of each
// credential's responses (Auth.Quota.Signals). Readings are not persisted, so
// after a restart no credential has one. A credential with no reading ties with
// the best-scoring one until its first response reports its real windows;
// otherwise the first credential to report would capture every new session and
// hide one whose week is about to expire. A credential with only a 5h reading
// scores its weekly window as a fresh week. Ties rotate round-robin.
//
// Limits: picks follow the last known readings, so a burst of new sessions made
// before the next response updates them all land on the top-scoring credential,
// and the 5h near-limit threshold moves the rest to the next. A credential that
// never reports a window keeps sharing the lead. Weights and per-credential
// sizes are not needed because windows report fractions.
//
// Candidate lists with any non-Claude or API-key credential use round-robin: API
// keys report no subscription windows. Wrap this selector in
// SessionAffinitySelector to keep bound sessions on their credential; only new
// sessions and failovers are routed by this selector.
type ResetAwareSelector struct {
	claudeUsageMemory
	roundRobin RoundRobinSelector
	nowFunc    func() time.Time
}

// Pick selects the next credential for a request.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if !allClaudeSubscriptionAuths(auths) {
		return s.roundRobin.Pick(ctx, provider, model, opts, auths)
	}
	now := time.Now()
	if s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}

	readings := s.readWindows(available, now)
	scores := make([]float64, len(available))
	open := make([]int, 0, len(available))
	for i, windows := range readings {
		scores[i] = weeklyDrainRate(windows[weeklyWindow], now)
		if !nearAnyLimit(windows) {
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		for i := range available {
			open = append(open, i)
		}
	}
	var noReading [len(claudeWindows)]windowUsage
	lead := 0.0
	for _, i := range open {
		if readings[i] != noReading {
			lead = math.Max(lead, scores[i])
		}
	}
	for _, i := range open {
		if readings[i] == noReading {
			scores[i] = lead
		}
	}

	best := []int{open[0]}
	for _, i := range open[1:] {
		switch {
		case scores[i] > scores[best[0]]:
			best = []int{i}
		case scores[i] == scores[best[0]]:
			best = append(best, i)
		}
	}
	tied := make([]*Auth, len(best))
	for i, index := range best {
		tied[i] = available[index]
	}
	picked := tied[0]
	if len(tied) > 1 {
		var errPick error
		if picked, errPick = s.roundRobin.Pick(ctx, provider, model, opts, tied); errPick != nil {
			return nil, errPick
		}
	}
	log.WithFields(log.Fields{
		"auth":       picked.ID,
		"candidates": len(open),
		"available":  len(available),
		"tied":       len(tied),
	}).Debug("reset-aware pick")
	return picked, nil
}

// weeklyDrainRate scores a credential by the fraction of the weekly window left
// per hour until it resets. A window that was never seen or has reset counts as
// a fresh week.
func weeklyDrainRate(weekly windowUsage, now time.Time) float64 {
	left, untilReset := 1.0, weekLength
	if weekly.reset.After(now) {
		left, untilReset = 1-weekly.used, weekly.reset.Sub(now)
	}
	if untilReset < minResetHorizon {
		untilReset = minResetHorizon
	}
	return left / untilReset.Hours()
}
