package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func quotaTestAuth(id, provider string, observedAt time.Time, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: provider, Status: StatusActive, Quota: QuotaState{ObservedAt: observedAt, Signals: signals}}
}

func quotaTestSignals(provider string, now time.Time, short, weekly int) map[string]string {
	switch provider {
	case "claude":
		return map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": fmt.Sprintf("%.2f", float64(short)/100),
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
			"Anthropic-Ratelimit-Unified-7d-Utilization": fmt.Sprintf("%.2f", float64(weekly)/100),
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
		}
	case "codex":
		return map[string]string{
			"X-Codex-Primary-Used-Percent":   strconv.Itoa(short),
			"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
			"X-Codex-Secondary-Used-Percent": strconv.Itoa(weekly),
			"X-Codex-Secondary-Reset-At":     strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
		}
	default:
		return nil
	}
}

func quotaTestExhaust(auth *Auth) {
	switch auth.Provider {
	case "claude":
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	case "codex":
		auth.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
		auth.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "100"
		auth.Quota.Signals["X-Codex-Limit-Reached"] = "true"
	}
}

func TestQuotaAwareSelectorWindowRankingAndUnknownFallback(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			a := quotaTestAuth("a", provider, now, quotaTestSignals(provider, now, 0, 95))
			b := quotaTestAuth("b", provider, now, quotaTestSignals(provider, now, 40, 40))
			s := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0.999 }}
			pick := func(want string) {
				t.Helper()
				got, errPick := s.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, []*Auth{b, a})
				if errPick != nil || got == nil || got.ID != want {
					t.Fatalf("pick = %v, %v; want %s", got, errPick, want)
				}
			}
			pick("b") // The weekly bottleneck outweighs a full short window.
			a.Quota.Signals = quotaTestSignals(provider, now, 95, 0)
			pick("b") // A short-window bottleneck is equally important.
			a.Quota.ObservedAt = now.Add(-DefaultQuotaMaxAge)
			pick("b") // Stale means neutral; known headroom remains in the weighted pool.
			a.Quota.ObservedAt = now.Add(time.Second)
			pick("b") // Future observations are untrustworthy and therefore neutral.
			a.Quota.ObservedAt = time.Time{}
			pick("b")
			a.Quota.ObservedAt = now
			a.Quota.Signals = nil
			pick("b")
			// Unsupported providers never acquire a synthetic quota from headers.
			a.Provider, b.Provider = "gemini", "gemini"
			pick("a")
		})
	}
}

func TestQuotaAwareSelectorDoesNotPreferMostUsedAccountForSoonerReset(t *testing.T) {
	now := time.Unix(1800000000, 0)
	hot := quotaTestAuth("hot", "claude", now, quotaTestSignals("claude", now, 80, 20))
	cool := quotaTestAuth("cool", "claude", now, quotaTestSignals("claude", now, 20, 20))
	hot.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	cool.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10)
	selector := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0.5 }}
	got, errPick := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{hot, cool})
	if errPick != nil || got == nil || got.ID != "cool" {
		t.Fatalf("pick = %v, %v; want cool account despite later reset", got, errPick)
	}
}

