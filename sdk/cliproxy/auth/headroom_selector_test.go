package auth

import (
	"context"
	"errors"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// headroomAuth builds a Claude credential whose last response reported the
// given 5h and 7d utilizations (0..1) with resets h5 and h7 hours from now.
func headroomAuth(id string, now time.Time, u5, h5, u7, h7 float64) *Auth {
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive}
	setHeadroomSignals(auth, now, u5, h5, u7, h7)
	return auth
}

func setHeadroomSignals(auth *Auth, now time.Time, u5, h5, u7, h7 float64) {
	reset := func(hours float64) string {
		return strconv.FormatInt(now.Add(time.Duration(hours*float64(time.Hour))).Unix(), 10)
	}
	auth.Quota.ObservedAt = now
	auth.Quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(u5, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-5h-Reset":       reset(h5),
		"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(u7, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       reset(h7),
		"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
	}
}

func unobservedClaudeAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive}
}

// allPairs makes consecutive picks walk every ordered pair of distinct
// candidates in turn, so shares over a multiple of n*(n-1) picks are exact.
func allPairs() func(int) int {
	pair, second := 0, -1
	return func(n int) int {
		if second >= 0 {
			value := second
			second = -1
			return value
		}
		p := pair % (n * (n - 1))
		pair++
		second = p % (n - 1)
		return p / (n - 1)
	}
}

func fixedHeadroomSelector(now time.Time) *HeadroomSelector {
	return &HeadroomSelector{nowFunc: func() time.Time { return now }, randFunc: allPairs()}
}

func headroomShares(t *testing.T, selector *HeadroomSelector, provider string, auths []*Auth, picks int) map[string]float64 {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < picks; i++ {
		got, err := selector.Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[got.ID]++
	}
	shares := make(map[string]float64, len(counts))
	for id, count := range counts {
		shares[id] = float64(count) / float64(picks)
	}
	return shares
}

func assertShare(t *testing.T, shares map[string]float64, id string, want float64) {
	t.Helper()
	if got := shares[id]; math.Abs(got-want) > 1e-9 {
		t.Fatalf("share[%s] = %.4f, want %.4f (all: %v)", id, got, want, shares)
	}
}

func TestReadClaudeWindow(t *testing.T) {
	t.Parallel()

	reset := time.Unix(1_800_000_000, 0)
	window := func(name, utilization, status string) map[string]string {
		prefix := "anthropic-ratelimit-unified-" + name + "-"
		return map[string]string{prefix + "utilization": utilization, prefix + "status": status, prefix + "reset": strconv.FormatInt(reset.Unix(), 10)}
	}
	tests := []struct {
		name     string
		signals  map[string]string
		window   string
		wantUsed float64
		wantOK   bool
	}{
		{"no data", map[string]string{}, "5h", 0, false},
		{"other window only", window("7d", "0.5", "allowed"), "5h", 0, false},
		{"half used", window("7d", "0.5", "allowed"), "7d", 0.5, true},
		{"over 100% clamps to used up", window("7d", "1.2", "allowed"), "7d", 1, true},
		{"rejected status is used up", window("5h", "0.4", "rejected"), "5h", 1, true},
		{"rejected without utilization is used up", window("5h", "", "rejected"), "5h", 1, true},
		{"unparsable utilization", window("5h", "NaN", "allowed"), "5h", 0, false},
		{"negative utilization", window("5h", "-0.1", "allowed"), "5h", 0, false},
		{"7d_oi namespace is not the 7d window", map[string]string{
			"anthropic-ratelimit-unified-7d_oi-utilization": "0.9",
			"anthropic-ratelimit-unified-7d_oi-reset":       strconv.FormatInt(reset.Unix(), 10),
		}, "7d", 0, false},
	}
	for _, tt := range tests {
		got, ok := readClaudeWindow(tt.signals, tt.window)
		if ok != tt.wantOK || math.Abs(got.used-tt.wantUsed) > 1e-9 || (ok && !got.reset.Equal(reset)) {
			t.Errorf("%s: got (%+v, %v), want used %.3f ok %v", tt.name, got, ok, tt.wantUsed, tt.wantOK)
		}
	}
}

