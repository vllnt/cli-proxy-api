package musecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// The relay selects Meta credentials itself, so it must judge the client key token budget itself. It
// knows no model, so only the catch-all rules of the key apply.
func TestRelayHonorsClientKeyBudget(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.APIKeys = []string{"test-key-agents", "test-key-other"}
	cfg.APIKeyLabels = map[string]string{"agents": "test-key-agents"}
	cfg.APIKeyBudgets = &config.APIKeyBudgets{Labels: map[string][]config.APIKeyBudgetRule{
		"agents": {
			{Models: "claude-fable-*", Window: "week", Limit: 1, Mode: "enforce"},
			{Models: "*", Window: "day", Limit: 1000, Mode: "enforce"},
		},
	}}
	budgetEngine := budget.New(budget.WithClock(func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }))
	if err := budgetEngine.Apply(&cfg.SDKConfig); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(budgetEngine)
	t.Cleanup(func() { budget.SetDefault(previous) })

	manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(executor.NewMetaExecutor(cfg))
	registerMetaTestCred(t, manager, &auth.Auth{
		ID: "meta-budget", Provider: "meta", Status: auth.StatusActive,
		Attributes: map[string]string{"api_key": "meta-key", "base_url": upstream.URL + "/v1"},
	}, "muse-wonder-1.0")
	handler := NewHandler(manager, cfg)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userApiKey", c.GetHeader("X-Test-Client-Key"))
		c.Next()
	}, handlers.BudgetHeadersMiddleware())
	router.Any("/muse-code/*path", handler.Handle)
	search := func(clientKey string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/muse-code/search", strings.NewReader(`{"q":"go"}`))
		req.Header.Set("X-Test-Client-Key", clientKey)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	allowed := search("test-key-agents")
	if allowed.Code != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("with budget left: status = %d, upstream calls = %d, want 200 and 1; body=%s", allowed.Code, upstreamCalls.Load(), allowed.Body.String())
	}
	if got := allowed.Header().Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000 (the catch-all rule)", got)
	}

	_, ticket := budgetEngine.Admit("test-key-agents", "muse-wonder-1.0")
	budgetEngine.HandleUsage(budget.WithTicket(context.Background(), ticket), coreusage.Record{
		APIKey: "test-key-agents", Provider: "meta", Model: "muse-wonder-1.0",
		Detail: coreusage.Detail{InputTokens: 5000, TotalTokens: 5000},
	})
	blocked := search("test-key-agents")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted budget: status = %d, want 429; body=%s", blocked.Code, blocked.Body.String())
	}
	if got := blocked.Header().Get("Retry-After"); got != "43200" {
		t.Fatalf("Retry-After = %q, want 43200", got)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &body); err != nil || body.Error.Code != "budget_exhausted" {
		t.Fatalf("body = %s (%v), want a budget_exhausted error", blocked.Body.String(), err)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1: a rejected request must not reach a credential", upstreamCalls.Load())
	}

	if other := search("test-key-other"); other.Code != http.StatusOK || other.Header().Get("X-Budget-Remaining") != "" {
		t.Fatalf("unlabelled key: status = %d, X-Budget-Remaining = %q, want 200 and no header", other.Code, other.Header().Get("X-Budget-Remaining"))
	}
}