func TestQuotaViewValidationAndResetHandling(t *testing.T) {
	now := time.Unix(1800000000, 0)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	for _, test := range []struct {
		name, provider string
		signals        map[string]string
		known          bool
		remaining      float64
		blocked        time.Duration
	}{
		{"codex relative reset anchored to observation", "codex", map[string]string{"X-Codex-Primary-Used-Percent": "25", "X-Codex-Primary-Reset-After-Seconds": "120"}, true, .75, 0},
		{"codex allowed at full usage", "codex", map[string]string{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Primary-Reset-At": future, "X-Codex-Allowed": "true", "X-Codex-Limit-Reached": "true"}, true, 0, 0},
		{"codex percentage alone is not rejection", "codex", map[string]string{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Primary-Reset-At": future}, true, 0, 0},
		{"codex explicit exhaustion", "codex", map[string]string{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Primary-Reset-At": future, "X-Codex-Allowed": "false"}, true, 0, time.Hour},
		{"codex limit reached without usage", "codex", map[string]string{"X-Codex-Primary-Reset-At": future, "X-Codex-Limit-Reached": "true"}, false, 0, time.Hour},
		{"codex denied without usage", "codex", map[string]string{"X-Codex-Primary-Reset-At": future, "X-Codex-Allowed": "false"}, false, 0, time.Hour},
		{"claude shared rejection without utilization", "claude", map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": future}, false, 0, time.Hour},
		{"claude aggregate rejection fallback", "claude", map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected", "Anthropic-Ratelimit-Unified-Reset": future}, false, 0, time.Hour},
		{"claude model-specific overage ignored", "claude", map[string]string{"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected", "Anthropic-Ratelimit-Unified-7d_oi-Utilization": "1", "Anthropic-Ratelimit-Unified-7d_oi-Reset": future, "Anthropic-Ratelimit-Unified-Status": "rejected"}, false, 0, 0},
		{"codex other model and code review ignored", "codex", map[string]string{"X-Codex-Additional-Spark-Primary-Used-Percent": "100", "X-Codex-Additional-Spark-Primary-Reset-At": future, "X-Codex-Code-Review-Primary-Used-Percent": "100", "X-Codex-Code-Review-Primary-Reset-At": future}, false, 0, 0},
		{"expired windows are unknown", "claude", map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Utilization": "1", "Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(now.Unix(), 10)}, false, 0, 0},
		{"missing reset does not block", "claude", map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Utilization": "1"}, false, 0, 0},
		{"retry-after alone does not block", "codex", map[string]string{"Retry-After": "60"}, false, 0, 0},
		{"wrong provider does not use claude signals", "gemini", map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": future}, false, 0, 0},
		{"conflicting case variants are unknown", "codex", map[string]string{"X-Codex-Primary-Used-Percent": "100", "x-codex-primary-used-percent": "0", "X-Codex-Primary-Reset-At": future, "X-Codex-Limit-Reached": "true"}, false, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := quotaTestAuth("a", test.provider, now.Add(-time.Minute), test.signals)
			view := quotaViewForAuth(auth, now, DefaultQuotaMaxAge)
			if view.known != test.known || view.remaining != test.remaining {
				t.Fatalf("view = %+v, want known=%v remaining=%v", view, test.known, test.remaining)
			}
			if test.blocked == 0 && !view.blockedUntil.IsZero() || test.blocked > 0 && !view.blockedUntil.Equal(now.Add(test.blocked)) {
				t.Fatalf("blockedUntil = %v, want wait %v", view.blockedUntil, test.blocked)
			}
			if test.name == "codex relative reset anchored to observation" && !view.reset.Equal(now.Add(time.Minute)) {
				t.Fatalf("relative reset = %v, want observedAt + 120 seconds", view.reset)
			}
		})
	}
	for _, raw := range []string{"NaN", "+Inf", "-1", "101", "bad", ""} {
		auth := quotaTestAuth("a", "codex", now, map[string]string{"X-Codex-Primary-Used-Percent": raw, "X-Codex-Primary-Reset-At": future, "X-Codex-Limit-Reached": "true"})
		if view := quotaViewForAuth(auth, now, DefaultQuotaMaxAge); view.known || !view.blockedUntil.Equal(now.Add(time.Hour)) {
			t.Errorf("invalid usage %q did not preserve explicit rejection: %+v", raw, view)
		}
	}
	staleAbsolute := quotaTestAuth("stale-absolute", "codex", now.Add(-time.Minute), map[string]string{
		"X-Codex-Primary-Used-Percent":        "99",
		"X-Codex-Primary-Reset-At":            strconv.FormatInt(now.Add(-time.Second).Unix(), 10),
		"X-Codex-Primary-Reset-After-Seconds": "120",
		"X-Codex-Limit-Reached":               "true",
	})
	if view := quotaViewForAuth(staleAbsolute, now, DefaultQuotaMaxAge); !view.known || !view.reset.Equal(now.Add(time.Minute)) || !view.blockedUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("stale absolute reset did not fall back to relative reset: %+v", view)
	}

	for _, raw := range []string{"-1", "bad", "1800000000000", "9223372036854775807", ""} {
		if reset := quotaReset(raw); !reset.IsZero() {
			t.Errorf("invalid reset %q = %v", raw, reset)
		}
	}
}

func TestClaudeAggregateRejectionWithOverageMarkerNeedsHealthySharedWindow(t *testing.T) {
	now := time.Unix(1800000000, 0)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	unsafe := quotaTestAuth("unsafe", "claude", now, map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "rejected",
		"Anthropic-Ratelimit-Unified-Reset":                future,
		"Anthropic-Ratelimit-Unified-7d_oi-Status":         "rejected",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day_overage_included",
	})
	if view := quotaViewForAuth(unsafe, now, DefaultQuotaMaxAge); view.known || !view.blockedUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("overage marker without healthy shared window bypassed rejection: %+v", view)
	}
}

func TestCodexQuotaViewRequiresCredentialWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	namespacedOnly := quotaTestAuth("namespaced", "codex", now, map[string]string{
		"X-Codex-Bengalfox-Secondary-Used-Percent": "100",
		"X-Codex-Bengalfox-Secondary-Reset-At":     future,
		"X-Codex-Limit-Reached":                    "true",
	})
	if view := quotaViewForAuth(namespacedOnly, now, DefaultQuotaMaxAge); view.known || !view.blockedUntil.IsZero() {
		t.Fatalf("namespaced model window was treated as credential quota: %+v", view)
	}
	primaryOnly := quotaTestAuth("primary-only", "codex", now, map[string]string{
		"X-Codex-Primary-Used-Percent": "25",
		"X-Codex-Primary-Reset-At":     future,
	})
	if view := quotaViewForAuth(primaryOnly, now, DefaultQuotaMaxAge); !view.known || view.remaining != .75 {
		t.Fatalf("credential primary window was not accepted: %+v", view)
	}
}

func TestQuotaAwareSelectorResetTiePriorityWeightsAndTransport(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := quotaTestAuth("a", "codex", now, quotaTestSignals("codex", now, 50, 10))
	b := quotaTestAuth("b", "codex", now, quotaTestSignals("codex", now, 50, 10))
	b.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
	s := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0.999 }}
	pick := func(ctx context.Context, want string) {
		t.Helper()
		got, errPick := s.Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{b, a})
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("pick = %v, %v; want %s", got, errPick, want)
		}
	}
	pick(context.Background(), "b")
	a.Attributes = map[string]string{"priority": "1"}
	pick(context.Background(), "a") // Priority is a constraint, not a quota score.
	a.Attributes = map[string]string{"websockets": "true"}
	pick(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "a")
	a.Attributes = map[string]string{AttributeWeight: "0"}
	s.Fallback = &WeightedRoundRobinSelector{}
	pick(context.Background(), "b")

	a.Attributes = map[string]string{AttributeWeight: "1"}
	b.Attributes = map[string]string{AttributeWeight: "3"}
	b.Quota.Signals = a.Quota.Clone().Signals
	counts := map[string]int{}
	randomIndex := 0
	s.randFunc = func() float64 {
		value := (float64(randomIndex%4) + 0.5) / 4
		randomIndex++
		return value
	}
	for i := 0; i < 40; i++ {
		got, errPick := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, []*Auth{b, a})
		if errPick != nil {
			t.Fatal(errPick)
		}
		counts[got.ID]++
	}
	if counts["a"] < 8 || counts["a"] > 15 || counts["b"] < 25 || counts["b"] > 32 {
		t.Fatalf("weighted ties = %v, want approximately 1:3", counts)
	}
}