func TestHeadroomUsageReadsBusierWindowAndNearLimits(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	lowerCase := &Auth{ID: "lower", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"anthropic-ratelimit-unified-7d-utilization": "0.25",
		"anthropic-ratelimit-unified-7d-reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}}}
	auths := []*Auth{
		headroomAuth("busier-5h", now, 0.7, 2, 0.1, 100),
		lowerCase,
		headroomAuth("5h-at-90", now, 0.9, 2, 0.1, 100),
		headroomAuth("7d-at-95", now, 0.1, 2, 0.95, 100),
		headroomAuth("7d-at-98", now, 0.1, 2, 0.98, 100),
		headroomAuth("5h-reset-passed", now, 0.95, -0.1, 0.2, 100),
		unobservedClaudeAuth("unobserved"),
	}
	used, open := (&HeadroomSelector{}).usage(auths, now)
	wantUsed := []float64{0.7, 0.25, 0.9, 0.95, 0.98, 0.2, 0}
	for i, want := range wantUsed {
		if math.Abs(used[i]-want) > 1e-9 {
			t.Errorf("%s used = %.3f, want %.3f", auths[i].ID, used[i], want)
		}
	}
	wantOpen := []int{0, 1, 3, 5, 6}
	if len(open) != len(wantOpen) {
		t.Fatalf("open = %v, want %v", open, wantOpen)
	}
	for i := range wantOpen {
		if open[i] != wantOpen[i] {
			t.Fatalf("open = %v, want %v", open, wantOpen)
		}
	}
}

// A response without a window, a reload of the credential file or a refresh can
// drop readings from Quota.Signals. The selector keeps each reading until its
// window resets, so the credential never looks unused by mistake.
func TestHeadroomUsageRemembersReadingsUntilReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auth := headroomAuth("a", now, 0.95, 2, 0.6, 50)
	selector := &HeadroomSelector{}
	usageAt := func(at time.Time) (float64, bool) {
		used, open := selector.usage([]*Auth{auth}, at)
		return used[0], len(open) == 0
	}
	if used, near := usageAt(now); used != 0.95 || !near {
		t.Fatalf("first reading = (%.2f, near %v), want (0.95, true)", used, near)
	}

	auth.Quota.Signals = map[string]string{"Retry-After": "30"}
	if used, near := usageAt(now); used != 0.95 || !near {
		t.Fatalf("after a snapshot without windows = (%.2f, near %v), want (0.95, true)", used, near)
	}
	auth.Quota.Signals = nil
	if used, near := usageAt(now.Add(time.Hour)); used != 0.95 || !near {
		t.Fatalf("after a reload = (%.2f, near %v), want (0.95, true)", used, near)
	}
	if used, near := usageAt(now.Add(3 * time.Hour)); used != 0.6 || near {
		t.Fatalf("after the 5h reset = (%.2f, near %v), want (0.60, false)", used, near)
	}

	// A new reading replaces the remembered one.
	auth.Quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.2",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(200*time.Hour).Unix(), 10),
	}
	if used, _ := usageAt(now.Add(4 * time.Hour)); used != 0.2 {
		t.Fatalf("after a new 7d reading = %.2f, want 0.20", used)
	}
	auth.Quota.Signals = nil
	if used, _ := usageAt(now.Add(201 * time.Hour)); used != 0 {
		t.Fatalf("after every window reset = %.2f, want 0", used)
	}
	if len(selector.seen) != 0 {
		t.Fatalf("selector still remembers %d credentials after their windows reset", len(selector.seen))
	}
}

