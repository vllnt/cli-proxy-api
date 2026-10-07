package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerQuotaAwareSelectionPathsAndPriorityRecovery(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		for _, path := range []string{"select", "execute", "stream"} {
			t.Run(fmt.Sprintf("affinity=%v/%s", affinity, path), func(t *testing.T) {
				now := time.Now()
				ctx := context.Background()
				model := "quota-routing-" + t.Name()
				var selector Selector = &QuotaAwareSelector{Fallback: &FillFirstSelector{}}
				if affinity {
					sticky := NewSessionAffinitySelector(selector)
					t.Cleanup(sticky.Stop)
					selector = sticky
				}
				manager := NewManager(nil, selector, nil)
				manager.SetRetryConfig(0, 0, 0)
				a := quotaTestAuth("a-"+t.Name(), "codex", now, quotaTestSignals("codex", now, 100, 100))
				a.Attributes = map[string]string{"priority": "10"}
				quotaTestExhaust(a)
				b := quotaTestAuth("b-"+t.Name(), "codex", now, quotaTestSignals("codex", now, 10, 10))
				for _, candidate := range []*Auth{a, b} {
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
					if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				var attempts []string
				manager.RegisterExecutor(&customStreamMockExecutor{
					identifier: "codex",
					mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
						attempts = append(attempts, auth.ID)
						return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
					}},
					streamFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						attempts = append(attempts, auth.ID)
						chunks := make(chan cliproxyexecutor.StreamChunk, 1)
						chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
						close(chunks)
						return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
					},
				})
				run := func(session string) (string, error) {
					opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{session}}}
					switch path {
					case "select":
						got, errPick := manager.SelectAuth(ctx, "codex", model, opts)
						if errPick != nil {
							return "", errPick
						}
						return got.ID, nil
					case "execute":
						got, errExecute := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, opts)
						return string(got.Payload), errExecute
					default:
						got, errStream := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, opts)
						if errStream != nil {
							return "", errStream
						}
						var payload []byte
						for chunk := range got.Chunks {
							if chunk.Err != nil {
								return "", chunk.Err
							}
							payload = append(payload, chunk.Payload...)
						}
						return string(payload), nil
					}
				}
				assertPick := func(session, want string) {
					t.Helper()
					got, errPick := run(session)
					if errPick != nil || got != want {
						t.Fatalf("session %s = %q, %v, want %s; attempts=%v", session, got, errPick, want, attempts)
					}
				}
				assertPick("stable", b.ID) // Fresh exhaustion permits a lower priority tier.
				// Observation replacement is performed by the owning manager, not
				// by a side cache in the selector.
				headers := http.Header{}
				for key, value := range quotaTestSignals("codex", now, 0, 0) {
					headers.Set(key, value)
				}
				observationCtx := internallogging.WithResponseHeadersHolder(ctx)
				internallogging.SetResponseHeaders(observationCtx, headers)
				manager.MarkResult(observationCtx, Result{AuthID: a.ID, Provider: "codex", Model: model, Success: true})
				want := a.ID
				if affinity {
					want = b.ID
				}
				assertPick("stable", want)
				assertPick("new", a.ID)
				for _, candidate := range []*Auth{a, b} {
					headers = http.Header{}
					for key, value := range quotaTestSignals("codex", now, 100, 100) {
						headers.Set(key, value)
					}
					headers.Set("X-Codex-Limit-Reached", "true")
					internallogging.SetResponseHeaders(observationCtx, headers)
					manager.MarkResult(observationCtx, Result{AuthID: candidate.ID, Provider: "codex", Model: model, Success: true})
				}
				before := len(attempts)
				if _, errPick := run("stable"); statusCodeFromError(errPick) != http.StatusTooManyRequests {
					t.Fatalf("all exhausted = %v, want 429", errPick)
				}
				if len(attempts) != before {
					t.Fatal("attempted upstream while all snapshots explicitly reject")
				}
			})
		}
	}
}

