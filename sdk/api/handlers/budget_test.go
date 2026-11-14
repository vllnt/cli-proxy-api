package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	pluginapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	budgetKeyLabelled   = "test-key-budget-x"
	budgetKeyUnlabelled = "test-key-budget-other"
)

// budgetNoon is a Saturday: the day window resets in 12h, the week window in 36h.
var budgetNoon = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func budgetTestConfig(budgets *sdkconfig.APIKeyBudgets) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{
		APIKeys:       []string{budgetKeyLabelled, budgetKeyUnlabelled},
		APIKeyLabels:  map[string]string{"team-x": budgetKeyLabelled},
		APIKeyBudgets: budgets,
	}
}

func fableBudget(mode string, limit int64) *sdkconfig.APIKeyBudgets {
	return &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
		"team-x": {{Models: "claude-fable-*", Window: "day", Limit: limit, Mode: mode}},
	}}
}

// installBudgetEngine makes a fresh engine with a fixed clock the process default and applies budgets.
func installBudgetEngine(t *testing.T, budgets *sdkconfig.APIKeyBudgets) *budget.Engine {
	t.Helper()
	engine := budget.New(budget.WithClock(func() time.Time { return budgetNoon }))
	if err := engine.Apply(budgetTestConfig(budgets)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })
	return engine
}

// useBudget records tokens against the label of apiKey, as a finished request would.
func useBudget(engine *budget.Engine, apiKey, model string, tokens int64) {
	_, ticket := engine.Admit(apiKey, model)
	ctx := budget.WithTicket(context.Background(), ticket)
	engine.HandleUsage(ctx, coreusage.Record{
		APIKey:   apiKey,
		Provider: "claude",
		Model:    model,
		Detail:   coreusage.Detail{InputTokens: tokens, TotalTokens: tokens},
	})
}

func budgetRequest(apiKey string) (context.Context, *gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if apiKey != "" {
		c.Set("userApiKey", apiKey)
	}
	BudgetHeadersMiddleware()(c)
	return context.WithValue(context.Background(), "gin", c), c, recorder
}

func requireNoBudgetHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{"X-Budget-Remaining", "X-Budget-Reset", "X-Budget-Mode"} {
		if got := header.Get(name); got != "" {
			t.Fatalf("%s = %q, want it absent", name, got)
		}
	}
}

func TestAdmitBudgetIsANoOpWithoutConfiguration(t *testing.T) {
	previous := budget.SetDefault(budget.New())
	t.Cleanup(func() { budget.SetDefault(previous) })
	ctx, c, recorder := budgetRequest(budgetKeyLabelled)

	gotCtx, errMsg := AdmitBudget(ctx, "claude-fable-5")
	if errMsg != nil {
		t.Fatalf("AdmitBudget() = %+v, want nil", errMsg)
	}
	if gotCtx != ctx {
		t.Fatal("AdmitBudget must return the context unchanged when budgets are not configured")
	}
	requireNoBudgetHeaders(t, committedHeader(c))
	requireNoBudgetHeaders(t, recorder.Header())
}

func TestAdmitBudgetReportsHeadersAndAttachesTicket(t *testing.T) {
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 400)
	ctx, c, _ := budgetRequest(budgetKeyLabelled)

	gotCtx, errMsg := AdmitBudget(ctx, "claude-fable-5(high)")
	if errMsg != nil {
		t.Fatalf("AdmitBudget() = %+v, want nil", errMsg)
	}
	if budget.TicketFromContext(gotCtx) == nil {
		t.Fatal("a labelled key must carry an admission ticket")
	}
	header := committedHeader(c)
	if got := header.Get("X-Budget-Remaining"); got != "600" {
		t.Fatalf("X-Budget-Remaining = %q, want 600", got)
	}
	if got := header.Get("X-Budget-Reset"); got != "2026-11-15T00:00:00Z" {
		t.Fatalf("X-Budget-Reset = %q", got)
	}
	if got := header.Get("X-Budget-Mode"); got != "enforce" {
		t.Fatalf("X-Budget-Mode = %q", got)
	}
}

