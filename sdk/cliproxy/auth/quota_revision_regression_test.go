package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaRevisionResets(auth *Auth, short, weekly time.Time) {
	switch auth.Provider {
	case "claude":
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(short.Unix(), 10)
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(weekly.Unix(), 10)
	case "codex":
		auth.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(short.Unix(), 10)
		auth.Quota.Signals["X-Codex-Secondary-Reset-At"] = strconv.FormatInt(weekly.Unix(), 10)
	}
}

func TestQuotaRevisionResetFirstPreservesHealthyBinding(t *testing.T) {
	// No expiry boundary relies on the real clock: the affinity cache only needs
	// these snapshots to be fresh during this synchronous sequence.
	now := time.Now().Truncate(time.Second)
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			a := quotaTestAuth("a", provider, now, quotaTestSignals(provider, now, 60, 60)) // 40% remaining.
			b := quotaTestAuth("b", provider, now, quotaTestSignals(provider, now, 20, 20)) // 80% remaining.
			quotaRevisionResets(a, now.Add(5*time.Minute), now.Add(4*time.Hour))
			quotaRevisionResets(b, now.Add(2*time.Hour), now.Add(3*time.Hour))
			aware := &QuotaAwareSelector{Fallback: &FillFirstSelector{}}
			sticky := NewSessionAffinitySelector(aware)
			t.Cleanup(sticky.Stop)
			options := func(id string) cliproxyexecutor.Options {
				return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{id}}}
			}
			if got, errPick := sticky.Pick(context.Background(), provider, "model", options("existing"), []*Auth{b}); errPick != nil || got == nil || got.ID != "b" {
				t.Fatalf("initial binding = %v, %v", got, errPick)
			}
			if got, errPick := sticky.Pick(context.Background(), provider, "model", options("new"), []*Auth{b, a}); errPick != nil || got == nil || got.ID != "a" {
				t.Errorf("new session = %v, %v; want earlier-reset account a despite less headroom", got, errPick)
			}
			if got, errPick := sticky.Pick(context.Background(), provider, "model", options("existing"), []*Auth{a, b}); errPick != nil || got == nil || got.ID != "b" {
				t.Errorf("healthy existing binding = %v, %v; want b", got, errPick)
			}
		})
	}
}

func TestQuotaRevisionCrossWindowResetOrder(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, provider := range []string{"claude", "codex"} {
		for _, test := range []struct {
			name                  string
			aShort, aWeekly       time.Duration
			bShort, bWeekly       time.Duration
			aUsedShort, aUsedWeek int
			bUsedShort, bUsedWeek int
			want                  string
			rejectedWeekly        bool
		}{
			{"short reset outranks headroom", 5 * time.Minute, 4 * time.Hour, 2 * time.Hour, 3 * time.Hour, 60, 60, 20, 20, "a", false},
			{"weekly reset outranks short reset", 3 * time.Hour, 5 * time.Minute, 2 * time.Hour, 4 * time.Hour, 60, 60, 20, 20, "a", false},
			{"equal earliest reset uses tightest headroom", 5 * time.Minute, 4 * time.Hour, 5 * time.Minute, 3 * time.Hour, 0, 90, 50, 50, "b", false},
			{"short reset cannot bypass rejected weekly", 5 * time.Minute, 4 * time.Hour, 2 * time.Hour, 3 * time.Hour, 60, 100, 20, 20, "b", true},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				a := quotaTestAuth("a", provider, now, quotaTestSignals(provider, now, test.aUsedShort, test.aUsedWeek))
				b := quotaTestAuth("b", provider, now, quotaTestSignals(provider, now, test.bUsedShort, test.bUsedWeek))
				quotaRevisionResets(a, now.Add(test.aShort), now.Add(test.aWeekly))
				quotaRevisionResets(b, now.Add(test.bShort), now.Add(test.bWeekly))
				if test.rejectedWeekly {
					switch provider {
					case "claude":
						a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
					case "codex":
						a.Quota.Signals["X-Codex-Limit-Reached"] = "true"
					}
				}
				s := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }}
				got, errPick := s.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, []*Auth{b, a})
				if errPick != nil || got == nil || got.ID != test.want {
					t.Fatalf("pick = %v, %v; want %s", got, errPick, test.want)
				}
			})
		}
	}
}

