package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
)

// committedHeader commits the response and returns the headers the client receives.
func committedHeader(c *gin.Context) http.Header {
	c.Writer.WriteHeaderNow()
	return c.Writer.Header()
}

func requireBudgetHeaders(t *testing.T, header http.Header, remaining string) {
	t.Helper()
	if got := header.Get("X-Budget-Remaining"); got != remaining {
		t.Fatalf("X-Budget-Remaining = %q, want %q", got, remaining)
	}
	if header.Get("X-Budget-Reset") == "" || header.Get("X-Budget-Mode") != "enforce" {
		t.Fatalf("incomplete budget headers: %v", header)
	}
}

// Without budgets the middleware must leave the response writer alone: no wrapper, no per-write cost.
func TestBudgetHeadersMiddlewareDoesNotWrapTheWriterWithoutBudgets(t *testing.T) {
	previous := budget.SetDefault(budget.New())
	t.Cleanup(func() { budget.SetDefault(previous) })
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	original := c.Writer

	BudgetHeadersMiddleware()(c)

	if c.Writer != original {
		t.Fatalf("the response writer was wrapped (%T) although no budget is configured", c.Writer)
	}
	if _, held := c.Get(budgetHeadersKey); held {
		t.Fatal("a header holder was stored although no budget is configured")
	}
}

func TestBudgetHeadersMiddlewareWrapsTheWriterOnlyWhileBudgetsAreEnabled(t *testing.T) {
	installBudgetEngine(t, fableBudget("enforce", 1000))
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	original := c.Writer
	BudgetHeadersMiddleware()(c)
	if c.Writer == original {
		t.Fatal("the response writer was not wrapped although budgets are configured")
	}

	upgrade, _ := gin.CreateTestContext(httptest.NewRecorder())
	upgrade.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	upgrade.Request.Header.Set("Upgrade", "websocket")
	original = upgrade.Writer
	BudgetHeadersMiddleware()(upgrade)
	if upgrade.Writer != original {
		t.Fatal("a websocket upgrade must keep its own writer: its turns carry no budget headers")
	}
}

// Admission only records the headers; whoever commits the response applies them.
func TestBudgetHeadersAreAppliedByEveryWayOfCommittingTheResponse(t *testing.T) {
	installBudgetEngine(t, fableBudget("enforce", 1000))
	commits := map[string]func(c *gin.Context){
		"WriteHeaderNow":  func(c *gin.Context) { c.Writer.WriteHeaderNow() },
		"Write":           func(c *gin.Context) { _, _ = c.Writer.Write([]byte("x")) },
		"WriteString":     func(c *gin.Context) { _, _ = c.Writer.WriteString("x") },
		"Flush":           func(c *gin.Context) { c.Writer.Flush() },
		"WriteHeader+Now": func(c *gin.Context) { c.Writer.WriteHeader(http.StatusAccepted); c.Writer.WriteHeaderNow() },
	}
	for name, commit := range commits {
		t.Run(name, func(t *testing.T) {
			ctx, c, recorder := budgetRequest(budgetKeyLabelled)
			if _, errMsg := AdmitBudget(ctx, "claude-fable-5"); errMsg != nil {
				t.Fatalf("AdmitBudget() = %+v", errMsg)
			}
			if got := recorder.Header().Get("X-Budget-Remaining"); got != "" {
				t.Fatalf("admission wrote the header map itself (X-Budget-Remaining = %q)", got)
			}
			commit(c)
			requireBudgetHeaders(t, recorder.Result().Header, "1000")
		})
	}
}

func TestBudgetHeadersThatArriveAfterTheCommitAreDropped(t *testing.T) {
	installBudgetEngine(t, fableBudget("enforce", 1000))
	ctx, c, recorder := budgetRequest(budgetKeyLabelled)
	_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))

	if _, errMsg := AdmitBudget(ctx, "claude-fable-5"); errMsg != nil {
		t.Fatalf("AdmitBudget() = %+v", errMsg)
	}
	_, _ = c.Writer.Write([]byte("data: late\n\n"))
	requireNoBudgetHeaders(t, recorder.Result().Header)
}

// A handler that returns without writing still gets the headers: the middleware applies them before
// gin commits the empty response.
func TestBudgetHeadersReachAResponseWithoutABody(t *testing.T) {
	installBudgetEngine(t, fableBudget("enforce", 1000))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", budgetKeyLabelled) }, BudgetHeadersMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		_, _ = AdmitBudget(context.WithValue(c.Request.Context(), "gin", c), "claude-fable-5")
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	requireBudgetHeaders(t, recorder.Result().Header, "1000")
}
