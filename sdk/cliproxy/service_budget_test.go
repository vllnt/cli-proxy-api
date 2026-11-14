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

// budgetsEnforced reports whether the engine applies the budgets of budgetServiceConfig to the test key.
func budgetsEnforced(engine *budget.Engine) bool {
	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	return ticket != nil
}

func TestCommitConfigUpdateAppliesAndValidatesAPIKeyBudgets(t *testing.T) {
	engine := isolateBudgetEngine(t)
	service := &Service{}
	if commit := service.commitConfigUpdate(budgetServiceConfig()); commit.cfg == nil {
		t.Fatal("a valid budgets config must be accepted")
	}
	if !engine.Enabled() || !budgetsEnforced(engine) {
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
	if !budgetsEnforced(engine) {
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
	t.Cleanup(cancel)

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
	t.Cleanup(cancel)
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
	engine := isolateBudgetEngine(t, withStore)
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
	requireRemainingAfterRestart(t, withStore, cfg, 1_300_000, "Shutdown did not flush pending usage")
}

// requireRemainingAfterRestart starts a fresh engine over the same store, as a new process would, and
// waits for it to report want tokens left. The persisted total is read in the background, so the first
// admissions still see the full limit.
func requireRemainingAfterRestart(t *testing.T, withStore budget.Option, cfg *internalconfig.Config, want int64, why string) {
	t.Helper()
	restarted := budget.New(withStore)
	if err := restarted.Apply(&cfg.SDKConfig); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		dec, _ := restarted.Admit(budgetServiceKey, "claude-fable-5")
		if dec.RemainingTokens == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remaining after restart = %d, want %d: %s", dec.RemainingTokens, want, why)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingUsagePlugin holds every record until gate is closed.
type blockingUsagePlugin struct{ gate chan struct{} }

func (p blockingUsagePlugin) HandleUsage(context.Context, usage.Record) { <-p.gate }

// stalledKV is a store whose writes never finish until release is closed.
type stalledKV struct {
	kvstore.KVClient
	release chan struct{}
}

func (s stalledKV) KVGet(context.Context, string) ([]byte, bool, error) { return nil, false, nil }

func (s stalledKV) KVIncrBy(context.Context, string, int64) (int64, error) {
	<-s.release
	return 0, nil
}

func chargeBudgetRecord(engine *budget.Engine, tokens int64) {
	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	engine.HandleUsage(budget.WithTicket(context.Background(), ticket), usage.Record{
		APIKey:   budgetServiceKey,
		Provider: "claude",
		Model:    "claude-fable-5",
		Detail:   usage.Detail{InputTokens: tokens, TotalTokens: tokens},
	})
}

// A record that is still queued behind a slow plugin when shutdown starts must be charged before the
// counters are flushed.
func TestShutdownChargesQueuedUsageBeforeFlushing(t *testing.T) {
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	withStore := budget.WithKV(func() (kvstore.KVClient, bool, error) { return store, true, nil })
	engine := isolateBudgetEngine(t, withStore)
	cfg := budgetServiceConfig()

	manager := usage.NewManager(16)
	gate := make(chan struct{})
	manager.Register(blockingUsagePlugin{gate: gate}) // delivered before the budget engine
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startBudgets(ctx, manager, cfg)
	_, ticket := engine.Admit(budgetServiceKey, "claude-fable-5")
	manager.Publish(budget.WithTicket(context.Background(), ticket), usage.Record{
		APIKey:   budgetServiceKey,
		Provider: "claude",
		Model:    "claude-fable-5",
		Detail:   usage.Detail{InputTokens: 700_000, TotalTokens: 700_000},
	})

	release := time.AfterFunc(50*time.Millisecond, func() { close(gate) })
	t.Cleanup(func() {
		if release.Stop() {
			close(gate)
		}
	})
	if !stopUsageAndFlushBudgets(context.Background(), manager, 5*time.Second) {
		t.Fatal("the budget flush did not finish")
	}

	requireRemainingAfterRestart(t, withStore, cfg, 1_300_000, "usage queued at shutdown was not charged before the flush")
}

// A plugin that never returns must not hold shutdown: the drain wait is bounded and the flush still runs.
func TestShutdownDoesNotWaitForeverForQueuedUsage(t *testing.T) {
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	withStore := budget.WithKV(func() (kvstore.KVClient, bool, error) { return store, true, nil })
	engine := isolateBudgetEngine(t, withStore)
	cfg := budgetServiceConfig()
	applyAPIKeyBudgets(cfg)
	chargeBudgetRecord(engine, 400_000)

	manager := usage.NewManager(4)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	manager.Register(blockingUsagePlugin{gate: gate})
	manager.Publish(context.Background(), usage.Record{Model: "m"})

	done := make(chan bool, 1)
	go func() { done <- stopUsageAndFlushBudgets(context.Background(), manager, 50*time.Millisecond) }()
	select {
	case flushed := <-done:
		if !flushed {
			t.Fatal("the flush must still run when the drain times out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown is held by a usage plugin that never returns")
	}
	requireRemainingAfterRestart(t, withStore, cfg, 1_600_000, "the flush did not run after the drain timed out")
}

// A store that stalls must not hold shutdown either, and the caller learns the flush is unfinished so it
// does not close a store that is still in use.
func TestShutdownDoesNotWaitForeverForAStalledFlush(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stalled := stalledKV{release: release}
	engine := isolateBudgetEngine(t,
		budget.WithKV(func() (kvstore.KVClient, bool, error) { return stalled, true, nil }))
	applyAPIKeyBudgets(budgetServiceConfig())
	chargeBudgetRecord(engine, 100_000)

	done := make(chan bool, 1)
	go func() {
		done <- stopUsageAndFlushBudgets(context.Background(), usage.NewManager(1), 50*time.Millisecond)
	}()
	select {
	case flushed := <-done:
		if flushed {
			t.Fatal("a flush that is still blocked on the store must be reported as unfinished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown is held by a stalled KV store")
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