func TestQuotaAwareSelectorEarliestAccountLatestWindowRecovery(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := quotaTestAuth("a", "claude", now, quotaTestSignals("claude", now, 100, 100))
	b := quotaTestAuth("b", "claude", now, quotaTestSignals("claude", now, 100, 100))
	quotaTestExhaust(a)
	quotaTestExhaust(b)
	b.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)
	s := &QuotaAwareSelector{MaxAge: 48 * time.Hour, nowFunc: func() time.Time { return now }}
	_, errPick := s.Pick(context.Background(), "mixed", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	cooldown, ok := errPick.(*modelCooldownError)
	if !ok || cooldown.resetIn != 2*time.Hour || cooldown.provider != "" || cooldown.model != "model" {
		t.Fatalf("all exhausted error = %#v, want model cooldown with 2h wait", errPick)
	}
	// The advertised recovery is never later than snapshot expiry.
	s.MaxAge = DefaultQuotaMaxAge
	_, errPick = s.Pick(context.Background(), "mixed", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if cooldown, ok = errPick.(*modelCooldownError); !ok || cooldown.resetIn != DefaultQuotaMaxAge {
		t.Fatalf("snapshot expiry error = %#v, want 5m wait", errPick)
	}
	// Once the observation ages out, stale exhaustion must not create a lockout.
	now = now.Add(DefaultQuotaMaxAge)
	got, errPick := s.Pick(context.Background(), "mixed", "model", cliproxyexecutor.Options{}, []*Auth{b, a})
	if errPick != nil || got == nil || got.ID != "a" {
		t.Fatalf("stale recovery pick = %v, %v", got, errPick)
	}
}

func TestQuotaAwareAffinityPreservesSessionsAndFailsOver(t *testing.T) {
	// Wide timestamp margins keep affinity's real clock independent of platform
	// timer granularity; expiry boundaries themselves are tested with a fake clock.
	now := time.Now()
	for _, provider := range []string{"claude", "codex", "gemini", "antigravity", "vertex", "aistudio", "openai", "openai-compatibility", "kimi", "kimi-ai", "xai", "grok", "meta"} {
		for _, mode := range []string{"explicit", "lcp"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				a := quotaTestAuth("a", provider, now, quotaTestSignals(provider, now, 10, 10))
				b := quotaTestAuth("b", provider, now, quotaTestSignals(provider, now, 60, 60))
				randomIndex := 0
				s := NewSessionAffinitySelector(&QuotaAwareSelector{Fallback: &FillFirstSelector{}, randFunc: func() float64 {
					value := 0.0
					if randomIndex > 0 {
						value = 0.999
					}
					randomIndex++
					return value
				}})
				t.Cleanup(s.Stop)
				opts := func(session string) cliproxyexecutor.Options {
					options := cliproxyexecutor.Options{Metadata: map[string]any{}}
					if mode == "explicit" {
						options.Headers = http.Header{"X-Session-Id": []string{session}}
					} else {
						options.SourceFormat = sdktranslator.FormatOpenAI
						options.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "quota-test"
						options.OriginalRequest = []byte(fmt.Sprintf(`{"messages":[{"role":"system","content":%q},{"role":"user","content":"hello"}]}`, session))
					}
					return options
				}
				pick := func(session, want string) {
					t.Helper()
					got, errPick := s.Pick(context.Background(), provider, "model", opts(session), []*Auth{b, a})
					if errPick != nil || got == nil || got.ID != want {
						t.Fatalf("%s pick = %v, %v; want %s", session, got, errPick, want)
					}
				}
				pick("stable", "a")
				b.Quota.Signals = quotaTestSignals(provider, now, 0, 0)
				pick("stable", "a") // Headroom alone must never migrate a binding.
				if ProviderSupportsQuotaObservation(provider) {
					pick("new", "b")
					quotaTestExhaust(a)
				} else {
					a.Unavailable = true
				}
				pick("stable", "b")
				a.Unavailable = false
				a.Quota.Signals = quotaTestSignals(provider, now, 0, 0)
				pick("stable", "b") // Recovery doesn't steal the rebound session.
				// Existing cooldown ownership still wins over passive quota ranking.
				b.Quota.Exceeded, b.Quota.Reason, b.Quota.NextRecoverAt = true, "credential_quota", now.Add(time.Hour)
				pick("stable", "a")
			})
		}
	}
}