func TestAdmitBudgetLeavesUnaffectedRequestsAlone(t *testing.T) {
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 5000)

	tests := []struct {
		name  string
		key   string
		model string
	}{
		{"unlabelled key", budgetKeyUnlabelled, "claude-fable-5"},
		{"no principal", "", "claude-fable-5"},
		{"model no rule matches", budgetKeyLabelled, "claude-sonnet-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, c, _ := budgetRequest(tt.key)
			gotCtx, errMsg := AdmitBudget(ctx, tt.model)
			if errMsg != nil {
				t.Fatalf("AdmitBudget() = %+v, want nil", errMsg)
			}
			requireNoBudgetHeaders(t, committedHeader(c))
			if tt.key == budgetKeyLabelled {
				// A labelled key keeps its ticket so the conductor can still check the upstream model.
				if budget.TicketFromContext(gotCtx) == nil {
					t.Fatal("a labelled key must carry a ticket even when no rule matches the requested model")
				}
				return
			}
			if budget.TicketFromContext(gotCtx) != nil {
				t.Fatal("a request the budgets do not cover must carry no ticket")
			}
		})
	}
}

func TestAdmitBudgetRejectsExhaustedEnforceRule(t *testing.T) {
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 1000)
	ctx, c, _ := budgetRequest(budgetKeyLabelled)

	_, errMsg := AdmitBudget(ctx, "claude-fable-5")
	if errMsg == nil {
		t.Fatal("AdmitBudget() = nil, want a rejection")
	}
	if errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 429", errMsg.StatusCode)
	}
	var exhausted *budget.ExhaustedError
	if !errors.As(errMsg.Error, &exhausted) {
		t.Fatalf("Error = %T, want *budget.ExhaustedError", errMsg.Error)
	}
	if got := committedHeader(c).Get("X-Budget-Remaining"); got != "0" {
		t.Fatalf("X-Budget-Remaining = %q, want 0 on a rejected request", got)
	}

	// The same error must reach the client intact through the shared error writer.
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	_, outContext, outRecorder := budgetRequest("")
	handler.WriteErrorResponse(outContext, errMsg)
	if outRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", outRecorder.Code)
	}
	if got := outRecorder.Header().Get("Retry-After"); got != "43200" {
		t.Fatalf("Retry-After = %q, want 43200 (12h to the day reset)", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(outRecorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, outRecorder.Body.String())
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Code != "budget_exhausted" {
		t.Fatalf("error = %+v", body.Error)
	}
	for _, want := range []string{"team-x", "claude-fable-*"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Fatalf("message %q does not name %q", body.Error.Message, want)
		}
	}
	if strings.Contains(outRecorder.Body.String(), budgetKeyLabelled) {
		t.Fatal("the response body must never contain the client key")
	}
}

func TestAdmitBudgetObserveModeNeverRejects(t *testing.T) {
	engine := installBudgetEngine(t, fableBudget("observe", 1000))
	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 5000)
	ctx, c, _ := budgetRequest(budgetKeyLabelled)

	if _, errMsg := AdmitBudget(ctx, "claude-fable-5"); errMsg != nil {
		t.Fatalf("observe mode rejected a request: %+v", errMsg)
	}
	if got := committedHeader(c).Get("X-Budget-Mode"); got != "observe" {
		t.Fatalf("X-Budget-Mode = %q, want observe", got)
	}
	if got := committedHeader(c).Get("X-Budget-Remaining"); got != "0" {
		t.Fatalf("X-Budget-Remaining = %q, want 0 (floored)", got)
	}
}

func TestAdmitBudgetSkipsHeadersOnceTheResponseStarted(t *testing.T) {
	installBudgetEngine(t, fableBudget("enforce", 1000))
	ctx, c, recorder := budgetRequest(budgetKeyLabelled)
	c.Writer.WriteHeaderNow()

	if _, errMsg := AdmitBudget(ctx, "claude-fable-5"); errMsg != nil {
		t.Fatalf("AdmitBudget() = %+v, want nil", errMsg)
	}
	requireNoBudgetHeaders(t, recorder.Header())
}

func TestAdmitBudgetWorksWithoutAGinContext(t *testing.T) {
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	ctx := coreusage.WithAPIKey(context.Background(), budgetKeyLabelled)

	gotCtx, errMsg := AdmitBudget(ctx, "claude-fable-5")
	if errMsg != nil || budget.TicketFromContext(gotCtx) == nil {
		t.Fatalf("AdmitBudget() = %v, %+v", budget.TicketFromContext(gotCtx), errMsg)
	}
	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 1000)
	if _, errMsg := AdmitBudget(ctx, "claude-fable-5"); errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("AdmitBudget() after exhaustion = %+v, want 429", errMsg)
	}
}