func TestQuotaRevisionManagerCombinesAllRecoveryBlocks(t *testing.T) {
	now := time.Unix(1800000000, 0)
	const routeModel = "recovery-route(high)"
	for _, provider := range []string{"codex", "mixed"} {
		for _, affinity := range []bool{false, true} {
			for _, test := range []struct {
				name                              string
				aOrdinary, bOrdinary, observedAge time.Duration
				aObserved, bObserved              bool
				want                              time.Duration
			}{
				{"ordinary earlier than another account observation", 20 * time.Second, 0, 0, false, true, 20 * time.Second},
				{"same account must clear both blocks", 20 * time.Second, 40 * time.Second, 0, true, false, 40 * time.Second},
				{"observation expiry cannot bypass longer ordinary block", 10 * time.Minute, 0, 0, true, true, 5 * time.Minute},
				{"all ordinary blocks still include observation", 20 * time.Second, 40 * time.Second, 0, true, true, 5 * time.Minute},
				{"single account takes max not min", 10 * time.Minute, -1, 0, true, false, 10 * time.Minute},
				{"snapshot expiry bounds only observation", 2 * time.Minute, -1, 4 * time.Minute, true, false, 2 * time.Minute},
				{"snapshot expiry may recover other account sooner", 2 * time.Minute, 0, 4 * time.Minute, true, true, time.Minute},
			} {
				t.Run(fmt.Sprintf("%s/affinity=%v/%s", provider, affinity, test.name), func(t *testing.T) {
					aware := &QuotaAwareSelector{Fallback: &FillFirstSelector{}}
					var selector Selector = aware
					if affinity {
						sticky := NewSessionAffinitySelector(aware)
						t.Cleanup(sticky.Stop)
						selector = sticky
					}
					manager := NewManager(nil, selector, nil)
					cause := &Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "synthetic model quota"}
					makeAuth := func(id string, ordinary time.Duration, observed bool) *Auth {
						auth := quotaTestAuth(id, "codex", now.Add(-test.observedAge), quotaTestSignals("codex", now, 10, 10))
						if observed {
							quotaTestExhaust(auth)
						}
						if ordinary > 0 {
							auth.ModelStates = map[string]*ModelState{"recovery-route": {
								Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(ordinary),
								Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(ordinary)}, LastError: cause, UpdatedAt: now,
							}}
						}
						return auth
					}
					auths := []*Auth{makeAuth("a", test.aOrdinary, test.aObserved)}
					if test.bOrdinary >= 0 {
						auths = append(auths, makeAuth("b", test.bOrdinary, test.bObserved))
					}
					// Exercise the manager's real route-aware filter with an explicit
					// timestamp, rather than sleeping through cooldown or expiry.
					_, _, errPick := manager.availableAuthsForSelector(selector, auths, provider, routeModel, now)
					var cooldown *modelCooldownError
					if !errors.As(errPick, &cooldown) || cooldown.resetIn != test.want {
						t.Fatalf("recovery = %v; want model cooldown wait %v", errPick, test.want)
					}
					wantProvider := provider
					if provider == "mixed" {
						wantProvider = ""
					}
					if cooldown.model != routeModel || cooldown.provider != wantProvider || !errors.Is(errPick, cause) {
						t.Fatalf("lost model/provider/cause contract: model=%q provider=%q cause=%v", cooldown.model, cooldown.provider, errors.Unwrap(errPick))
					}
					// At the exact reported recovery at least one candidate is usable.
					_, available, errRecovered := manager.availableAuthsForSelector(selector, auths, provider, routeModel, now.Add(test.want))
					if errRecovered != nil || len(available) == 0 {
						t.Fatalf("reported recovery is optimistic: available=%d error=%v", len(available), errRecovered)
					}
				})
			}
		}
	}
}

func TestQuotaRevisionSelectorAndRetryUseCombinedRecovery(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name     string
		ordinary time.Duration
		want     time.Duration
	}{
		{"ordinary earlier than passive expiry", 20 * time.Second, 5 * time.Minute},
		{"ordinary later than passive expiry", 10 * time.Minute, 10 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := quotaTestAuth("a", "codex", now, quotaTestSignals("codex", now, 100, 100))
			quotaTestExhaust(auth)
			auth.Unavailable, auth.Quota.Exceeded = true, true
			auth.NextRetryAfter, auth.Quota.NextRecoverAt = now.Add(test.ordinary), now.Add(test.ordinary)
			aware := &QuotaAwareSelector{Fallback: &FillFirstSelector{}, nowFunc: func() time.Time { return now }}
			_, errPick := aware.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{auth})
			var cooldown *modelCooldownError
			if !errors.As(errPick, &cooldown) || cooldown.resetIn != test.want {
				t.Fatalf("direct selector recovery = %v, want %v", errPick, test.want)
			}
			eligible, next := retryRoundAvailabilityForAuthWithQuota(auth, "", now, aware)
			if !eligible || !next.Equal(now.Add(test.want)) {
				t.Fatalf("retry recovery = %v, %v, want %v", eligible, next, now.Add(test.want))
			}
			auth.Disabled = true
			if eligible, _ := retryRoundAvailabilityForAuthWithQuota(auth, "", now, aware); eligible {
				t.Fatal("passive recovery made disabled auth retryable")
			}
		})
	}
}

