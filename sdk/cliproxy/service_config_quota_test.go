package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// This regression test uses only pre-existing APIs so it also runs against the
// unmodified source. Without quota-aware routing the configured strategy picks a.
func TestConfiguredQuotaRoutingPrefersHeadroomAndPreservesAffinity(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "fill-first"} {
		t.Run(strategy, func(t *testing.T) {
			cfg, errParse := internalconfig.ParseConfigBytes([]byte(fmt.Sprintf("routing:\n  strategy: %s\n  session-affinity: true\n  quota-aware: true\n  quota-max-age: 5m\n", strategy)))
			if errParse != nil {
				t.Fatal(errParse)
			}
			selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
			if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
				t.Cleanup(stoppable.Stop)
			}
			now := time.Now()
			a := &coreauth.Auth{ID: "a", Provider: "claude", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
					"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
					"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9",
					"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
				},
			}}
			b := a.Clone()
			b.ID = "b"
			b.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.2"
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"stable"}}}
			got, errPick := selector.Pick(context.Background(), "claude", "model", opts, []*coreauth.Auth{a, b})
			if errPick != nil || got == nil || got.ID != "b" {
				t.Fatalf("new session picked %v, %v; want higher-headroom account b", got, errPick)
			}
			a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.0"
			got, errPick = selector.Pick(context.Background(), "claude", "model", opts, []*coreauth.Auth{a, b})
			if errPick != nil || got == nil || got.ID != "b" {
				t.Fatalf("active session picked %v, %v; want pinned account b", got, errPick)
			}
		})
	}
}

func TestNormalizedQuotaRoutingRuntimeConfig(t *testing.T) {
	defaults := normalizedRoutingRuntimeState(nil)
	if defaults.quotaAware || defaults.quotaMaxAge != coreauth.DefaultQuotaMaxAge {
		t.Fatalf("defaults = %+v", defaults)
	}
	for _, raw := range []string{"", "bad", "0s", "-1m"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{QuotaAware: true, QuotaMaxAge: raw}})
		if state.quotaMaxAge != coreauth.DefaultQuotaMaxAge {
			t.Errorf("invalid age %q = %v", raw, state.quotaMaxAge)
		}
	}
	for _, raw := range []string{"", "1m", "invalid"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{QuotaMaxAge: raw}})
		if state != defaults {
			t.Errorf("disabled quota age %q changes runtime state", raw)
		}
	}
	state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{QuotaAware: true, QuotaMaxAge: "60s"}})
	if state.quotaMaxAge != time.Minute {
		t.Fatalf("age = %v, want 1m", state.quotaMaxAge)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.QuotaAwareSelector); !ok {
		t.Fatalf("selector = %T, want quota-aware without implicit affinity", newRoutingSelector(state))
	}
}

func TestServiceQuotaRoutingHotReload(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: true}}
	apply := func() {
		t.Helper()
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
			t.Fatal("applyManagerConfig failed")
		}
	}
	apply()
	initial := service.coreManager.Selector()
	cfg.Routing.QuotaMaxAge = "1m"
	apply()
	if service.coreManager.Selector() != initial {
		t.Fatal("disabled quota setting recreated affinity")
	}
	cfg.Routing.QuotaAware = true
	apply()
	enabled := service.coreManager.Selector()
	if enabled == initial {
		t.Fatal("enabling quota routing did not recreate selector")
	}
	cfg.Routing.QuotaMaxAge = "60s"
	apply()
	if service.coreManager.Selector() != enabled {
		t.Fatal("equivalent duration recreated selector")
	}
	cfg.Routing.QuotaMaxAge = "2m"
	apply()
	if service.coreManager.Selector() == enabled {
		t.Fatal("changed quota freshness did not recreate selector")
	}
	service.coreManager.StopAutoRefresh()
}