func TestQuotaAwareAffinityWeightedZeroRebindAndConcurrentPicks(t *testing.T) {
	now := time.Now()
	a := quotaTestAuth("a", "codex", now, quotaTestSignals("codex", now, 0, 0))
	b := quotaTestAuth("b", "codex", now, quotaTestSignals("codex", now, 10, 10))
	s := NewSessionAffinitySelector(&QuotaAwareSelector{Fallback: &WeightedRoundRobinSelector{}, randFunc: func() float64 { return 0 }})
	t.Cleanup(s.Stop)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"stable"}}}
	got, errPick := s.Pick(context.Background(), "codex", "model", opts, []*Auth{a, b})
	if errPick != nil || got.ID != "a" {
		t.Fatalf("initial pick = %v, %v", got, errPick)
	}
	a.Attributes = map[string]string{AttributeWeight: "0"}
	got, errPick = s.Pick(context.Background(), "codex", "model", opts, []*Auth{a, b})
	if errPick != nil || got.ID != "b" {
		t.Fatalf("zero-weight rebind = %v, %v", got, errPick)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			for n := 0; n < 10; n++ {
				got, errPick := s.Pick(context.Background(), "codex", "model", opts, []*Auth{a, b})
				if errPick != nil || got == nil || got.ID != "b" {
					t.Errorf("concurrent pick = %v, %v", got, errPick)
				}
			}
		})
	}
	wg.Wait()
}

