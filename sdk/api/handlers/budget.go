package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	budgetRemainingHeader = "X-Budget-Remaining"
	budgetResetHeader     = "X-Budget-Reset"
	budgetModeHeader      = "X-Budget-Mode"
)

// AdmitBudget applies the client key token budgets to a request for model.
//
// It returns ctx carrying the admission ticket, so the usage record is charged to exactly what was
// admitted and the conductor can re-check the upstream model after alias resolution. A non-nil
// ErrorMessage (HTTP 429 with Retry-After) means an enforced budget is used up and the request must
// not run. Without budgets, for unlabelled keys, and on any internal error it returns ctx and nil.
func AdmitBudget(ctx context.Context, model string) (outCtx context.Context, errMsg *interfaces.ErrorMessage) {
	outCtx = ctx
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("budget: admission failed open after panic: %v", r)
			outCtx, errMsg = ctx, nil
		}
	}()
	engine := budget.Default()
	if !engine.Enabled() {
		return ctx, nil
	}
	apiKey := coreusage.APIKeyFromContext(ctx)
	if apiKey == "" {
		return ctx, nil
	}
	decision, ticket := engine.Admit(apiKey, model)
	if ticket == nil {
		return ctx, nil
	}
	outCtx = budget.WithTicket(ctx, ticket)
	if decision.Applies {
		setBudgetHeaders(ctx, decision)
	}
	if decision.Exhausted != nil {
		return outCtx, &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: decision.Exhausted}
	}
	return outCtx, nil
}

// setBudgetHeaders reports the matching rule with the least budget left, on streaming and non-streaming
// responses alike. Headers cannot change once the response has started, so a response a keep-alive
// already started goes without, and so do websocket turns.
//
// Admission can run while a keep-alive goroutine writes the same response. The writer guard orders the
// two, and a keep-alive that holds it is committing the response, so there is nothing left to set.
func setBudgetHeaders(ctx context.Context, decision budget.Decision) {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Writer == nil {
		return
	}
	if guard := responseWriterGuard(ginCtx); guard != nil {
		if !guard.TryLock() {
			return
		}
		defer guard.Unlock()
	}
	if ginCtx.Writer.Written() {
		return
	}
	header := ginCtx.Writer.Header()
	header.Set(budgetRemainingHeader, strconv.FormatInt(max(decision.RemainingTokens, 0), 10))
	header.Set(budgetResetHeader, decision.ResetAt.UTC().Format(time.RFC3339))
	header.Set(budgetModeHeader, decision.Mode)
}

// WriteBudgetError answers err as a rejected token budget: HTTP 429, Retry-After and the JSON error
// body. Code that selects a credential itself uses it for the error AdmitBudget produced. It reports
// false and writes nothing when err is not a budget error.
func WriteBudgetError(c *gin.Context, err error) bool {
	var exhausted *budget.ExhaustedError
	if c == nil || !errors.As(err, &exhausted) {
		return false
	}
	for _, value := range exhausted.SafeResponseHeaders().Values("Retry-After") {
		c.Writer.Header().Set("Retry-After", value)
	}
	c.Data(exhausted.StatusCode(), "application/json", []byte(exhausted.Error()))
	return true
}
