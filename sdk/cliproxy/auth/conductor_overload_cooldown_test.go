package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// overloadRejection is what executors return when the upstream sheds load.
type overloadRejection struct{ customStatusError }

func (overloadRejection) IsOverload() bool { return true }

func newOverloadRejection() overloadRejection {
	return overloadRejection{overloadStatusError()}
}

func TestMarkResult_OverloadCooldownOnlyShortensOverloadRejections(t *testing.T) {
	withQuotaCooldownEnabled(t)
	previousTransient := transientErrorCooldownSeconds.Load()
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previousTransient) })
	explicitRetry := 30 * time.Second
	for _, test := range []struct {
		name             string
		transient        int
		overloadCooldown int
		err              error
		retryAfter       *time.Duration
		model            string
		want             time.Duration // 0 means no cooldown
	}{
		{name: "overload uses the overload cooldown", overloadCooldown: 5, err: newOverloadRejection(), model: "gpt-6-astra", want: 5 * time.Second},
		{name: "overload without a model uses the overload cooldown", overloadCooldown: 5, err: newOverloadRejection(), want: 5 * time.Second},
		{name: "overload keeps the transient cooldown when unset", err: newOverloadRejection(), model: "gpt-6-astra", want: transientErrorCooldown},
		{name: "other 503s keep the transient cooldown", overloadCooldown: 5, err: customStatusError{code: http.StatusServiceUnavailable, msg: "unavailable"}, model: "gpt-6-astra", want: transientErrorCooldown},
		{name: "upstream retry advice wins", overloadCooldown: 5, err: newOverloadRejection(), retryAfter: &explicitRetry, model: "gpt-6-astra", want: explicitRetry},
		{name: "disabled transient cooldowns stay disabled", transient: -1, overloadCooldown: 5, err: newOverloadRejection(), model: "gpt-6-astra"},
		{name: "a non-5xx overload keeps the quota backoff", overloadCooldown: 5, err: overloadRejection{customStatusError{code: http.StatusTooManyRequests, msg: "slow down"}}, want: quotaBackoffBase},
	} {
		t.Run(test.name, func(t *testing.T) {
			SetTransientErrorCooldownSeconds(test.transient)
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{OverloadCooldownSeconds: test.overloadCooldown})
			auth := &Auth{ID: "overload-cooldown", Provider: "codex"}
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			before := time.Now()
			manager.MarkResult(context.Background(), Result{
				AuthID:     auth.ID,
				Provider:   auth.Provider,
				Model:      test.model,
				RetryAfter: test.retryAfter,
				Error:      resultErrorFromError(test.err),
			})

			updated, _ := manager.GetByID(auth.ID)
			next := updated.NextRetryAfter
			if test.model != "" {
				state := updated.ModelStates[test.model]
				if state == nil {
					t.Fatal("missing model state")
				}
				next = state.NextRetryAfter
				if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-6.1-sol", time.Now()); blocked {
					t.Fatal("a failure on one model must not block the credential for other models")
				}
			}
			if test.want == 0 {
				if !next.IsZero() {
					t.Fatalf("expected no cooldown, got %v", time.Until(next))
				}
				return
			}
			if got := next.Sub(before); got < test.want || got > test.want+time.Second {
				t.Fatalf("cooldown = %v, want %v", got, test.want)
			}
		})
	}
}
