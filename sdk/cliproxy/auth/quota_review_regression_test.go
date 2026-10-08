package auth

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaReviewPluginDelegateUsesApprovedMembership(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, weighted := range []bool{false, true} {
			for _, delegate := range []string{pluginapi.SchedulerBuiltinFillFirst, pluginapi.SchedulerBuiltinRoundRobin} {
				t.Run(fmt.Sprintf("mixed=%v/weighted=%v/%s", mixed, weighted, delegate), func(t *testing.T) {
					now := time.Now()
					var fallback Selector = &RoundRobinSelector{}
					if weighted {
						fallback = &WeightedRoundRobinSelector{}
					}
					manager := NewManager(nil, &QuotaAwareSelector{Fallback: fallback}, nil)
					a := quotaTestAuth("a", "codex", now, quotaTestSignals("codex", now, 100, 100))
					quotaTestExhaust(a)
					if weighted {
						a.Quota.ClearObservationSignals()
						a.Attributes = map[string]string{AttributeWeight: "0"}
					}
					b := quotaTestAuth("b", "codex", now, quotaTestSignals("codex", now, 10, 10))
					c := quotaTestAuth("c", "codex", now, quotaTestSignals("codex", now, 10, 10))
					if mixed {
						c.Provider = "claude"
						c.Quota.Signals = quotaTestSignals("claude", now, 10, 10)
					}
					for _, candidate := range []*Auth{a, b, c} {
						if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
							t.Fatal(errRegister)
						}
						manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: candidate.Provider})
					}
					plugin := &fakePluginScheduler{resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: delegate}, handled: true}
					manager.SetPluginScheduler(plugin)
					var picks []string
					for i := 0; i < 2; i++ {
						var got *Auth
						var errPick error
						if mixed {
							got, _, _, errPick = manager.pickNextMixed(context.Background(), []string{"codex", "claude"}, "", cliproxyexecutor.Options{}, nil)
						} else {
							got, _, errPick = manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
						}
						if errPick != nil || got == nil {
							t.Fatalf("delegate could not select usable auth: %v, %v", got, errPick)
						}
						picks = append(picks, got.ID)
					}
					for _, req := range plugin.requests {
						var ids []string
						for _, candidate := range req.Candidates {
							ids = append(ids, candidate.ID)
						}
						if !slices.Equal(ids, []string{"b", "c"}) {
							t.Fatalf("plugin candidates = %v, want b,c", ids)
						}
					}
					if slices.Contains(picks, "a") {
						t.Fatalf("native delegate reintroduced excluded a: %v", picks)
					}
					if delegate == pluginapi.SchedulerBuiltinRoundRobin && picks[0] == picks[1] {
						t.Fatalf("native round-robin state did not advance across approved candidates: %v", picks)
					}
				})
			}
		}
	}
}

func TestQuotaReviewManagerPassesHotHigherTierToQuotaSelector(t *testing.T) {
	now := time.Unix(1800000000, 0)
	high := quotaTestAuth("high-hot", "claude", now, quotaTestSignals("claude", now, 95, 20))
	high.Attributes = map[string]string{"priority": "2"}
	low := quotaTestAuth("low-cool", "claude", now, quotaTestSignals("claude", now, 20, 20))
	low.Attributes = map[string]string{"priority": "1"}
	selector := &QuotaAwareSelector{
		Fallback: &FillFirstSelector{},
		nowFunc:  func() time.Time { return now },
		randFunc: func() float64 { return 0 },
	}
	manager := NewManager(nil, selector, nil)
	priorityAuths, selectorAuths, errAvailable := manager.availableAuthsForSelector(selector, []*Auth{high, low}, "claude", "", now)
	if errAvailable != nil {
		t.Fatalf("availableAuthsForSelector() error = %v", errAvailable)
	}
	if len(priorityAuths) != 1 || priorityAuths[0].ID != high.ID {
		t.Fatalf("plugin priority candidates = %v, want only %s", authIDs(priorityAuths), high.ID)
	}
	if len(selectorAuths) != 2 {
		t.Fatalf("quota selector candidates = %v, want both priority tiers", authIDs(selectorAuths))
	}
	got, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, selectorAuths)
	if errPick != nil || got == nil || got.ID != low.ID {
		t.Fatalf("quota-aware pick = %v, %v; want lower cool tier %s", got, errPick, low.ID)
	}
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			ids = append(ids, auth.ID)
		}
	}
	return ids
}

func TestQuotaReviewXAITransportPreferenceDoesNotMigrateBinding(t *testing.T) {
	sticky := NewSessionAffinitySelector(&QuotaAwareSelector{Fallback: &FillFirstSelector{}})
	t.Cleanup(sticky.Stop)
	manager := NewManager(nil, sticky, nil)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "xai"})
	for _, candidate := range []*Auth{
		{ID: "xai-http", Provider: "xai"},
		{ID: "xai-ws", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	options := func(id string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{id}}}
	}
	if got, _, _, errPick := manager.pickNextMixed(context.Background(), []string{"xai"}, "", options("stable"), nil); errPick != nil || got == nil || got.ID != "xai-http" {
		t.Fatalf("initial HTTP binding = %v, %v", got, errPick)
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	if got, _, _, errPick := manager.pickNextMixed(ctx, []string{"xai"}, "", options("stable"), nil); errPick != nil || got == nil || got.ID != "xai-http" {
		t.Fatalf("transport preference migrated healthy binding = %v, %v", got, errPick)
	}
	if got, _, _, errPick := manager.pickNextMixed(ctx, []string{"xai"}, "", options("new"), nil); errPick != nil || got == nil || got.ID != "xai-ws" {
		t.Fatalf("new WebSocket binding = %v, %v", got, errPick)
	}
}

func TestQuotaReviewXAIWebsocketPreferenceOffAndOn(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "fill-first"} {
		for _, aware := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quota=%v", strategy, aware), func(t *testing.T) {
				var selector Selector = &RoundRobinSelector{}
				switch strategy {
				case "weighted-round-robin":
					selector = &WeightedRoundRobinSelector{}
				case "fill-first":
					selector = &FillFirstSelector{}
				}
				if aware {
					selector = &QuotaAwareSelector{Fallback: selector}
				}
				manager := NewManager(nil, selector, nil)
				manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "xai"})
				for _, candidate := range []*Auth{
					{ID: "xai-http", Provider: "xai"},
					{ID: "xai-ws-a", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
				} {
					if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
				got, _, errPick := manager.pickNext(ctx, "xai", "", cliproxyexecutor.Options{}, nil)
				if errPick != nil || got == nil || got.ID != "xai-ws-a" {
					t.Fatalf("websocket pick = %v, %v; want xai-ws-a", got, errPick)
				}
				got, _, _, errPick = manager.pickNextMixed(ctx, []string{"xai"}, "", cliproxyexecutor.Options{}, nil)
				if errPick != nil || got == nil || got.ID != "xai-ws-a" {
					t.Fatalf("single-provider mixed websocket pick = %v, %v; want xai-ws-a", got, errPick)
				}
			})
		}
	}
}
