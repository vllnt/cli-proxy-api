package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Live selection picks its credential directly, so it must judge the client key token budget itself,
// on every route that selects a credential, and answer with the budget error body.
func TestSelectOAuthHonorsClientKeyBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-realtime-2"
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{statusCode: http.StatusOK})
	registerCredential(t, manager, &auth.Auth{
		ID:       "live-budget",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	})
	cfg := &config.Config{}
	cfg.APIKeys = []string{"test-key-agents", "test-key-other"}
	cfg.APIKeyLabels = map[string]string{"agents": "test-key-agents"}
	cfg.APIKeyBudgets = &config.APIKeyBudgets{Labels: map[string][]config.APIKeyBudgetRule{
		"agents": {{Models: "gpt-realtime-*", Window: "day", Limit: 1000, Mode: "enforce"}},
	}}
	engine := budget.New(budget.WithClock(func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }))
	if err := engine.Apply(&cfg.SDKConfig); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })
	handler := NewHandler(manager, cfg)

	ginFor := func(path, apiKey string) (context.Context, *gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		c.Set("userApiKey", apiKey)
		return context.WithValue(context.Background(), "gin", c), c, recorder
	}

	ctx, _, _ := ginFor("/v1/realtime/calls", "test-key-agents")
	if _, selected, err := handler.selectOAuth(ctx, model, coreexecutor.Options{}); err != nil || selected == nil {
		t.Fatalf("selection with budget left = %v, %v; want a credential", selected, err)
	}

	_, ticket := engine.Admit("test-key-agents", model)
	engine.HandleUsage(budget.WithTicket(context.Background(), ticket), coreusage.Record{
		APIKey: "test-key-agents", Provider: "codex", Model: model,
		Detail: coreusage.Detail{InputTokens: 5000, TotalTokens: 5000},
	})

	for _, path := range []string{"/v1/realtime/calls", "/v1/live/calls"} {
		ctx, c, recorder := ginFor(path, "test-key-agents")
		selection, selected, err := handler.selectOAuth(ctx, model, coreexecutor.Options{})
		var exhausted *budget.ExhaustedError
		if selected != nil || selection != nil || !errors.As(err, &exhausted) {
			t.Fatalf("%s: exhausted selection = %v, %v, %v; want a budget error and no credential", path, selection, selected, err)
		}
		writeSelectionError(c, err)
		if recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status = %d, want 429; body=%s", path, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Retry-After"); got != "43200" {
			t.Fatalf("%s: Retry-After = %q, want 43200", path, got)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil || body.Error.Code != "budget_exhausted" {
			t.Fatalf("%s: body = %s (%v), want a budget_exhausted error", path, recorder.Body.String(), errDecode)
		}
		if strings.Contains(recorder.Body.String(), "test-key-agents") {
			t.Fatalf("%s: body leaks the client key: %s", path, recorder.Body.String())
		}
	}

	ctx, _, _ = ginFor("/v1/realtime/calls", "test-key-other")
	if _, selected, err := handler.selectOAuth(ctx, model, coreexecutor.Options{}); err != nil || selected == nil {
		t.Fatalf("unlabelled key selection = %v, %v; want it unaffected", selected, err)
	}
}
