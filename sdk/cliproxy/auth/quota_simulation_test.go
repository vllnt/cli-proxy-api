package auth

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type quotaSimulationResult struct {
	peakSpread       int
	simulated429s    int
	firstExhaustStep int
	finalUtilization map[string]int
}

func runQuotaSimulation(t *testing.T, selector Selector, now time.Time) quotaSimulationResult {
	t.Helper()
	auths := []*Auth{
		quotaTestAuth("fast-burner", "claude", now, quotaTestSignals("claude", now, 0, 0)),
		quotaTestAuth("steady-a", "claude", now, quotaTestSignals("claude", now, 0, 0)),
		quotaTestAuth("steady-b", "claude", now, quotaTestSignals("claude", now, 0, 0)),
	}
	usage := map[string]int{"fast-burner": 0, "steady-a": 0, "steady-b": 0}
	peakSpread := 0
	simulated429s := 0
	firstExhaustStep := -1
	for step := 0; step < 140; step++ {
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			break
		}
		before := usage[picked.ID]
		if before >= 95 {
			simulated429s++
		}
		increment := 1
		if picked.ID == "fast-burner" {
			increment = 4
		}
		usage[picked.ID] += increment
		if usage[picked.ID] >= 100 && !picked.Unavailable {
			picked.Unavailable = true
			picked.Quota.Exceeded = true
			picked.Quota.Reason = "credential_quota"
			picked.Quota.NextRecoverAt = now.Add(5 * time.Hour)
			if firstExhaustStep < 0 {
				firstExhaustStep = step
			}
		}
		for _, auth := range auths {
			used := usage[auth.ID]
			auth.Quota.ObservedAt = now
			auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = fmt.Sprintf("%.2f", float64(used)/100)
			auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)
			auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "allowed"
		}
		minUsage, maxUsage := 100, 0
		for _, used := range usage {
			if used < minUsage {
				minUsage = used
			}
			if used > maxUsage {
				maxUsage = used
			}
		}
		if spread := maxUsage - minUsage; spread > peakSpread {
			peakSpread = spread
		}
	}
	return quotaSimulationResult{peakSpread: peakSpread, simulated429s: simulated429s, firstExhaustStep: firstExhaustStep, finalUtilization: usage}
}

func TestQuotaAwareSelectorSimulationReducesCascade(t *testing.T) {
	now := time.Unix(1800000000, 0)
	baseline := runQuotaSimulation(t, &RoundRobinSelector{}, now)
	randomState := uint64(1)
	quotaAware := &QuotaAwareSelector{
		Fallback: &RoundRobinSelector{},
		MaxAge:   DefaultQuotaMaxAge,
		nowFunc:  func() time.Time { return now },
		randFunc: func() float64 {
			randomState = randomState*6364136223846793005 + 1
			return float64(randomState>>11) / float64(uint64(1)<<53)
		},
	}
	aware := runQuotaSimulation(t, quotaAware, now)
	t.Logf("baseline: peak_utilization_spread=%d simulated_429s=%d first_exhaustion_step=%d final=%v", baseline.peakSpread, baseline.simulated429s, baseline.firstExhaustStep, baseline.finalUtilization)
	t.Logf("quota-aware: peak_utilization_spread=%d simulated_429s=%d first_exhaustion_step=%d final=%v", aware.peakSpread, aware.simulated429s, aware.firstExhaustStep, aware.finalUtilization)
	if aware.simulated429s >= baseline.simulated429s {
		t.Fatalf("quota-aware simulated limit events = %d, baseline = %d", aware.simulated429s, baseline.simulated429s)
	}
	if aware.peakSpread >= baseline.peakSpread {
		t.Fatalf("quota-aware peak utilization spread = %d, baseline = %d", aware.peakSpread, baseline.peakSpread)
	}
	if baseline.firstExhaustStep < 0 {
		t.Fatal("baseline did not exhaust the fast burner in the steady-load scenario")
	}
	if aware.firstExhaustStep >= 0 {
		t.Fatalf("quota-aware exhausted an account at step %d; want no cascade in this load window", aware.firstExhaustStep)
	}
}
