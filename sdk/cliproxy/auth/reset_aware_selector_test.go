package auth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func fixedResetAwareSelector(now time.Time) *ResetAwareSelector {
	return &ResetAwareSelector{nowFunc: func() time.Time { return now }}
}

func TestWeeklyDrainRate(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	weekly := func(used, hours float64) windowUsage {
		return windowUsage{used: used, reset: now.Add(time.Duration(hours * float64(time.Hour)))}
	}
	tests := []struct {
		name   string
		weekly windowUsage
		want   float64
	}{
		{"never seen is a fresh week", windowUsage{}, 1.0 / 168},
		{"reset passed is a fresh week", weekly(0.9, -1), 1.0 / 168},
		{"80% left in 1h", weekly(0.2, 1), 0.8},
		{"30% left in 5 days", weekly(0.7, 120), 0.3 / 120},
		{"a reset in minutes counts as one hour", weekly(0.5, 0.1), 0.5},
		{"used up", weekly(1, 10), 0},
	}
	for _, tt := range tests {
		if got := weeklyDrainRate(tt.weekly, now); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: weeklyDrainRate = %.6f, want %.6f", tt.name, got, tt.want)
		}
	}
}

// The incident this strategy exists for: the credential with 80% of its week left
// and a reset in one hour goes first, even though others have used less.
func TestResetAwareSelectorPick_PrefersBudgetAboutToExpire(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		headroomAuth("least-used", now, 0, 4, 0.05, 150),
		headroomAuth("half-left", now, 0.1, 4, 0.50, 120),
		headroomAuth("resets-in-1h", now, 0.1, 4, 0.20, 1),
	}
	shares := headroomShares(t, fixedResetAwareSelector(now), "claude", auths, 30)
	assertShare(t, shares, "resets-in-1h", 1)
}

// Whatever is left cannot be drained in less than an hour, so a reset in minutes
// does not outrank a reset in one hour with more budget.
func TestResetAwareSelectorPick_ResetHorizonIsBounded(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		headroomAuth("minutes", now, 0.1, 4, 0.50, 0.1),
		headroomAuth("hour", now, 0.1, 4, 0.20, 1),
	}
	shares := headroomShares(t, fixedResetAwareSelector(now), "claude", auths, 10)
	assertShare(t, shares, "hour", 1)
}

func TestResetAwareSelectorPick_SkipsNearLimitUntilItRecovers(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	clock := now
	selector := &ResetAwareSelector{nowFunc: func() time.Time { return clock }}
	steady := headroomAuth("steady", now, 0.1, 4, 0.5, 100)
	hot5h := headroomAuth("hot-5h", now, 0.92, 2, 0.2, 10)
	spent7d := headroomAuth("spent-7d", now, 0.1, 4, 0.99, 1)
	auths := []*Auth{steady, hot5h, spent7d}

	// Both would outscore steady, but one has no 5h room and the other no weekly room.
	shares := headroomShares(t, selector, "claude", auths, 10)
	assertShare(t, shares, "steady", 1)

	// Once its 5h window resets, the credential with the soonest weekly reset is first again.
	clock = now.Add(3 * time.Hour)
	shares = headroomShares(t, selector, "claude", auths, 10)
	assertShare(t, shares, "hot-5h", 1)
}

// When every candidate is near a limit the scores still decide, and a real
// rejection cools the credential and takes it out of the candidates.
func TestResetAwareSelectorPick_ScoresDecideWhenEveryoneIsNearLimit(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	burning := headroomAuth("burning", now, 0.92, 2, 0.3, 100)
	weekly := headroomAuth("weekly", now, 0.2, 4, 0.99, 1)
	shares := headroomShares(t, fixedResetAwareSelector(now), "claude", []*Auth{burning, weekly}, 10)
	assertShare(t, shares, "weekly", 1)
}

// A credential whose weekly window is unknown scores as a fresh week; several of
// them rotate round-robin.
func TestResetAwareSelectorPick_UnobservedCountsAsFreshWeek(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	slow := headroomAuth("slow", now, 0, 4, 0.5, 100)
	soon := headroomAuth("soon", now, 0, 4, 0.1, 10)

	shares := headroomShares(t, fixedResetAwareSelector(now), "claude", []*Auth{slow, unobservedClaudeAuth("new")}, 10)
	assertShare(t, shares, "new", 1)

	shares = headroomShares(t, fixedResetAwareSelector(now), "claude", []*Auth{soon, unobservedClaudeAuth("new")}, 10)
	assertShare(t, shares, "soon", 1)

	rotating := []*Auth{unobservedClaudeAuth("c"), unobservedClaudeAuth("a"), unobservedClaudeAuth("b"), slow}
	shares = headroomShares(t, fixedResetAwareSelector(now), "claude", rotating, 120)
	for _, id := range []string{"a", "b", "c"} {
		assertShare(t, shares, id, 1.0/3)
	}
}