func TestQuotaRevisionManagerExcludesNonPositiveWeightRecovery(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := quotaTestAuth("a", "codex", now, quotaTestSignals("codex", now, 10, 10))
	a.Attributes = map[string]string{AttributeWeight: "0"}
	a.Unavailable, a.Quota.Exceeded = true, true
	a.NextRetryAfter, a.Quota.NextRecoverAt = now.Add(20*time.Second), now.Add(20*time.Second)
	b := quotaTestAuth("b", "codex", now, quotaTestSignals("codex", now, 100, 100))
	quotaTestExhaust(b)
	aware := &QuotaAwareSelector{Fallback: &WeightedRoundRobinSelector{}}
	manager := NewManager(nil, aware, nil)
	_, _, errPick := manager.availableAuthsForSelector(aware, []*Auth{a, b}, "codex", "", now)
	var cooldown *modelCooldownError
	if !errors.As(errPick, &cooldown) || cooldown.resetIn != DefaultQuotaMaxAge {
		t.Fatalf("zero-weight account influenced recovery: %v", errPick)
	}
}

func TestQuotaRevisionRetryCannotBusyRetryExhaustedPool(t *testing.T) {
	now := time.Now()
	aware := &QuotaAwareSelector{Fallback: &FillFirstSelector{}}
	manager := NewManager(nil, aware, nil)
	manager.SetRetryConfig(2, 30*time.Second, 0)
	for _, candidate := range []*Auth{
		quotaTestAuth("a-retry", "codex", now, quotaTestSignals("codex", now, 10, 10)),
		quotaTestAuth("b-retry", "codex", now, quotaTestSignals("codex", now, 100, 100)),
	} {
		if candidate.ID == "a-retry" {
			candidate.Unavailable = true
			candidate.NextRetryAfter = now.Add(20 * time.Second)
			candidate.Quota.Exceeded = true
			candidate.Quota.NextRecoverAt = candidate.NextRetryAfter
		} else {
			quotaTestExhaust(candidate)
		}
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	errCooling := newModelCooldownError("", "codex", 20*time.Second)
	wait, retry := manager.shouldRetryAfterError(errCooling, 0, []string{"codex"}, "", 0)
	if retry || wait != 0 {
		t.Fatalf("no-wait retry = %v, %v; exhausted pool must not busy retry", wait, retry)
	}
	wait, retry = manager.shouldRetryAfterError(errCooling, 0, []string{"codex"}, "", 30*time.Second)
	if !retry || wait <= 0 || wait > 20*time.Second {
		t.Fatalf("bounded retry = %v, %v; want positive ordinary cooldown wait", wait, retry)
	}
}

func TestQuotaAwareAffinitySeparatesModels(t *testing.T) {
	now := time.Now()
	a := quotaTestAuth("model-a", "codex", now, quotaTestSignals("codex", now, 10, 10))
	b := quotaTestAuth("model-b", "codex", now, quotaTestSignals("codex", now, 10, 10))
	selector := NewSessionAffinitySelector(&QuotaAwareSelector{Fallback: &FillFirstSelector{}})
	t.Cleanup(selector.Stop)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"shared-session"}}}
	if got, errPick := selector.Pick(context.Background(), "codex", "model-one", opts, []*Auth{a, b}); errPick != nil || got == nil || got.ID != a.ID {
		t.Fatalf("model-one initial pick = %v, %v; want %s", got, errPick, a.ID)
	}
	if got, errPick := selector.Pick(context.Background(), "codex", "model-two", opts, []*Auth{b}); errPick != nil || got == nil || got.ID != b.ID {
		t.Fatalf("model-two isolated pick = %v, %v; want %s", got, errPick, b.ID)
	}
	if got, errPick := selector.Pick(context.Background(), "codex", "model-one", opts, []*Auth{a, b}); errPick != nil || got == nil || got.ID != a.ID {
		t.Fatalf("model-one binding changed = %v, %v; want %s", got, errPick, a.ID)
	}
	if got, errPick := selector.Pick(context.Background(), "codex", "model-two", opts, []*Auth{a, b}); errPick != nil || got == nil || got.ID != b.ID {
		t.Fatalf("model-two binding changed = %v, %v; want %s", got, errPick, b.ID)
	}
}
