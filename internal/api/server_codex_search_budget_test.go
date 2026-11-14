package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// The standalone search endpoint selects a credential itself, so it must judge the client key budget itself.
func TestCodexAlphaSearchHonorsTheClientKeyBudget(t *testing.T) {
	const apiKey = "test-key"
	const model = "gpt-6.1-sol"
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{
		APIKeys:      []string{apiKey},
		APIKeyLabels: map[string]string{"agents": apiKey},
		APIKeyBudgets: &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
			"agents": {{Models: "gpt-6.1-*", Window: "day", Limit: 1000, Mode: "enforce"}},
		}},
	}}
	engine := budget.New(budget.WithClock(func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }))
	if err := engine.Apply(&cfg.SDKConfig); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })

	server := newTestServerWithConfig(t, cfg)
	server.handlers.AuthManager.RegisterExecutor(executor.NewCodexAutoExecutor(server.getConfig()))
	credential := &auth.Auth{
		ID:       "codex-search-budget",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "oauth-token", "account_id": "account"},
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	if _, err := server.handlers.AuthManager.Register(t.Context(), credential); err != nil {
		t.Fatalf("register Codex credential: %v", err)
	}

	upstreamCalls := 0
	transport := codexSearchSIWCTransport(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"results":[]}`)),
			Request:    req,
		}, nil
	})
	search := func() *httptest.ResponseRecorder {
		t.Helper()
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
		req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"id":"budget-search","model":"`+model+`","commands":{"search_query":[{"q":"OpenAI"}]}}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}

	allowed := search()
	if allowed.Code != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("with budget left: status = %d, upstream calls = %d, want 200 and 1; body=%s", allowed.Code, upstreamCalls, allowed.Body.String())
	}
	if got := allowed.Header().Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000", got)
	}

	_, ticket := engine.Admit(apiKey, model)
	engine.HandleUsage(budget.WithTicket(context.Background(), ticket), coreusage.Record{
		APIKey: apiKey, Provider: "codex", Model: model,
		Detail: coreusage.Detail{InputTokens: 5000, TotalTokens: 5000},
	})
	blocked := search()
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted budget: status = %d, want 429; body=%s", blocked.Code, blocked.Body.String())
	}
	if got := blocked.Header().Get("Retry-After"); got != "43200" {
		t.Fatalf("Retry-After = %q, want 43200", got)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &body); err != nil || body.Error.Code != "budget_exhausted" {
		t.Fatalf("body = %s (%v), want a budget_exhausted error", blocked.Body.String(), err)
	}
	if strings.Contains(blocked.Body.String(), apiKey) {
		t.Fatalf("body leaks the client key: %s", blocked.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1: a rejected search must not reach a credential", upstreamCalls)
	}
}
