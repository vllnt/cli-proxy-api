package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const budgetServiceKey = "test-key-agents"

func budgetServiceConfig() *internalconfig.Config {
	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{budgetServiceKey}
	cfg.APIKeyLabels = map[string]string{"agents": budgetServiceKey}
	cfg.APIKeyBudgets = &internalconfig.APIKeyBudgets{Labels: map[string][]internalconfig.APIKeyBudgetRule{
		"agents": {{Models: "claude-fable-*", Window: "week", Limit: 2_000_000, Mode: "enforce"}},
	}}
	return cfg
}

// isolateBudgetEngine installs a fresh process-wide engine for the test and restores the previous one.
func isolateBudgetEngine(t *testing.T, opts ...budget.Option) *budget.Engine {
	t.Helper()
	engine := budget.New(opts...)
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })
	return engine
}

func budgetLabel(engine *budget.Engine) string {
	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	return ticket.Label()
}

func TestCommitConfigUpdateAppliesAndValidatesAPIKeyBudgets(t *testing.T) {
	engine := isolateBudgetEngine(t)
	service := &Service{}
	if commit := service.commitConfigUpdate(budgetServiceConfig()); commit.cfg == nil {
		t.Fatal("a valid budgets config must be accepted")
	}
	if !engine.Enabled() || budgetLabel(engine) != "agents" {
		t.Fatal("a committed config must install the budget policy")
	}

	broken := budgetServiceConfig()
	broken.APIKeyBudgets.Labels["ghost"] = []internalconfig.APIKeyBudgetRule{{Models: "*", Window: "day", Limit: 1}}
	if commit := service.commitConfigUpdate(broken); commit.cfg != nil {
		t.Fatal("a budget for a label without api-key-labels entry must be rejected")
	}

	home := budgetServiceConfig()
	home.Home.Enabled = true
	if commit := service.commitConfigUpdate(home); commit.cfg != nil {
		t.Fatal("budgets in Home mode must be rejected")
	}
	if budgetLabel(engine) != "agents" {
		t.Fatal("a rejected update must keep the last valid policy")
	}
	service.cfgMu.RLock()
	current := service.cfg
	service.cfgMu.RUnlock()
	if current == nil || current.APIKeyBudgets == nil || len(current.APIKeyBudgets.Labels) != 1 {
		t.Fatalf("a rejected update must keep the last valid config, got %+v", current)
	}

	cleared := budgetServiceConfig()
	cleared.APIKeyBudgets = nil
	if commit := service.commitConfigUpdate(cleared); commit.cfg == nil {
		t.Fatal("removing the budgets must be accepted")
	}
	if engine.Enabled() {
		t.Fatal("removing the budgets from the config must disable the engine")
	}
}

func TestStartBudgetsChargesPublishedUsage(t *testing.T) {
	engine := isolateBudgetEngine(t)
	manager := usage.NewManager(16)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	ctx, cancel := context.WithCancel(context.Background())

	startBudgets(ctx, manager, budgetServiceConfig())
	t.Cleanup(func() { cancel(); engine.Wait() })

	if !engine.Enabled() {
		t.Fatal("startBudgets must install the policy of the initial config")
	}
	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	manager.Publish(budget.WithTicket(context.Background(), ticket), usage.Record{
		APIKey:   budgetServiceKey,
		Provider: "claude",
		Model:    "claude-fable-5",
		Detail:   usage.Detail{InputTokens: 500_000, TotalTokens: 500_000},
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		dec, _ := engine.Admit(budgetServiceKey, "claude-fable-5")
		if dec.RemainingTokens == 1_500_000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remaining = %d, want 1500000: the engine is not registered as a usage plugin", dec.RemainingTokens)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartBudgetsIgnoresBudgetsInHomeMode(t *testing.T) {
	engine := isolateBudgetEngine(t)
	cfg := budgetServiceConfig()
	cfg.Home.Enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	startBudgets(ctx, usage.NewManager(1), cfg)
	t.Cleanup(func() { cancel(); engine.Wait() })
	if engine.Enabled() {
		t.Fatal("budgets are not supported in Home mode and must not be enforced")
	}
	startBudgets(ctx, usage.NewManager(1), nil)
	if engine.Enabled() {
		t.Fatal("a nil config must leave budgets disabled")
	}
}

// Pending usage must reach the KV store before the service closes it.
func TestShutdownFlushesPendingBudgetUsage(t *testing.T) {
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	withStore := budget.WithKV(func() (kvstore.KVClient, bool, error) { return store, true, nil })
	engine := isolateBudgetEngine(t, withStore, budget.WithFlushInterval(time.Hour))
	cfg := budgetServiceConfig()
	applyAPIKeyBudgets(cfg)
	if !engine.Enabled() {
		t.Fatal("policy not installed")
	}

	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	engine.HandleUsage(budget.WithTicket(context.Background(), ticket), usage.Record{
		APIKey:   budgetServiceKey,
		Provider: "claude",
		Model:    "claude-fable-5",
		Detail:   usage.Detail{InputTokens: 700_000, TotalTokens: 700_000},
	})

	service := &Service{cfg: cfg}
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("Shutdown() = %v", errShutdown)
	}

	// A new process (a fresh engine over the same store) sees the usage the first one had pending.
	restarted := budget.New(withStore)
	if err := restarted.Apply(&cfg.SDKConfig); err != nil {
		t.Fatal(err)
	}
	dec, _ := restarted.Admit(budgetServiceKey, "claude-fable-5")
	if dec.RemainingTokens != 1_300_000 {
		t.Fatalf("remaining after restart = %d, want 1300000: Shutdown did not flush pending usage", dec.RemainingTokens)
	}
}

func TestStageHomeOverlayRejectsBudgets(t *testing.T) {
	isolateBudgetEngine(t)
	service := &Service{cfg: &internalconfig.Config{}}
	service.cfg.Home.Enabled = true
	_, err := service.stageHomeOverlayWithClient(context.Background(), budgetServiceConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "home config rejected") || strings.Contains(err.Error(), budgetServiceKey) {
		t.Fatalf("stageHomeOverlayWithClient() error = %v, want a Home rejection that does not leak a key", err)
	}
}
