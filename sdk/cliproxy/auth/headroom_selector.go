package auth

import (
	"context"
	"math"
	mathrand "math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// claudeWindows lists the unified subscription windows the headroom selector
// reads, and the utilization at which each window stops a credential from
// receiving new picks while another candidate is below both. The budget left
// above that point is kept for sessions already bound to the credential.
var claudeWindows = [...]struct {
	name      string
	nearLimit float64
}{
	{name: "5h", nearLimit: 0.90},
	{name: "7d", nearLimit: 0.98},
}

// windowUsage is the last known reading of one unified window.
type windowUsage struct {
	used  float64
	reset time.Time
}

// HeadroomSelector picks Claude subscription credentials by the budget they have
// left. Usage comes from the anthropic-ratelimit-unified-5h/7d headers of each
// credential's responses (Auth.Quota.Signals), so it follows whichever
// credentials are enabled and available at pick time without any
// per-credential configuration.
//
// Each pick draws two different candidates at random and keeps the one whose
// busier window has the lower utilization. Credentials therefore converge to a
// similar utilization, so larger subscriptions take a larger share on their
// own, while a burst of picks made before any header updates still spreads over
// several credentials instead of piling onto the least used one. A credential
// that has not reported yet, or whose window has reset, counts as unused.
// Credentials near either limit are skipped while any other candidate is not.
//
// Limits: with only two candidates every pick goes to the less used one until
// a response updates its headers. One of n candidates wins at most 2/n of new
// picks, so a credential holding more of the combined budget than that falls
// behind the others and keeps its spare budget until they reach the near-limit
// threshold.
//
// Readings are remembered per credential until their window resets, because a
// response without a window, a reload of the credential file or a refresh can
// drop it from Quota.Signals. Readings are not persisted: after a restart every
// credential counts as unused until its next response.
//
// Candidate lists with any non-Claude or API-key credential use round-robin:
// API keys report no subscription windows and would always look unused. Wrap
// this selector in SessionAffinitySelector to keep bound sessions on their
// credential.
type HeadroomSelector struct {
	roundRobin RoundRobinSelector
	nowFunc    func() time.Time
	randFunc   func(n int) int

	mu   sync.Mutex
	seen map[string][len(claudeWindows)]windowUsage
}

// Pick selects the next credential for a request.
func (s *HeadroomSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
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

	used, open := s.usage(available, now)
	candidates := open
	if len(candidates) == 0 {
		// Every candidate is near a limit: keep serving the least used rather than
		// failing early. A real rejection still cools the credential.
		candidates = make([]int, len(available))
		for i := range available {
			candidates[i] = i
		}
	}
	picked := available[s.pickLessUsed(used, candidates)]
	log.WithFields(log.Fields{
		"auth":       picked.ID,
		"candidates": len(candidates),
		"available":  len(available),
	}).Debug("headroom pick")
	return picked, nil
}

// pickLessUsed draws two different candidates and returns the index of the one
// with the lower utilization, or of the first drawn on a tie.
func (s *HeadroomSelector) pickLessUsed(used []float64, candidates []int) int {
	if len(candidates) == 1 {
		return candidates[0]
	}
	random := mathrand.IntN
	if s.randFunc != nil {
		random = s.randFunc
	}
	first := random(len(candidates))
	second := random(len(candidates) - 1)
	if second >= first {
		second++
	}
	a, b := candidates[first], candidates[second]
	if used[b] < used[a] {
		return b
	}
	return a
}

// usage returns each credential's busier-window utilization (0 means unused)
// and the indexes of credentials below both near-limit thresholds.
func (s *HeadroomSelector) usage(auths []*Auth, now time.Time) ([]float64, []int) {
	used := make([]float64, len(auths))
	open := make([]int, 0, len(auths))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string][len(claudeWindows)]windowUsage)
	}
	for i, auth := range auths {
		signals := make(map[string]string, len(auth.Quota.Signals))
		for key, value := range auth.Quota.Signals {
			signals[strings.ToLower(key)] = strings.TrimSpace(value)
		}
		known := s.seen[auth.ID]
		nearLimit := false
		for w, window := range claudeWindows {
			if reading, ok := readClaudeWindow(signals, window.name); ok {
				known[w] = reading
			}
			if !known[w].reset.After(now) {
				known[w] = windowUsage{}
				continue
			}
			used[i] = math.Max(used[i], known[w].used)
			nearLimit = nearLimit || known[w].used >= window.nearLimit
		}
		if known == ([len(claudeWindows)]windowUsage{}) {
			delete(s.seen, auth.ID)
		} else {
			s.seen[auth.ID] = known
		}
		if !nearLimit {
			open = append(open, i)
		}
	}
	return used, open
}

// readClaudeWindow parses one unified window from lower-cased signals. A
// rejected window reads as fully used.
func readClaudeWindow(signals map[string]string, window string) (windowUsage, bool) {
	prefix := "anthropic-ratelimit-unified-" + window + "-"
	reset, errReset := strconv.ParseInt(signals[prefix+"reset"], 10, 64)
	if errReset != nil {
		return windowUsage{}, false
	}
	reading := windowUsage{used: 1, reset: time.Unix(reset, 0)}
	if strings.EqualFold(signals[prefix+"status"], "rejected") {
		return reading, true
	}
	used, errUsed := strconv.ParseFloat(signals[prefix+"utilization"], 64)
	if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return windowUsage{}, false
	}
	reading.used = math.Min(used, 1)
	return reading, true
}

// allClaudeSubscriptionAuths reports whether every candidate is a Claude
// credential other than an API key.
func allClaudeSubscriptionAuths(auths []*Auth) bool {
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || auth.AuthKind() == AuthKindAPIKey {
			return false
		}
	}
	return len(auths) > 0
}
