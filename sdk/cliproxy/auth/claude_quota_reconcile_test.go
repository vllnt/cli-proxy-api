package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type quotaUsageTransport func(*http.Request) (*http.Response, error)

func (f quotaUsageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f quotaUsageTransport) RoundTripperFor(*Auth) http.RoundTripper           { return f }

func quotaUsageResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

const restoredQuotaUsage = `{"five_hour":{"utilization":0},"seven_day":{"utilization":42}}`

func quotaReconcileFixture(t *testing.T) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, &HeadroomSelector{}, nil)
	a, errRegister := m.Register(context.Background(), &Auth{
		ID: t.Name(), Provider: "claude", Status: StatusActive,
		Metadata:   map[string]any{"access_token": "synthetic-oauth-token"},
		Attributes: map[string]string{AttributeRuntimeOnly: "true"},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-test"}, {ID: "claude-opus-test"}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	quotaReconcileReject(m, a.ID, "claude-sonnet-test", "5h")
	m.SetRoundTripperProvider(quotaUsageTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != claudeUsageURL || r.Body != nil {
			t.Fatalf("unexpected provider request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-oauth-token" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
			t.Fatal("missing saved OAuth authorization")
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > claudeQuotaReadTimeout {
			t.Fatal("quota operation is not bounded")
		}
		return quotaUsageResponse(restoredQuotaUsage), nil
	}))
	return m, a
}

func quotaReconcileReject(m *Manager, id, model, window string) {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-" + window + "-Status": []string{"rejected"},
	})
	after := 24 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: model, CredentialScope: true, RetryAfter: &after, Error: &Error{HTTPStatus: 429, Message: "quota rejected"}})
}