// A response without a window, a reload of the credential file or a refresh can
// drop readings from Quota.Signals. The reading is kept until its window resets,
// and a credential whose week has reset counts as a fresh one.
func TestResetAwareSelectorPick_RemembersWeeklyReadingUntilReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	clock := now
	selector := &ResetAwareSelector{nowFunc: func() time.Time { return clock }}
	soon := headroomAuth("soon", now, 0, 4, 0.2, 1)
	later := headroomAuth("later", now, 0, 4, 0.1, 100)
	auths := []*Auth{soon, later}

	assertShare(t, headroomShares(t, selector, "claude", auths, 10), "soon", 1)

	soon.Quota.Signals = nil
	assertShare(t, headroomShares(t, selector, "claude", auths, 10), "soon", 1)

	clock = now.Add(2 * time.Hour)
	assertShare(t, headroomShares(t, selector, "claude", auths, 10), "later", 1)
}

func TestResetAwareSelectorPick_FollowsLiveMembership(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	soon := headroomAuth("soon", now, 0.1, 4, 0.2, 1)
	soon.Quota.Exceeded = true
	soon.Quota.Reason = "credential_quota"
	soon.Quota.NextRecoverAt = now.Add(time.Hour)
	disabled := headroomAuth("disabled", now, 0.1, 4, 0.05, 1)
	disabled.Disabled = true
	later := headroomAuth("later", now, 0.1, 4, 0.5, 100)
	auths := []*Auth{soon, disabled, later}

	assertShare(t, headroomShares(t, fixedResetAwareSelector(now), "claude", auths, 10), "later", 1)

	// Recovery and re-enabling need no configuration either.
	soon.Quota = QuotaState{Signals: soon.Quota.Signals}
	assertShare(t, headroomShares(t, fixedResetAwareSelector(now), "claude", auths, 10), "soon", 1)
	disabled.Disabled = false
	assertShare(t, headroomShares(t, fixedResetAwareSelector(now), "claude", auths, 10), "disabled", 1)
}

func TestResetAwareSelectorPick_AllCooledReturnsCooldownError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cooled := func(id string) *Auth {
		auth := headroomAuth(id, now, 0.5, 2, 0.5, 50)
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "credential_quota"
		auth.Quota.NextRecoverAt = now.Add(10 * time.Minute)
		return auth
	}
	selector := fixedResetAwareSelector(now)
	_, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{cooled("a"), cooled("b")})
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("Pick() error = %v, want a cooldown error", err)
	}
}

func TestResetAwareSelectorPick_KeepsPriorityTiers(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	high := headroomAuth("high", now, 0.5, 2, 0.9, 100)
	high.Attributes = map[string]string{"priority": "5"}
	low := headroomAuth("low", now, 0, 4, 0.1, 1)
	shares := headroomShares(t, fixedResetAwareSelector(now), "claude", []*Auth{high, low}, 10)
	assertShare(t, shares, "high", 1)
}

func TestResetAwareSelectorPick_NonClaudeCandidatesUseRoundRobin(t *testing.T) {
	t.Parallel()

	apiKey := map[string]string{AttributeAPIKey: "test-key"}
	for name, auths := range map[string][]*Auth{
		"codex": {{ID: "b", Provider: "codex"}, {ID: "a", Provider: "codex"}, {ID: "c", Provider: "codex"}},
		"mixed": {{ID: "b", Provider: "claude"}, {ID: "a", Provider: "codex"}, {ID: "c", Provider: "claude"}},
		// API keys report no subscription windows, so they would always look like a fresh week.
		"claude-api-key": {{ID: "b", Provider: "claude"}, {ID: "a", Provider: "claude", Attributes: apiKey}, {ID: "c", Provider: "claude"}},
	} {
		provider := name
		if name == "claude-api-key" {
			provider = "claude"
		}
		selector := &ResetAwareSelector{}
		for i, want := range []string{"a", "b", "c", "a"} {
			got, err := selector.Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("%s Pick() #%d error = %v", name, i, err)
			}
			if got.ID != want {
				t.Fatalf("%s Pick() #%d = %q, want %q", name, i, got.ID, want)
			}
		}
	}
}

// The mixed-provider path passes "mixed" as the provider; Claude-only candidate
// lists still pick by reset.
func TestResetAwareSelectorPick_MixedPathWithClaudeCandidates(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{headroomAuth("later", now, 0.1, 4, 0.5, 100), headroomAuth("soon", now, 0.1, 4, 0.2, 1)}
	shares := headroomShares(t, fixedResetAwareSelector(now), "mixed", auths, 10)
	assertShare(t, shares, "soon", 1)
}

