package auth

import (
	"context"
	"time"
)

// retryBudgetNow is the clock for max-retry-duration; tests replace it.
var retryBudgetNow = time.Now

type retryBudgetKey struct{}

type retryBudget struct {
	start    time.Time
	deadline time.Time
}

// withRetryBudget records when max-retry-duration runs out for this request. It is a plain
// timestamp rather than a context deadline: the retry loops stop starting new credential
// attempts after it, but an attempt already in flight or a stream already delivering is never
// cut short. Home owns retries when enabled, so no budget applies there.
func (m *Manager) withRetryBudget(ctx context.Context) context.Context {
	cfg := m.runtimeConfigSnapshot()
	if ctx == nil || cfg == nil || cfg.MaxRetryDuration <= 0 || m.HomeEnabled() {
		return ctx
	}
	start := retryBudgetNow()
	return context.WithValue(ctx, retryBudgetKey{}, retryBudget{start: start, deadline: start.Add(time.Duration(cfg.MaxRetryDuration) * time.Second)})
}

// retryBudgetExhausted reports whether the next round or credential, starting after waiting
// wait, would begin past the request's max-retry-duration. done counts the rounds or
// credentials already tried and is only logged.
func retryBudgetExhausted(ctx context.Context, wait time.Duration, next string, done int) bool {
	if ctx == nil {
		return false
	}
	budget, ok := ctx.Value(retryBudgetKey{}).(retryBudget)
	if !ok {
		return false
	}
	now := retryBudgetNow()
	if now.Add(wait).Before(budget.deadline) {
		return false
	}
	logEntryWithRequestID(ctx).Debugf("retry budget spent after %s and %d %ss; not starting another %s", now.Sub(budget.start).Round(time.Millisecond), done, next, next)
	return true
}