func TestBudgetHeadersAreReservedFromUpstreams(t *testing.T) {
	src := http.Header{}
	src.Set("X-Budget-Remaining", "999999")
	src.Set("X-Budget-Reset", "2099-01-01T00:00:00Z")
	src.Set("X-Budget-Mode", "observe")
	src.Set("X-Request-Id", "req-1")
	filtered := FilterUpstreamHeaders(src)
	requireNoBudgetHeaders(t, filtered)
	if filtered.Get("X-Request-Id") != "req-1" {
		t.Fatal("unrelated upstream headers must be preserved")
	}
	for _, name := range []string{"X-Budget-Remaining", "x-budget-reset", "X-Budget-Mode"} {
		if !IsCPAReservedResponseHeader(name) {
			t.Fatalf("%s must be reserved", name)
		}
	}
}

// budgetExecutor counts invocations per entry point so a test can prove no credential was reached.
type budgetExecutor struct {
	*interceptorCaptureExecutor
	executes, counts, streams atomic.Int32
}

func newBudgetHandlerFixture(t *testing.T, model string, cfg *sdkconfig.SDKConfig) (*BaseAPIHandler, *budgetExecutor) {
	t.Helper()
	fixture := &budgetExecutor{}
	fixture.interceptorCaptureExecutor = &interceptorCaptureExecutor{
		provider: "handler-budget",
		execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			fixture.executes.Add(1)
			return coreexecutor.Response{Payload: []byte("ok")}, nil
		},
		executeCount: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
			fixture.counts.Add(1)
			return coreexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
		},
		stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			fixture.streams.Add(1)
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(fixture.interceptorCaptureExecutor)
	auth := &coreauth.Auth{ID: "budget-handler-auth.json", Provider: "handler-budget", Status: coreauth.StatusActive}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return NewBaseAPIHandlers(cfg, manager), fixture
}

func drainStream(data <-chan []byte, errs <-chan *interfaces.ErrorMessage) (chunks int, errMsg *interfaces.ErrorMessage) {
	if data != nil {
		for range data {
			chunks++
		}
	}
	return chunks, <-errs
}

func TestExecuteEnforcesTheKeyBudgetOnEveryGenerationPath(t *testing.T) {
	const model = "claude-fable-handler-budget"
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	handler, exec := newBudgetHandlerFixture(t, model, budgetTestConfig(fableBudget("enforce", 1000)))
	body := []byte(`{"model":"` + model + `"}`)

	t.Run("under budget runs and reports the budget", func(t *testing.T) {
		ctx, c, _ := budgetRequest(budgetKeyLabelled)
		if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "claude", model, body, ""); errMsg != nil {
			t.Fatalf("non-stream: %+v", errMsg)
		}
		if got := committedHeader(c).Get("X-Budget-Remaining"); got != "1000" {
			t.Fatalf("X-Budget-Remaining = %q, want 1000", got)
		}
		ctx, _, _ = budgetRequest(budgetKeyLabelled)
		data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, "claude", model, body, "")
		if chunks, errMsg := drainStream(data, errs); errMsg != nil || chunks != 1 {
			t.Fatalf("stream: chunks=%d err=%+v", chunks, errMsg)
		}
		if exec.executes.Load() != 1 || exec.streams.Load() != 1 {
			t.Fatalf("executes=%d streams=%d, want 1 each", exec.executes.Load(), exec.streams.Load())
		}
	})

	useBudget(engine, budgetKeyLabelled, model, 1000)
	exec.executes.Store(0)
	exec.streams.Store(0)

	t.Run("exhausted non-stream is rejected before any credential", func(t *testing.T) {
		ctx, _, _ := budgetRequest(budgetKeyLabelled)
		_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "claude", model, body, "")
		if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("ExecuteWithAuthManager() = %+v, want 429", errMsg)
		}
		var exhausted *budget.ExhaustedError
		if !errors.As(errMsg.Error, &exhausted) {
			t.Fatalf("Error = %T, want *budget.ExhaustedError", errMsg.Error)
		}
	})

	t.Run("exhausted stream reports 429 on the error channel", func(t *testing.T) {
		ctx, _, _ := budgetRequest(budgetKeyLabelled)
		data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, "claude", model, body, "")
		chunks, errMsg := drainStream(data, errs)
		if chunks != 0 || errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("stream: chunks=%d err=%+v, want 0 chunks and 429", chunks, errMsg)
		}
		if got := coreauth.SafeResponseHeaders(errMsg.Error).Get("Retry-After"); got != "43200" {
			t.Fatalf("Retry-After = %q, want 43200", got)
		}
	})

	if exec.executes.Load() != 0 || exec.streams.Load() != 0 {
		t.Fatalf("a rejected request reached a credential: executes=%d streams=%d", exec.executes.Load(), exec.streams.Load())
	}

	t.Run("count tokens is not budgeted", func(t *testing.T) {
		ctx, c, _ := budgetRequest(budgetKeyLabelled)
		if _, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "claude", model, body, ""); errMsg != nil {
			t.Fatalf("ExecuteCountWithAuthManager() = %+v, want it admitted", errMsg)
		}
		requireNoBudgetHeaders(t, committedHeader(c))
		if exec.counts.Load() != 1 {
			t.Fatalf("count executions = %d, want 1", exec.counts.Load())
		}
	})

	t.Run("an unlabelled key is unaffected", func(t *testing.T) {
		ctx, c, _ := budgetRequest(budgetKeyUnlabelled)
		if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "claude", model, body, ""); errMsg != nil {
			t.Fatalf("ExecuteWithAuthManager() = %+v", errMsg)
		}
		requireNoBudgetHeaders(t, committedHeader(c))
	})
}

