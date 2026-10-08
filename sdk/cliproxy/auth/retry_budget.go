package auth

import (
	"context"
	"time"
)

// retryBudgetNow is the clock for max-retry-duration; tests replace it.
var retryBudgetNow = time.Now

type retryBudgetDeadlineKey struct{}

// withRetryBudget records when max-retry-duration runs out for this request. It is a plain
// timestamp rather than a context deadline: the retry loops stop starting new credential
// attempts after it, but an attempt already in flight or a stream already delivering is never
// cut short. Home owns retries when enabled, so no budget applies there.
func (m *Manager) withRetryBudget(ctx context.Context) context.Context {
	cfg := m.runtimeConfigSnapshot()
	if ctx == nil || cfg == nil || cfg.MaxRetryDuration <= 0 || m.HomeEnabled() {
		return ctx
	}
	deadline := retryBudgetNow().Add(time.Duration(cfg.MaxRetryDuration) * time.Second)
	return context.WithValue(ctx, retryBudgetDeadlineKey{}, deadline)
}

// retryBudgetExhausted reports whether an attempt that would start after waiting wait begins
// past the request's max-retry-duration.
func retryBudgetExhausted(ctx context.Context, wait time.Duration) bool {
	if ctx == nil {
		return false
	}
	deadline, ok := ctx.Value(retryBudgetDeadlineKey{}).(time.Time)
	return ok && !retryBudgetNow().Add(wait).Before(deadline)
}