func TestHeadroomSelectorPick_PrefersLessUsedWithoutPilingOn(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		headroomAuth("most", now, 0, 4, 0.40, 100),
		headroomAuth("least", now, 0, 4, 0.30, 100),
		headroomAuth("middle", now, 0, 4, 0.35, 100),
	}
	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", auths, 600)
	assertShare(t, shares, "least", 4.0/6)
	assertShare(t, shares, "middle", 2.0/6)
	assertShare(t, shares, "most", 0)
}

// Picks made before any response updates the headers all see the same snapshot.
// A fresh credential among busier ones takes only the pairs it is drawn in, not
// every new session. With two candidates the less used one takes every pick
// until its headers update (a documented limit).
func TestHeadroomSelectorPick_BurstOnOneSnapshotSpreads(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{unobservedClaudeAuth("fresh")}
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7"} {
		auths = append(auths, headroomAuth(id, now, 0.2, 4, 0.5, 100))
	}
	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", auths, 56)
	assertShare(t, shares, "fresh", 14.0/56)
	assertShare(t, shares, "b1", 6.0/56)

	shares = headroomShares(t, fixedHeadroomSelector(now), "claude", auths[:2], 10)
	assertShare(t, shares, "fresh", 1)
}

func TestHeadroomSelectorPick_SpreadsEquallyUsedCredentials(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{
		headroomAuth("a", now, 0.2, 4, 0.4, 100),
		headroomAuth("b", now, 0.1, 4, 0.4, 30),
		headroomAuth("c", now, 0.4, 1, 0.3, 60),
		headroomAuth("d", now, 0.0, 4, 0.4, 10),
	}
	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", auths, 120)
	for _, id := range []string{"a", "b", "c", "d"} {
		assertShare(t, shares, id, 0.25)
	}
}

func TestHeadroomSelectorPick_SkipsNearLimitUnlessEveryoneIs(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	burning := headroomAuth("burning", now, 0.92, 2, 0.3, 100)
	busy := headroomAuth("busy", now, 0.85, 4, 0.8, 100)
	if shares := headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{burning, busy}, 20); shares["burning"] != 0 {
		t.Fatalf("near-limit credential got %.2f of picks while another was below the limit", shares["burning"])
	}

	weekly := headroomAuth("weekly", now, 0.2, 4, 0.99, 30)
	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{burning, weekly}, 20)
	assertShare(t, shares, "burning", 1)
}

func TestHeadroomSelectorPick_FollowsLiveMembership(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	used := headroomAuth("used", now, 0.1, 4, 0.5, 100)
	disabled := unobservedClaudeAuth("disabled")
	disabled.Disabled = true
	cooled := unobservedClaudeAuth("cooled")
	cooled.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}
	added := unobservedClaudeAuth("added")

	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{used, disabled, cooled}, 30)
	assertShare(t, shares, "used", 1)

	// A credential added without any observation counts as unused.
	shares = headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{used, disabled, cooled, added}, 30)
	assertShare(t, shares, "added", 1)

	// Re-enabling and recovery need no configuration either.
	disabled.Disabled = false
	cooled.Quota = QuotaState{}
	shares = headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{used, disabled, cooled, added}, 120)
	assertShare(t, shares, "used", 0)
	for _, id := range []string{"disabled", "cooled", "added"} {
		assertShare(t, shares, id, 1.0/3)
	}
}

func TestHeadroomSelectorPick_AllCooledReturnsCooldownError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cooled := func(id string) *Auth {
		auth := headroomAuth(id, now, 0.5, 2, 0.5, 50)
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "credential_quota"
		auth.Quota.NextRecoverAt = now.Add(10 * time.Minute)
		return auth
	}
	selector := &HeadroomSelector{nowFunc: func() time.Time { return now }}
	_, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{cooled("a"), cooled("b")})
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("Pick() error = %v, want a cooldown error", err)
	}
}

