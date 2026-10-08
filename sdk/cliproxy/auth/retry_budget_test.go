package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// fakeRetryClock lets a test decide how long each upstream attempt takes.
type fakeRetryClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeRetryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeRetryClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func withFakeRetryClock(t *testing.T) *fakeRetryClock {
	t.Helper()
	clock := &fakeRetryClock{now: time.Unix(1_790_000_000, 0)}
	previous := retryBudgetNow
	retryBudgetNow = clock.Now
	t.Cleanup(func() { retryBudgetNow = previous })
	return clock
}

// Every credential is overloaded and each attempt takes 6s. A 10s budget admits the attempt that
// starts at 6s but none after 12s, whether the next attempt would come from the same retry round
// (all credentials per round, long cooldowns) or from a new round (one credential per round, no
// cooldown, so rounds start without waiting). Without a budget all four credentials are tried.
func TestRetryBudgetStopsStartingNewAttempts(t *testing.T) {
	const attemptDuration = 6 * time.Second
	for _, test := range []struct {
		name                string
		budget              int
		maxRetryCredentials int
		transientCooldown   int
		want                int
	}{
		{name: "within a round", budget: 10, want: 2},
		{name: "across rounds", budget: 10, maxRetryCredentials: 1, transientCooldown: -1, want: 2},
		{name: "unlimited within a round", want: 4},
		{name: "unlimited across rounds", maxRetryCredentials: 1, transientCooldown: -1, want: 4},
	} {
		for _, mode := range []string{"execute", "count", "stream"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				withQuotaCooldownEnabled(t)
				previousTransient := transientErrorCooldownSeconds.Load()
				SetTransientErrorCooldownSeconds(test.transientCooldown)
				t.Cleanup(func() { transientErrorCooldownSeconds.Store(previousTransient) })
				clock := withFakeRetryClock(t)
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{MaxRetryDuration: test.budget, OverloadCooldownSeconds: 3600})
				manager.SetRetryConfig(3, 0, test.maxRetryCredentials)
				registerOverloadAuths(t, manager, 4)

				var mu sync.Mutex
				attempts := 0
				overloaded := func(ctx context.Context) error {
					// The budget only gates new attempts; it must never put a deadline on one.
					if _, hasDeadline := ctx.Deadline(); hasDeadline {
						t.Error("an attempt must not inherit a deadline from max-retry-duration")
					}
					mu.Lock()
					attempts++
					mu.Unlock()
					clock.Advance(attemptDuration)
					return newOverloadRejection()
				}
				manager.RegisterExecutor(&customStreamMockExecutor{
					mockCustomErrorExecutor: mockCustomErrorExecutor{
						executeFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
							return cliproxyexecutor.Response{}, overloaded(ctx)
						},
						countFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
							return cliproxyexecutor.Response{}, overloaded(ctx)
						},
					},
					identifier: "codex",
					streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						return nil, overloaded(ctx)
					},
				})

				req := cliproxyexecutor.Request{Model: "gpt-5.6-terra"}
				var err error
				switch mode {
				case "execute":
					_, err = manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				case "count":
					_, err = manager.ExecuteCount(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				default:
					_, err = manager.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				}
				if statusCodeFromError(err) != http.StatusServiceUnavailable {
					t.Fatalf("expected the last upstream overload to surface, got %T %v", err, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if attempts != test.want {
					t.Fatalf("attempts = %d, want %d", attempts, test.want)
				}
			})
		}
	}
}