// Credentials converge on draining at the same relative rate: the one whose week
// ends first is drained first and the one with the latest reset is left alone
// while the others still have more budget per hour to give.
func TestResetAwareSelector_DrainsSoonestResetFirst(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	const size = 1000.0
	ids := []string{"2h", "24h", "120h"}
	hours := map[string]float64{"2h": 2, "24h": 24, "120h": 120}
	spent := map[string]float64{}
	auths := make([]*Auth, len(ids))
	for i, id := range ids {
		auths[i] = headroomAuth(id, now, 0, 4, 0, hours[id])
	}
	selector := fixedResetAwareSelector(now)
	for i := 0; i < 1200; i++ {
		got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		spent[got.ID]++
		// The response headers report the new utilization of the picked credential only.
		setHeadroomSignals(got, now, 0, 4, spent[got.ID]/size, hours[got.ID])
	}

	if spent["120h"] != 0 {
		t.Fatalf("the latest reset was used %v times while earlier resets still had budget left (spent %v)", spent["120h"], spent)
	}
	rate := func(id string) float64 { return (1 - spent[id]/size) / hours[id] }
	if spread := math.Abs(rate("2h") - rate("24h")); spread > 0.002 {
		t.Fatalf("drain rates differ by %.4f, want them to converge (spent %v)", spread, spent)
	}
	if spent["2h"]/size < 0.9 {
		t.Fatalf("soonest reset used %.2f of its week, want it drained first (spent %v)", spent["2h"]/size, spent)
	}
	if spent["2h"]/size >= claudeWindows[weeklyWindow].nearLimit {
		t.Fatalf("soonest reset reached %.3f of its week while others had budget left", spent["2h"]/size)
	}
}

func TestResetAwareSelectorPick_ConcurrentPicks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{headroomAuth("a", now, 0.1, 4, 0.2, 10), headroomAuth("b", now, 0.3, 4, 0.1, 100), unobservedClaudeAuth("c")}
	selector := &ResetAwareSelector{}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths); err != nil {
					t.Errorf("Pick() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestResetAwareSelectorUnderSessionAffinityKeepsBoundSessions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	a := headroomAuth("a", now, 0.1, 4, 0.1, 100)
	b := headroomAuth("b", now, 0.1, 4, 0.1, 100)
	auths := []*Auth{a, b}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &ResetAwareSelector{},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)

	headers := make(http.Header)
	headers.Set("X-Session-ID", "bound-session")
	opts := cliproxyexecutor.Options{Headers: headers}
	first, err := selector.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	other := b
	if first.ID == "b" {
		other = a
	}

	// The other credential now has the soonest reset, but the bound session stays
	// on its credential to keep its prompt cache.
	setHeadroomSignals(first, now, 0.1, 4, 0.5, 100)
	setHeadroomSignals(other, now, 0.1, 4, 0.2, 1)
	for i := 0; i < 5; i++ {
		got, errPick := selector.Pick(context.Background(), "claude", "", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		if got.ID != first.ID {
			t.Fatalf("bound session moved from %s to %s while its credential was usable", first.ID, got.ID)
		}
	}

	// Once the bound credential is cooled, the session fails over to the other one.
	first.Quota.Exceeded = true
	first.Quota.Reason = "credential_quota"
	first.Quota.NextRecoverAt = now.Add(time.Hour)
	got, err := selector.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("failover Pick() error = %v", err)
	}
	if got.ID != other.ID {
		t.Fatalf("failover picked %s, want %s", got.ID, other.ID)
	}
}

// Through the manager's real pick path: new sessions follow the weekly reset each
// credential reports in its response headers, while a bound session stays on its
// credential.
func TestManagerResetAwareRoutingWithSessionAffinity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &ResetAwareSelector{},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	for _, id := range []string{"a", "b"} {
		if _, errRegister := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "test-" + id}}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	pick := func(session string) string {
		t.Helper()
		headers := make(http.Header)
		headers.Set("X-Session-ID", session)
		auth, _, errPick := manager.pickNext(ctx, "claude", "", cliproxyexecutor.Options{Headers: headers}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(%s) error = %v", session, errPick)
		}
		return auth.ID
	}
	report := func(id string, used7d float64, resetIn time.Duration) {
		resultCtx := internallogging.WithResponseHeadersHolder(ctx)
		internallogging.SetResponseHeaders(resultCtx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{strconv.FormatFloat(used7d, 'f', 4, 64)},
			"Anthropic-Ratelimit-Unified-7d-Reset":       []string{strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10)},
		})
		manager.MarkResult(resultCtx, Result{AuthID: id, Provider: "claude", Success: true})
	}

	bound := pick("bound")
	soon := "a"
	if bound == "a" {
		soon = "b"
	}
	report(bound, 0.5, 100*time.Hour)
	report(soon, 0.2, time.Hour)

	for i := 0; i < 20; i++ {
		if got := pick("new-" + strconv.Itoa(i)); got != soon {
			t.Fatalf("new session #%d went to %s, want %s (80%% of its week left, resets in 1h)", i, got, soon)
		}
	}
	for turn := 0; turn < 20; turn++ {
		if got := pick("bound"); got != bound {
			t.Fatalf("bound session moved from %s to %s on turn %d", bound, got, turn)
		}
	}
}

func TestSelectorContextTreatsResetAwareCandidatesAsValidated(t *testing.T) {
	t.Parallel()

	ctx := selectorContextForAvailableAuths(context.Background(), &ResetAwareSelector{}, "claude-sonnet-5-5")
	if validated, _ := ctx.Value(prevalidatedAuthCandidatesKey{}).(bool); !validated {
		t.Fatal("reset-aware selector context is not marked as carrying prevalidated candidates")
	}
}