func TestHeadroomSelectorPick_KeepsPriorityTiers(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	high := headroomAuth("high", now, 0.5, 2, 0.8, 50)
	high.Attributes = map[string]string{"priority": "5"}
	low := headroomAuth("low", now, 0, 4, 0, 100)
	if shares := headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{high, low}, 10); shares["high"] != 1 {
		t.Fatalf("higher priority tier share = %.2f, want 1", shares["high"])
	}
}

func TestHeadroomSelectorPick_NonClaudeCandidatesUseRoundRobin(t *testing.T) {
	t.Parallel()

	apiKey := map[string]string{AttributeAPIKey: "test-key"}
	for name, auths := range map[string][]*Auth{
		"codex": {{ID: "b", Provider: "codex"}, {ID: "a", Provider: "codex"}, {ID: "c", Provider: "codex"}},
		"mixed": {{ID: "b", Provider: "claude"}, {ID: "a", Provider: "codex"}, {ID: "c", Provider: "claude"}},
		// API keys report no subscription windows, so they would always look unused.
		"claude-api-key": {{ID: "b", Provider: "claude"}, {ID: "a", Provider: "claude", Attributes: apiKey}, {ID: "c", Provider: "claude"}},
	} {
		provider := name
		if name == "claude-api-key" {
			provider = "claude"
		}
		selector := &HeadroomSelector{}
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
// lists still pick by headroom.
func TestHeadroomSelectorPick_MixedPathWithClaudeCandidates(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{headroomAuth("busy", now, 0.5, 2, 0.5, 50), unobservedClaudeAuth("idle")}
	shares := headroomShares(t, fixedHeadroomSelector(now), "mixed", auths, 10)
	assertShare(t, shares, "idle", 1)
}

// PR #1 sent every new session to the most-used account when priorities were
// set by soonest reset, and let one hot account keep ~20% of picks among idle ones.
func TestHeadroomSelectorPick_QuotaAwareRegressions(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	soonResetMostUsed := headroomAuth("most", now, 0.2, 3, 0.9, 12)
	lateResetLeastUsed := headroomAuth("least", now, 0.2, 3, 0.1, 72)
	shares := headroomShares(t, fixedHeadroomSelector(now), "claude", []*Auth{soonResetMostUsed, lateResetLeastUsed}, 10)
	assertShare(t, shares, "most", 0)

	idle := []*Auth{headroomAuth("hot", now, 0.3, 3, 0.97, 20)}
	for _, id := range []string{"i1", "i2", "i3", "i4"} {
		idle = append(idle, unobservedClaudeAuth(id))
	}
	shares = headroomShares(t, fixedHeadroomSelector(now), "claude", idle, 100)
	assertShare(t, shares, "hot", 0)
}

// Through the manager's real pick path: new sessions follow the budget each
// credential reports in its response headers, and a bound session stays on its
// credential while another has more room.
func TestManagerHeadroomRoutingWithSessionAffinity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rng := mathrand.New(mathrand.NewPCG(3, 4))
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &HeadroomSelector{randFunc: rng.IntN},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	budget := map[string]float64{"big": 300, "small": 100}
	for id := range budget {
		if _, errRegister := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "test-" + id}}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	used := map[string]float64{}
	reset := strconv.FormatInt(time.Now().Add(100*time.Hour).Unix(), 10)
	pick := func(session string) string {
		t.Helper()
		headers := make(http.Header)
		headers.Set("X-Session-ID", session)
		auth, _, errPick := manager.pickNext(ctx, "claude", "", cliproxyexecutor.Options{Headers: headers}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(%s) error = %v", session, errPick)
		}
		used[auth.ID]++
		resultCtx := internallogging.WithResponseHeadersHolder(ctx)
		internallogging.SetResponseHeaders(resultCtx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{strconv.FormatFloat(used[auth.ID]/budget[auth.ID], 'f', 4, 64)},
			"Anthropic-Ratelimit-Unified-7d-Reset":       []string{reset},
		})
		manager.MarkResult(resultCtx, Result{AuthID: auth.ID, Provider: "claude", Success: true})
		return auth.ID
	}

	for i := 0; i < 200; i++ {
		pick("new-" + strconv.Itoa(i))
	}
	if spread := math.Abs(used["big"]/budget["big"] - used["small"]/budget["small"]); spread > 0.05 {
		t.Fatalf("utilization spread = %.3f after 200 new sessions (used %v), want <= 0.05", spread, used)
	}

	bound := pick("bound")
	for turn := 0; turn < 20; turn++ {
		if got := pick("bound"); got != bound {
			t.Fatalf("bound session moved from %s to %s on turn %d", bound, got, turn)
		}
	}
}