func TestQuotaAwarePriorityUsesLowerTierOnlyWhenHigherTierIsHot(t *testing.T) {
	now := time.Unix(1800000000, 0)
	highHot := quotaTestAuth("high-hot", "claude", now, quotaTestSignals("claude", now, 95, 20))
	highHot.Attributes = map[string]string{"priority": "2"}
	lowCool := quotaTestAuth("low-cool", "claude", now, quotaTestSignals("claude", now, 20, 20))
	lowCool.Attributes = map[string]string{"priority": "1"}
	selector := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0 }}
	got, errPick := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{highHot, lowCool})
	if errPick != nil || got == nil || got.ID != lowCool.ID {
		t.Fatalf("hot high-priority pick = %v, %v; want lower cool tier", got, errPick)
	}

	highCool := quotaTestAuth("high-cool", "claude", now, quotaTestSignals("claude", now, 20, 20))
	highCool.Attributes = map[string]string{"priority": "2"}
	selected, selectedViews := pacedPriorityTier([]*Auth{highHot, highCool, lowCool}, []quotaRoutingView{
		quotaViewForAuth(highHot, now, DefaultQuotaMaxAge),
		quotaViewForAuth(highCool, now, DefaultQuotaMaxAge),
		quotaViewForAuth(lowCool, now, DefaultQuotaMaxAge),
	})
	if len(selected) != 2 || selected[0].ID != highHot.ID || selected[1].ID != highCool.ID || len(selectedViews) != 2 {
		t.Fatalf("higher tier with paced headroom was not kept intact: auths=%v views=%v", selected, selectedViews)
	}
}

func TestQuotaAwareCodexCreditsPlanAndResetMetadata(t *testing.T) {
	now := time.Unix(1800000000, 0)
	future := strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)
	credits := quotaTestAuth("credits", "codex", now, map[string]string{
		"X-Codex-Primary-Used-Percent":   "100",
		"X-Codex-Primary-Reset-At":       future,
		"X-Codex-Secondary-Used-Percent": "100",
		"X-Codex-Secondary-Reset-At":     future,
		"X-Codex-Limit-Reached":          "true",
		"X-Codex-Credits-Has-Credits":    "true",
		"X-Codex-Plan-Type":              "enterprise",
		"X-Codex-Primary-Resets-Left":    "3",
		"X-Codex-Primary-Reset-Expiry":   strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	})
	view := quotaViewForAuth(credits, now, DefaultQuotaMaxAge)
	if !view.known || !view.credits || view.planTypeRank != 4 || !view.blockedUntil.IsZero() || view.resetsLeft != 3 || !view.resetExpiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("credit overflow/reset metadata view = %+v", view)
	}
	quota := quotaTestAuth("quota", "codex", now, map[string]string{
		"X-Codex-Primary-Used-Percent":   "50",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10),
		"X-Codex-Secondary-Used-Percent": "50",
		"X-Codex-Secondary-Reset-At":     strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10),
		"X-Codex-Plan-Type":              "plus",
	})
	selector := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0.5 }}
	got, errPick := selector.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, []*Auth{credits, quota})
	if errPick != nil || got == nil || got.ID != credits.ID {
		t.Fatalf("credit overflow pick = %v, %v; want credits account", got, errPick)
	}
}

func TestQuotaAwareStaleSnapshotIsNeutral(t *testing.T) {
	now := time.Unix(1800000000, 0)
	stale := quotaTestAuth("stale", "claude", now.Add(-DefaultQuotaMaxAge-time.Second), quotaTestSignals("claude", now, 0, 0))
	fresh := quotaTestAuth("fresh", "claude", now, quotaTestSignals("claude", now, 80, 80))
	view := quotaViewForAuth(stale, now, DefaultQuotaMaxAge)
	if view.known || !view.blockedUntil.IsZero() {
		t.Fatalf("stale snapshot was treated as a routing advantage: %+v", view)
	}
	fallback := &FillFirstSelector{}
	selector := &QuotaAwareSelector{Fallback: fallback, nowFunc: func() time.Time { return now }, randFunc: func() float64 { return 0 }}
	got, errPick := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{stale, fresh})
	if errPick != nil || got == nil || got.ID != "fresh" {
		t.Fatalf("mixed stale/fresh selection = %v, %v; want known headroom account", got, errPick)
	}
	// A cool known account must beat an unknown one; stale data is not a
	// synthetic best score.
	cool := quotaTestAuth("cool", "claude", now, quotaTestSignals("claude", now, 0, 0))
	selector.randFunc = func() float64 { return 0 }
	got, errPick = selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{stale, cool})
	if errPick != nil || got == nil || got.ID != "cool" {
		t.Fatalf("unknown account outranked cool known account = %v, %v", got, errPick)
	}
}