func TestClaudeQuotaReconcileRestoresOnlyProvenQuota(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		clear      bool
	}{
		{"restored", restoredQuotaUsage, true},
		{"still exhausted", `{"five_hour":{"utilization":100},"seven_day":{"utilization":42}}`, false},
		{"different exhausted window", `{"five_hour":{"utilization":0},"seven_day":{"utilization":100}}`, false},
		{"model window exhausted", `{"five_hour":{"utilization":0},"seven_day_sonnet":{"utilization":100}}`, false},
		{"null short window", `{"five_hour":null,"seven_day":{"utilization":42}}`, false},
		{"unknown short window", `{"seven_day":{"utilization":42}}`, false},
		{"past reset", `{"five_hour":{"utilization":0,"resets_at":"2020-01-01T00:00:00Z"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a := quotaReconcileFixture(t)
			m.SetRoundTripperProvider(quotaUsageTransport(func(*http.Request) (*http.Response, error) { return quotaUsageResponse(tc.body), nil }))
			result, errReconcile := m.ReconcileClaudeQuotaByIndex(context.Background(), a.Index)
			if errReconcile != nil {
				t.Fatal(errReconcile)
			}
			got, _ := m.GetByID(a.ID)
			if got.Quota.Exceeded == tc.clear {
				t.Fatalf("clear=%v quota=%+v", tc.clear, got.Quota)
			}
			if result.ObservedAt.IsZero() || got.Quota.ObservedAt != result.ObservedAt || got.Quota.Signals["Anthropic-Usage-Source"] != "oauth_usage" {
				t.Fatal("fresh observation missing")
			}
			if got.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] != "" {
				t.Fatal("old rejected observation republished")
			}
			if !tc.clear && result.Outcome != "blocked" {
				t.Fatalf("outcome=%s", result.Outcome)
			}
		})
	}
}

func TestClaudeQuotaReconcilePreservesIndependentRestrictions(t *testing.T) {
	m, a := quotaReconcileFixture(t)
	now := time.Now()
	m.mu.Lock()
	current := m.auths[a.ID]
	shadow := &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), LastError: &Error{HTTPStatus: 503, Message: "independent failure"}}
	current.ModelStates["claude-opus-test"] = &ModelState{Quota: cooldownFieldsOf(current.Quota), Status: StatusError, Unavailable: true, NextRetryAfter: current.NextRetryAfter}
	current.ModelStates["claude-opus-test"].credentialQuotaShadow = shadow
	current.ModelStates["claude-opus-test"].credentialQuotaPropagated = true
	current.forceCooldownUntil = now.Add(2 * time.Hour)
	m.mu.Unlock()
	if _, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID); errReconcile != nil {
		t.Fatal(errReconcile)
	}
	got, _ := m.GetByID(a.ID)
	state := got.ModelStates["claude-opus-test"]
	if got.Quota.Exceeded || !got.Unavailable || !got.forceCooldownUntil.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("independent credential force altered: %+v", got)
	}
	if state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(shadow.NextRetryAfter) {
		t.Fatalf("independent model shadow lost: %+v", state)
	}
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "active force", false: "expired force"}[active], func(t *testing.T) {
			m, a := quotaReconcileFixture(t)
			m.mu.Lock()
			if active {
				m.auths[a.ID].credQuotaForceUntil = time.Now().Add(time.Hour)
			} else {
				m.auths[a.ID].credQuotaForceUntil = time.Now().Add(-time.Hour)
			}
			m.mu.Unlock()
			_, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID)
			if errReconcile != nil {
				t.Fatal(errReconcile)
			}
			got, _ := m.GetByID(a.ID)
			if got.Quota.Exceeded != active {
				t.Fatalf("force active=%v quota=%+v", active, got.Quota)
			}
		})
	}
}

func TestClaudeQuotaReconcileRejectsAmbiguousUsage(t *testing.T) {
	for _, body := range []string{
		`{}`, `[]`, `{"five_hour":null}`, `{"five_hour":{"utilization":-1}}`, `{"five_hour":{"utilization":101}}`,
		`{"five_hour":{"utilization":"0"}}`, `{"five_hour":{"utilization":0,"resets_at":"invalid"}}`,
		`{"five_hour":{"utilization":0},"five_hour":{"utilization":100}}`,
		`{"five_hour":{"utilization":0,"utilization":100}}`, `{"five_hour":{"utilization":0,"resets_at":null,"resets_at":"2027-01-01T00:00:00Z"}}`,
	} {
		if _, errParse := parseClaudeQuotaUsage([]byte(body)); !errors.Is(errParse, ErrQuotaUsageUnavailable) {
			t.Fatalf("accepted ambiguous usage: %s", body)
		}
	}
}

func TestClaudeQuotaReconcileFencesConcurrentChanges(t *testing.T) {
	for _, kind := range []string{"new rejection", "disable", "token replacement", "remove", "registration replacement", "file replacement", "file deletion", "out of order"} {
		t.Run(kind, func(t *testing.T) {
			m, a := quotaReconcileFixture(t)
			var path string
			if strings.HasPrefix(kind, "file ") {
				path = filepath.Join(t.TempDir(), "credential.json")
				if errWrite := os.WriteFile(path, []byte(`{"access_token":"synthetic-oauth-token"}`), 0600); errWrite != nil {
					t.Fatal(errWrite)
				}
				m.mu.Lock()
				delete(m.auths[a.ID].Attributes, AttributeRuntimeOnly)
				m.auths[a.ID].Attributes[AttributePath] = path
				m.mu.Unlock()
			}
			m.SetRoundTripperProvider(quotaUsageTransport(func(*http.Request) (*http.Response, error) {
				switch kind {
				case "new rejection":
					quotaReconcileReject(m, a.ID, "claude-opus-test", "7d")
				case "disable", "token replacement":
					updated, _ := m.GetByID(a.ID)
					if kind == "disable" {
						updated.Disabled = true
					} else {
						updated.Metadata["access_token"] = "replacement-token"
					}
					if _, errUpdate := m.Update(context.Background(), updated); errUpdate != nil {
						t.Fatal(errUpdate)
					}
				case "remove":
					m.Remove(context.Background(), a.ID)
				case "registration replacement":
					m.Remove(context.Background(), a.ID)
					if _, errRegister := m.Register(context.Background(), &Auth{ID: a.ID, Provider: "claude", Metadata: map[string]any{"access_token": "replacement-token"}}); errRegister != nil {
						t.Fatal(errRegister)
					}
				case "file replacement":
					if errWrite := os.WriteFile(path, []byte(`{"access_token":"replacement-token"}`), 0600); errWrite != nil {
						t.Fatal(errWrite)
					}
				case "file deletion":
					if errRemove := os.Remove(path); errRemove != nil {
						t.Fatal(errRemove)
					}
				case "out of order":
					m.SetRoundTripperProvider(quotaUsageTransport(func(*http.Request) (*http.Response, error) {
						return quotaUsageResponse(`{"five_hour":{"utilization":100}}`), nil
					}))
					if _, errNew := m.ReconcileClaudeQuota(context.Background(), a.ID); errNew != nil {
						t.Fatal(errNew)
					}
				}
				return quotaUsageResponse(restoredQuotaUsage), nil
			}))
			if result, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID); result != nil || !errors.Is(errReconcile, ErrQuotaObservationStale) {
				t.Fatalf("result=%+v err=%v", result, errReconcile)
			}
			if kind == "disable" {
				got, _ := m.GetByID(a.ID)
				if !got.Disabled {
					t.Fatal("stale read re-enabled credential")
				}
			}
			if kind != "remove" && kind != "registration replacement" && kind != "disable" {
				got, _ := m.GetByID(a.ID)
				if !got.Quota.Exceeded {
					t.Fatal("stale observation cleared newer cooldown")
				}
			}
			if kind == "file deletion" {
				if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
					t.Fatal("deleted OAuth file recreated")
				}
			}
		})
	}
}

func TestClaudeQuotaReconcileProviderErrorsStayBlocked(t *testing.T) {
	for _, kind := range []string{"redirect", "failure", "oversize", "transport"} {
		t.Run(kind, func(t *testing.T) {
			m, a := quotaReconcileFixture(t)
			var calls int
			m.SetRoundTripperProvider(quotaUsageTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != claudeUsageURL {
					t.Fatal("redirect followed")
				}
				response := quotaUsageResponse(restoredQuotaUsage)
				switch kind {
				case "redirect":
					response.StatusCode = 302
					response.Header.Set("Location", "https://example.invalid/steal")
				case "failure":
					response.StatusCode = 401
				case "oversize":
					response.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", 1024*1024+1)))
				case "transport":
					return nil, errors.New("synthetic-oauth-token")
				}
				return response, nil
			}))
			_, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID)
			if !errors.Is(errReconcile, ErrQuotaUsageUnavailable) || strings.Contains(errReconcile.Error(), "synthetic-oauth-token") || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, errReconcile)
			}
			got, _ := m.GetByID(a.ID)
			if !got.Quota.Exceeded || got.QuotaReconcile.Status != "provider_error" {
				t.Fatal("failed read changed routing or freshness")
			}
		})
	}
}

func TestClaudeQuotaAutomaticRecoveryCadenceAndCancellation(t *testing.T) {
	for _, kind := range []string{"success", "missing file", "provider error", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			m, a := quotaReconcileFixture(t)
			now := time.Now()
			m.mu.Lock()
			m.auths[a.ID].Quota.ObservedAt = now.Add(-time.Hour)
			m.auths[a.ID].NextRefreshAfter = now.Add(2 * time.Hour)
			m.mu.Unlock()
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.SetRoundTripperProvider(quotaUsageTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("quota read missing deadline")
				}
				if kind == "provider error" {
					return nil, errors.New("unavailable")
				}
				if kind == "cancellation" {
					cancel()
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return quotaUsageResponse(restoredQuotaUsage), nil
			}))
			if kind == "missing file" {
				m.mu.Lock()
				delete(m.auths[a.ID].Attributes, AttributeRuntimeOnly)
				m.auths[a.ID].Attributes[AttributePath] = filepath.Join(t.TempDir(), "missing.json")
				m.mu.Unlock()
			}
			loop := newAuthAutoRefreshLoop(m, time.Second, 1)
			loop.handleDueAuth(ctx, now, a.ID)
			var job *authRefreshJob
			select {
			case job = <-loop.jobs:
			default:
				t.Fatal("due quota not queued without provider executor")
			}
			if !job.quotaReconcile {
				t.Fatal("queued token refresh instead of quota read")
			}
			loop.runRefreshJob(ctx, job)
			m.mu.RLock()
			remainingJob := m.refreshJobs[a.ID]
			next, scheduled := m.nextRefreshCheckAt(now, m.auths[a.ID], time.Second)
			marker := m.auths[a.ID].NextRefreshAfter
			m.mu.RUnlock()
			if remainingJob != nil || scheduled && !next.After(now) {
				t.Fatalf("job not released or immediate retry: next=%v", next)
			}
			if !marker.Equal(now.Add(2 * time.Hour)) {
				t.Fatal("quota job overwrote OAuth refresh schedule")
			}
			loop.handleDueAuth(context.Background(), now, a.ID)
			select {
			case <-loop.jobs:
				t.Fatal("immediate quota retry storm")
			default:
			}
			want := int32(1)
			if kind == "missing file" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("provider calls=%d want=%d", calls.Load(), want)
			}
			got, _ := m.GetByID(a.ID)
			if kind == "success" && got.Quota.Exceeded {
				t.Fatal("automatic non-spending read did not restore routing")
			}
		})
	}
}

func TestClaudeQuotaAutomaticRecoveryExcludesHomeAndDisabled(t *testing.T) {
	m, a := quotaReconcileFixture(t)
	m.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	got, _ := m.GetByID(a.ID)
	if _, scheduled := m.claudeQuotaRevalidateAt(got, time.Now()); scheduled {
		t.Fatal("Home quota job scheduled")
	}
	m.SetConfig(&config.Config{})
	got.Disabled = true
	if _, scheduled := m.claudeQuotaRevalidateAt(got, time.Now()); scheduled {
		t.Fatal("disabled quota job scheduled")
	}
}

func TestClaudeQuotaObservationReplacesSelectorMemory(t *testing.T) {
	for _, name := range []string{"headroom", "reset aware"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			var selector Selector
			if name == "headroom" {
				selector = fixedHeadroomSelector(now)
			} else {
				selector = &ResetAwareSelector{nowFunc: func() time.Time { return now }}
			}
			a := headroomAuth("restored", now, 1, 4, 1, 20)
			b := headroomAuth("other", now, .7, 4, .8, 100)
			if got, errPick := selector.Pick(context.Background(), "claude", "", executor.Options{}, []*Auth{a, b}); errPick != nil || got.ID != b.ID {
				t.Fatalf("old observation not used: %v %v", got, errPick)
			}
			a.Quota.Signals = claudeUsageSignals(claudeQuotaUsage{"five_hour": {used: 0, reset: now.Add(time.Hour)}, "seven_day": {used: 1, reset: now.Add(2 * time.Hour)}})
			if got, errPick := selector.Pick(context.Background(), "claude", "", executor.Options{}, []*Auth{a, b}); errPick != nil || got.ID != a.ID {
				t.Fatalf("fresh usage retained old 100%% cache: %v %v", got, errPick)
			}
		})
	}
}

func TestClaudeQuotaReconcileRegistryRegistrationFence(t *testing.T) {
	m, a := quotaReconcileFixture(t)
	reg := registry.GetGlobalRegistry()
	m.SetRoundTripperProvider(quotaUsageTransport(func(*http.Request) (*http.Response, error) {
		// A model-list refresh can happen independently of manager mutation.
		// Publication must use the current registry epoch while the credential
		// gate prevents a replacement auth registration from joining it.
		reg.UnregisterClient(a.ID)
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-test"}})
		_, epoch := reg.GetModelsAndEpochForClient(a.ID)
		if !reg.ApplyClientModelProjections(a.ID, epoch, 1, []registry.ClientModelProjection{{ModelID: "claude-sonnet-test", QuotaExceeded: true}}) {
			t.Fatal("replacement projection failed")
		}
		return quotaUsageResponse(restoredQuotaUsage), nil
	}))
	if _, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID); errReconcile != nil {
		t.Fatal(errReconcile)
	}
	if reg.IsModelQuotaExceededForClient(a.ID, "claude-sonnet-test") {
		t.Fatal("restored availability was not projected to current model-list registration")
	}
}

func TestClaudeQuotaReconcilePreservesDisabledModelsAndUnknownClaims(t *testing.T) {
	m, a := quotaReconcileFixture(t)
	m.mu.Lock()
	m.auths[a.ID].ModelStates["disabled"] = &ModelState{Status: StatusDisabled, Unavailable: true, Quota: cooldownFieldsOf(m.auths[a.ID].Quota)}
	m.mu.Unlock()
	if _, errReconcile := m.ReconcileClaudeQuota(context.Background(), a.ID); errReconcile != nil {
		t.Fatal(errReconcile)
	}
	got, _ := m.GetByID(a.ID)
	if got.ModelStates["disabled"].Status != StatusDisabled {
		t.Fatal("quota reconcile enabled disabled model")
	}
	unknown := QuotaState{Exceeded: true, Reason: "credential_quota", cooldownEvaluated: true, cooldownSignals: map[string]string{"Anthropic-Ratelimit-Unified-Representative-Claim": "unrecognized"}}
	if claudeQuotaProvenRestored(unknown, claudeQuotaUsage{"five_hour": {used: 0}}, "", time.Now()) {
		t.Fatal("unknown claim accepted as restored")
	}
}

func TestClaudeQuotaReconcileModelLocalRestrictions(t *testing.T) {
	now := time.Now()
	q := QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour), cooldownEvaluated: true, cooldownSignals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "rejected"}}
	a := &Auth{Status: StatusError, ModelStates: map[string]*ModelState{}}
	for _, model := range []string{"claude-sonnet-test", "claude-opus-test", "forced"} {
		a.ModelStates[model] = &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: q, LastError: &Error{HTTPStatus: 429}}
	}
	a.ModelStates["forced"].forceCooldownSeeded = true
	a.ModelStates["forced"].forceCooldownUntil = now.Add(time.Hour)
	models, changed := reconcileClaudeQuotaState(a, claudeQuotaUsage{"five_hour": {used: 0}, "seven_day_sonnet": {used: 100}}, now)
	if !changed || len(models) != 1 || models[0] != "claude-opus-test" {
		t.Fatalf("model-local application=%v changed=%v", models, changed)
	}
	if a.ModelStates["claude-opus-test"].Quota.Exceeded || !a.ModelStates["claude-sonnet-test"].Quota.Exceeded || !a.ModelStates["forced"].Quota.Exceeded {
		t.Fatal("model-specific or forced restriction was broadened")
	}
}