// Credentials of different, unknown sizes converge to a similar utilization, so
// each takes a share of traffic proportional to its size and none is emptied
// while the pool still has budget. A credential added midway catches up.
func TestHeadroomSelector_BalancesUtilizationWithoutKnownSizes(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	size := map[string]float64{"big": 9400, "mid": 4600, "small": 3300, "tiny": 2200}
	spent := map[string]float64{}
	auths := []*Auth{unobservedClaudeAuth("big"), unobservedClaudeAuth("mid"), unobservedClaudeAuth("small"), unobservedClaudeAuth("tiny")}
	rng := mathrand.New(mathrand.NewPCG(1, 2))
	selector := &HeadroomSelector{nowFunc: func() time.Time { return now }, randFunc: rng.IntN}
	pick := func() {
		got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		spent[got.ID]++
		// The response headers report the new utilization of the picked credential only.
		setHeadroomSignals(got, now, 0, 4, spent[got.ID]/size[got.ID], 100)
	}

	for i := 0; i < 8000; i++ {
		pick()
	}
	size["late"] = 4600
	auths = append(auths, unobservedClaudeAuth("late"))
	for i := 0; i < 9000; i++ {
		pick()
	}

	minUsed, maxUsed := 1.0, 0.0
	for id, capacity := range size {
		used := spent[id] / capacity
		minUsed, maxUsed = math.Min(minUsed, used), math.Max(maxUsed, used)
		if used >= claudeWindows[1].nearLimit {
			t.Fatalf("%s reached %.3f of its budget while the pool had budget left", id, used)
		}
	}
	if maxUsed-minUsed > 0.05 {
		t.Fatalf("utilization spread = %.3f (min %.3f, max %.3f), want <= 0.05; spent %v", maxUsed-minUsed, minUsed, maxUsed, spent)
	}
}

func TestHeadroomSelectorPick_ConcurrentPicks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{headroomAuth("a", now, 0.1, 4, 0.2, 100), headroomAuth("b", now, 0.3, 4, 0.1, 100), unobservedClaudeAuth("c")}
	selector := &HeadroomSelector{}
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

func TestHeadroomSelectorUnderSessionAffinityKeepsBoundSessions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	a := headroomAuth("a", now, 0.1, 4, 0.1, 100)
	b := headroomAuth("b", now, 0.1, 4, 0.1, 100)
	auths := []*Auth{a, b}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &HeadroomSelector{},
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

	// The bound credential is now much busier than the other one but still usable:
	// the session stays to keep its prompt cache.
	setHeadroomSignals(first, now, 0.85, 4, 0.9, 100)
	setHeadroomSignals(other, now, 0, 4, 0, 100)
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

func TestSelectorContextTreatsHeadroomCandidatesAsValidated(t *testing.T) {
	t.Parallel()

	ctx := selectorContextForAvailableAuths(context.Background(), &HeadroomSelector{}, "claude-sonnet-5-5")
	if validated, _ := ctx.Value(prevalidatedAuthCandidatesKey{}).(bool); !validated {
		t.Fatal("headroom selector context is not marked as carrying prevalidated candidates")
	}
}