func TestPluginExecutorRouteIsBudgeted(t *testing.T) {
	const pluginID = "budget-route-plugin"
	engine := installBudgetEngine(t, fableBudget("enforce", 1000))
	usagePlugin := newCapturePluginExecutorUsagePlugin(pluginID)
	registerUsagePluginForTest(t, "test-budget-plugin-route-capture", usagePlugin)
	registerUsagePluginForTest(t, "test-budget-plugin-route-engine", engine)

	host := &mockPluginUsageHost{execResp: coreexecutor.Response{
		Payload: []byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":400,"completion_tokens":0,"total_tokens":400}}`),
	}}
	host.hasRouters = true
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: pluginID}, true
	}
	handler := NewBaseAPIHandlers(budgetTestConfig(fableBudget("enforce", 1000)), nil)
	handler.SetModelRouterHost(host)
	body := []byte(`{"model":"claude-fable-5"}`)

	ctx, _, _ := budgetRequest(budgetKeyLabelled)
	if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "claude-fable-5", body, ""); errMsg != nil {
		t.Fatalf("first request: %+v", errMsg)
	}
	usagePlugin.waitRecord(t)
	waitForBudgetRemaining(t, engine, budgetKeyLabelled, "claude-fable-5", 600)

	useBudget(engine, budgetKeyLabelled, "claude-fable-5", 600)
	host.lastPluginID = ""
	ctx, _, _ = budgetRequest(budgetKeyLabelled)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "claude-fable-5", body, "")
	if errMsg == nil || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted plugin-executor request = %+v, want 429", errMsg)
	}
	if host.lastPluginID != "" {
		t.Fatal("a rejected request reached the plugin executor")
	}
}

// waitForBudgetRemaining waits for the asynchronous usage dispatch to charge the budget.
func waitForBudgetRemaining(t *testing.T, engine *budget.Engine, apiKey, model string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		dec, _ := engine.Admit(apiKey, model)
		if dec.RemainingTokens == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remaining tokens = %d, want %d", dec.RemainingTokens, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWriteBudgetError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exhausted := &budget.ExhaustedError{
		Label: "team-x", Pattern: "claude-fable-*", Window: "day",
		ResetAt: budgetNoon.Add(12 * time.Hour), RetryAfter: 12 * time.Hour,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	if !WriteBudgetError(c, fmt.Errorf("selecting a credential: %w", exhausted)) {
		t.Fatal("WriteBudgetError() = false, want true for a wrapped budget error")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "43200" {
		t.Fatalf("Retry-After = %q, want 43200", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", got)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Type != "rate_limit_error" || body.Error.Code != "budget_exhausted" {
		t.Fatalf("body = %s (%v), want the budget_exhausted error", recorder.Body.String(), err)
	}

	other := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(other)
	if WriteBudgetError(c, errors.New("upstream unavailable")) || WriteBudgetError(c, nil) || WriteBudgetError(nil, exhausted) {
		t.Fatal("WriteBudgetError() = true for something that is not a budget error on a usable context")
	}
	if other.Body.Len() != 0 || other.Code != http.StatusOK {
		t.Fatalf("a non-budget error must write nothing, got %d %q", other.Code, other.Body.String())
	}
}
