package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	budgetConductorKey   = "test-key-conductor-budget"
	budgetConductorLabel = "team-x"
	budgetAliasModel     = "team-model"
	budgetFableModel     = "claude-fable-5"
	budgetOtherModel     = "claude-sonnet-5"
)

// budgetNoon is a Saturday: the day window resets in 12h, the week window in 36h.
var budgetNoon = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type budgetResultHook struct {
	NoopHook
	results atomic.Int32
}

func (h *budgetResultHook) OnResult(context.Context, Result) { h.results.Add(1) }

func installBudgetEngine(t *testing.T, rules ...internalconfig.APIKeyBudgetRule) *budget.Engine {
	t.Helper()
	engine := budget.New(budget.WithClock(func() time.Time { return budgetNoon }))
	cfg := &internalconfig.SDKConfig{
		APIKeys:      []string{budgetConductorKey},
		APIKeyLabels: map[string]string{budgetConductorLabel: budgetConductorKey},
		APIKeyBudgets: &internalconfig.APIKeyBudgets{Labels: map[string][]internalconfig.APIKeyBudgetRule{
			budgetConductorLabel: rules,
		}},
	}
	if err := engine.Apply(cfg); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })
	return engine
}

func enforceRule(models, window string, limit int64) internalconfig.APIKeyBudgetRule {
	return internalconfig.APIKeyBudgetRule{Models: models, Window: window, Limit: limit, Mode: "enforce"}
}

// useBudgetTokens charges tokens to the label of the test key, as a finished request would.
func useBudgetTokens(engine *budget.Engine, model string, tokens int64) {
	_, ticket := engine.Admit(budgetConductorKey, model)
	engine.HandleUsage(budget.WithTicket(context.Background(), ticket), coreusage.Record{
		APIKey:   budgetConductorKey,
		Provider: "claude",
		Model:    model,
		Detail:   coreusage.Detail{InputTokens: tokens, TotalTokens: tokens},
	})
}

func ticketContext(t *testing.T, engine *budget.Engine, requestedModel string) context.Context {
	t.Helper()
	_, ticket := engine.Admit(budgetConductorKey, requestedModel)
	if ticket == nil {
		t.Fatal("the labelled key must get a ticket")
	}
	return budget.WithTicket(context.Background(), ticket)
}

type budgetConductor struct {
	manager *Manager
	hook    *budgetResultHook
	authID  string

	mu       sync.Mutex
	upstream []string
}

func (c *budgetConductor) record(model string) {
	c.mu.Lock()
	c.upstream = append(c.upstream, model)
	c.mu.Unlock()
}

func (c *budgetConductor) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.upstream...)
}

// newBudgetConductor registers one credential whose alias team-model resolves to the budgeted Fable model.
func newBudgetConductor(t *testing.T) *budgetConductor {
	t.Helper()
	const provider = "codex" // any provider works: the budget check does not depend on it
	c := &budgetConductor{hook: &budgetResultHook{}, authID: "budget-conductor-" + t.Name()}
	c.manager = NewManager(nil, nil, c.hook)
	c.manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		provider: {{Name: budgetFableModel, Alias: budgetAliasModel, Fork: true}},
	})
	auth := &Auth{ID: c.authID, Provider: provider, Status: StatusActive}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: budgetAliasModel}, {ID: budgetFableModel}, {ID: budgetOtherModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := c.manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	respond := func(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		c.record(req.Model)
		return cliproxyexecutor.Response{Payload: []byte(req.Model)}, nil
	}
	c.manager.RegisterExecutor(&customStreamMockExecutor{
		identifier:              provider,
		mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: respond, countFn: respond},
		streamFn: func(ctx context.Context, selected *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			response, err := respond(ctx, selected, req, opts)
			if err != nil {
				return nil, err
			}
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})
	return c
}

func (c *budgetConductor) requireCredentialUntouched(t *testing.T) {
	t.Helper()
	if got := c.hook.results.Load(); got != 0 {
		t.Fatalf("MarkResult ran %d times for a budget rejection, want 0", got)
	}
	auth, ok := c.manager.GetByID(c.authID)
	if !ok {
		t.Fatal("credential disappeared")
	}
	if auth.Unavailable || auth.LastError != nil || auth.StatusMessage != "" || !auth.NextRetryAfter.IsZero() ||
		len(auth.ModelStates) != 0 || auth.Quota.Exceeded || auth.Status != StatusActive {
		t.Fatalf("a budget rejection changed the credential: %+v", auth)
	}
}

func requireBudgetExhausted(t *testing.T, err error, retryAfter string) {
	t.Helper()
	var exhausted *budget.ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %T %v, want *budget.ExhaustedError", err, err)
	}
	if exhausted.Label != budgetConductorLabel {
		t.Fatalf("label = %q", exhausted.Label)
	}
	if got := clienterror.HTTPStatusFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", got)
	}
	if got := SafeResponseHeaders(err).Get("Retry-After"); got != retryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, retryAfter)
	}
}

