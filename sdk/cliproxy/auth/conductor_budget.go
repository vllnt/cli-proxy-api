package auth

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
)

// admitUpstreamModelsBudget re-checks the client key token budgets against the upstream models one
// credential resolved for this request. Admission only sees the model the client asked for, so
// without this check an alias that resolves to a budgeted model would bypass the budget.
//
// It returns models unchanged (no allocation) when the request carries no admission ticket or no
// model is over budget. Exhausted models are skipped, so a pooled alias can still reach a model that
// has budget left. When none is left it returns the budget error as a request stop: the request ends
// with a 429, no other credential is tried, and the credential is neither cooled down nor penalized
// because no attempt was made and no result is recorded.
func admitUpstreamModelsBudget(ctx context.Context, models []string) ([]string, error) {
	ticket := budget.TicketFromContext(ctx)
	if ticket == nil || len(models) == 0 {
		return models, nil
	}
	engine := budget.Default()
	var (
		allowed []string
		blocker *budget.ExhaustedError
	)
	for i, model := range models {
		exhausted := engine.CheckUpstream(ticket, model)
		if exhausted == nil {
			if allowed != nil {
				allowed = append(allowed, model)
			}
			continue
		}
		if allowed == nil {
			allowed = append(make([]string, 0, len(models)), models[:i]...)
		}
		if blocker == nil || exhausted.ResetAt.After(blocker.ResetAt) {
			blocker = exhausted
		}
	}
	if blocker == nil {
		return models, nil
	}
	if len(allowed) == 0 {
		return nil, wrapRequestStopError(blocker)
	}
	return allowed, nil
}
