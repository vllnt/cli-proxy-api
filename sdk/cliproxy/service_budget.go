package cliproxy

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// budgetUsagePluginName registers the budget engine by name, so starting twice replaces it instead of
// charging every record twice.
const budgetUsagePluginName = "budget"

// startBudgets installs the token budget policy of the initial config, registers the engine as a
// usage plugin (usage records are what it charges) and starts its flusher until ctx is cancelled.
func startBudgets(ctx context.Context, manager *usage.Manager, cfg *config.Config) {
	engine := budget.Default()
	manager.RegisterNamed(budgetUsagePluginName, engine)
	applyAPIKeyBudgets(cfg)
	engine.Start(ctx)
}

// applyAPIKeyBudgets installs the budget policy for cfg. Budgets are not supported in Home mode, so a
// Home config never enforces them. An invalid config keeps the previous policy; a committed config has
// already passed the same validation, so that is a safety net, not an expected path.
func applyAPIKeyBudgets(cfg *config.Config) {
	var sdkCfg *config.SDKConfig
	if cfg != nil && !cfg.Home.Enabled {
		sdkCfg = &cfg.SDKConfig
	}
	if err := budget.Default().Apply(sdkCfg); err != nil {
		log.WithError(err).Warn("api-key budgets were not applied; keeping the previous policy")
	}
}

// budgetShutdownWait bounds each of the two waits of stopUsageAndFlushBudgets, so a stuck usage plugin
// or a stalled KV store delays shutdown by seconds, not indefinitely.
const budgetShutdownWait = 5 * time.Second

// flushBudgets writes pending usage counts to the KV store. Call it before the store is closed.
func flushBudgets(ctx context.Context) {
	if err := budget.Default().Flush(ctx); err != nil {
		log.WithError(err).Warn("failed to flush api-key budget counters before shutdown")
	}
}

// stopUsageAndFlushBudgets stops the usage manager, waits (at most wait) for it to deliver the records
// still queued, so the budget engine charges them, then flushes the budget counters (again at most
// wait). It reports whether the flush finished. When it did not, a store write may still be running and
// the caller must not close the store under it.
func stopUsageAndFlushBudgets(ctx context.Context, manager *usage.Manager, wait time.Duration) bool {
	manager.Stop()
	drainCtx, cancelDrain := context.WithTimeout(ctx, wait)
	defer cancelDrain()
	if !manager.WaitStopped(drainCtx) {
		log.Warn("usage records were still queued at shutdown; api-key budgets may miss the last ones")
	}

	flushCtx, cancelFlush := context.WithTimeout(ctx, wait)
	defer cancelFlush()
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		flushBudgets(flushCtx)
	}()
	select {
	case <-flushed:
		return true
	case <-flushCtx.Done():
		select {
		case <-flushed:
			return true
		default:
			log.Warn("api-key budget flush did not finish before shutdown; the KV store is left open")
			return false
		}
	}
}