func TestManagerQuotaAwareMixedProviderAndRetryExclusions(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	const model = "quota-mixed"
	s := NewSessionAffinitySelector(&QuotaAwareSelector{Fallback: &RoundRobinSelector{}})
	t.Cleanup(s.Stop)
	manager := NewManager(nil, s, nil)
	for _, candidate := range []*Auth{
		quotaTestAuth("quota-a", "claude", now, quotaTestSignals("claude", now, 90, 90)),
		quotaTestAuth("quota-b", "codex", now, quotaTestSignals("codex", now, 10, 10)),
	} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, candidate.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
		if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
		manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: candidate.Provider})
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"mixed-session"}}}
	got, _, provider, errPick := manager.pickNextMixed(ctx, []string{"claude", "codex"}, model, opts, nil)
	if errPick != nil || got == nil || got.ID != "quota-b" || provider != "codex" {
		t.Fatalf("mixed initial = %v, %s, %v", got, provider, errPick)
	}
	got, _, provider, errPick = manager.pickNextMixed(ctx, []string{"claude", "codex"}, model, opts, map[string]struct{}{"quota-b": {}})
	if errPick != nil || got == nil || got.ID != "quota-a" || provider != "claude" {
		t.Fatalf("retry exclusion = %v, %s, %v", got, provider, errPick)
	}
	bound, status := manager.LookupSessionAffinity("claude", model, "mixed-session")
	if status != "bound" || bound == nil || bound.ID != "quota-a" {
		t.Fatalf("rebound lookup = %v, %s", bound, status)
	}
	// A provider without trustworthy telemetry keeps fallback semantics even
	// when other mixed-pool candidates do have percentages.
	unknown := quotaTestAuth("quota-0", "gemini", now, nil)
	registry.GetGlobalRegistry().RegisterClient(unknown.ID, "gemini", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(unknown.ID) })
	if _, errRegister := manager.Register(ctx, unknown); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "gemini"})
	// New route state makes round-robin's first ID deterministic.
	manager.SetSelector(&QuotaAwareSelector{Fallback: &FillFirstSelector{}})
	got, _, provider, errPick = manager.pickNextMixed(ctx, []string{"claude", "codex", "gemini"}, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil || got == nil || got.ID != "quota-0" || provider != "gemini" {
		t.Fatalf("unknown mixed fallback = %v, %s, %v", got, provider, errPick)
	}
}

func TestQuotaAwareManagerWeightedStateUsesRouteNotAvailabilityAlias(t *testing.T) {
	s := &QuotaAwareSelector{Fallback: &WeightedRoundRobinSelector{}}
	ctx := selectorContextForAvailableAuths(context.Background(), s, "route(1000)")
	if got := weightedSelectorStateModel(ctx, "availability-target"); got != "route(1000)" {
		t.Fatalf("weighted state model = %q, want route", got)
	}
	if isBuiltInSelector(s) {
		t.Fatal("quota wrapper must not enter the scheduler fast path that bypasses passive observations")
	}
	if _, prevalidated := ctx.Value(prevalidatedAuthCandidatesKey{}).(bool); !prevalidated {
		t.Fatal("quota wrapper must retain manager alias prevalidation")
	}
	// Absolute reset beats a relative watermark that would move on every pick.
	now := time.Unix(1800000000, 0)
	auth := quotaTestAuth("a", "codex", now, map[string]string{"X-Codex-Primary-Used-Percent": "50", "X-Codex-Primary-Reset-At": strconv.FormatInt(now.Add(time.Minute).Unix(), 10), "X-Codex-Primary-Reset-After-Seconds": "3600"})
	if view := quotaViewForAuth(auth, now, DefaultQuotaMaxAge); !view.reset.Equal(now.Add(time.Minute)) {
		t.Fatalf("reset = %v, want absolute deadline", view.reset)
	}
}

func TestQuotaAwareSelectionLeavesHomeDispatchAuthoritative(t *testing.T) {
	fallback := &trackingSelector{}
	manager := NewManager(nil, &QuotaAwareSelector{Fallback: fallback}, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, Routing: internalconfig.RoutingConfig{QuotaAware: true}})
	_, _, errPick := manager.pickNext(context.Background(), "codex", "model", cliproxyexecutor.Options{}, nil)
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr.Code != "home_unavailable" {
		t.Fatalf("Home pick error = %v, want home_unavailable", errPick)
	}
	if fallback.calls != 0 {
		t.Fatalf("quota-aware local selector was called in Home mode: %d", fallback.calls)
	}
}