func TestBudgetRejectsAnAliasOfABudgetedModelWithoutPenalizingTheCredential(t *testing.T) {
	engine := installBudgetEngine(t, enforceRule("claude-fable-*", "day", 1000))
	useBudgetTokens(engine, budgetFableModel, 1000)

	for _, path := range []string{"execute", "stream"} {
		t.Run(path, func(t *testing.T) {
			c := newBudgetConductor(t)
			// The alias names no budgeted model, so admission lets it through.
			ctx := ticketContext(t, engine, budgetAliasModel)
			var err error
			switch path {
			case "execute":
				_, err = c.manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: budgetAliasModel}, cliproxyexecutor.Options{})
			case "stream":
				_, err = c.manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: budgetAliasModel}, cliproxyexecutor.Options{Stream: true})
			}
			requireBudgetExhausted(t, err, "43200")
			if calls := c.calls(); len(calls) != 0 {
				t.Fatalf("the executor ran for a rejected request: %v", calls)
			}
			c.requireCredentialUntouched(t)

			// The credential is still healthy: a request that is not over budget runs on it.
			resp, errOK := c.manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: budgetAliasModel}, cliproxyexecutor.Options{})
			if errOK != nil || string(resp.Payload) != budgetFableModel {
				t.Fatalf("follow-up request = %q, %v", resp.Payload, errOK)
			}
			if got := c.hook.results.Load(); got != 1 {
				t.Fatalf("results after the follow-up = %d, want 1", got)
			}
		})
	}
}

func TestBudgetAllowsAnAliasWhileTheUpstreamModelHasBudget(t *testing.T) {
	engine := installBudgetEngine(t, enforceRule("claude-fable-*", "day", 1000))
	c := newBudgetConductor(t)
	ctx := ticketContext(t, engine, budgetAliasModel)

	resp, err := c.manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: budgetAliasModel}, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != budgetFableModel {
		t.Fatalf("Execute() = %q, %v", resp.Payload, err)
	}
}

func TestBudgetDoesNotApplyToCountTokens(t *testing.T) {
	engine := installBudgetEngine(t, enforceRule("claude-fable-*", "day", 1000))
	useBudgetTokens(engine, budgetFableModel, 1000)
	c := newBudgetConductor(t)
	ctx := ticketContext(t, engine, budgetAliasModel)

	if _, err := c.manager.ExecuteCount(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: budgetAliasModel}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("ExecuteCount() = %v, want it admitted", err)
	}
}

func TestAdmitUpstreamModelsBudget(t *testing.T) {
	engine := installBudgetEngine(t,
		enforceRule("claude-fable-*", "day", 1000),
		enforceRule("claude-opus-*", "week", 1000),
	)
	useBudgetTokens(engine, budgetFableModel, 1000)
	useBudgetTokens(engine, "claude-opus-5", 1000)
	ticketCtx := ticketContext(t, engine, budgetAliasModel)

	t.Run("no ticket returns the models untouched without allocating", func(t *testing.T) {
		models := []string{budgetFableModel}
		got, err := admitUpstreamModelsBudget(context.Background(), models)
		if err != nil || len(got) != 1 || &got[0] != &models[0] {
			t.Fatalf("got %v, %v", got, err)
		}
		if allocs := testing.AllocsPerRun(100, func() { _, _ = admitUpstreamModelsBudget(context.Background(), models) }); allocs != 0 {
			t.Fatalf("allocs = %v, want 0", allocs)
		}
	})

	t.Run("a model with budget left passes", func(t *testing.T) {
		models := []string{budgetOtherModel}
		got, err := admitUpstreamModelsBudget(ticketCtx, models)
		if err != nil || len(got) != 1 || got[0] != budgetOtherModel {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("an exhausted pooled model is skipped", func(t *testing.T) {
		got, err := admitUpstreamModelsBudget(ticketCtx, []string{budgetFableModel, budgetOtherModel})
		if err != nil || len(got) != 1 || got[0] != budgetOtherModel {
			t.Fatalf("got %v, %v, want only %s", got, err, budgetOtherModel)
		}
	})

	t.Run("every model exhausted stops the request with the latest reset", func(t *testing.T) {
		got, err := admitUpstreamModelsBudget(ticketCtx, []string{budgetFableModel, "claude-opus-5"})
		if len(got) != 0 || err == nil {
			t.Fatalf("got %v, %v, want a rejection", got, err)
		}
		if !isRequestStopError(err) {
			t.Fatal("the rejection must stop the request instead of rotating credentials")
		}
		requireBudgetExhausted(t, err, "129600")
	})
}
