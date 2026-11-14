package cliproxy

import (
	"context"

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

// flushBudgets writes pending usage counts to the KV store. Call it before the store is closed.
func flushBudgets(ctx context.Context) {
	if err := budget.Default().Flush(ctx); err != nil {
		log.WithError(err).Warn("failed to flush api-key budget counters before shutdown")
	}
}
