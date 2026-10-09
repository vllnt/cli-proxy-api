package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildConfigChangeDetailsQuotaRouting(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{Routing: config.RoutingConfig{QuotaAware: true, QuotaMaxAge: "2m"}}
	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	for _, want := range []string{"routing.quota-aware: false -> true", "routing.quota-max-age:  -> 2m"} {
		if !slices.Contains(changes, want) {
			t.Errorf("changes=%v, missing %q", changes, want)
		}
	}
	if changes := BuildConfigChangeDetails(newCfg, newCfg); len(changes) != 0 {
		t.Errorf("unchanged quota routing produced changes %v", changes)
	}
}
