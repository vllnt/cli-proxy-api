package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// This control-plane read is bounded to keep quota polling from starving OAuth
// refresh workers. Inference transports and their timeout policy are unchanged.
const claudeQuotaReadTimeout = 30 * time.Second
const claudeQuotaRevalidateInterval = 15 * time.Minute

var (
	ErrQuotaAuthNotFound      = errors.New("auth not found")
	ErrClaudeQuotaUnsupported = errors.New("quota reconciliation requires an enabled Claude OAuth credential")
	ErrQuotaObservationStale  = errors.New("credential changed during quota observation")
	ErrQuotaUsageUnavailable  = errors.New("provider quota usage unavailable")
	ErrQuotaPersistence       = errors.New("routing updated; persistence retry required")
)

// QuotaReconcileState separates the last provider read from a status snapshot.
// It is runtime-only: cooldown stores own durable routing restrictions.
type QuotaReconcileState struct {
	CheckedAt          time.Time `json:"checked_at,omitempty"`
	NextCheckAt        time.Time `json:"next_check_at,omitempty"`
	Status             string    `json:"status,omitempty"`
	PersistencePending bool      `json:"persistence_pending"`
}

type QuotaReconcileResult struct {
	AuthIndex            string    `json:"auth_index"`
	Outcome              string    `json:"outcome"`
	ObservedAt           time.Time `json:"observed_at"`
	Models               []string  `json:"models"`
	RemainingQuotaBlocks int       `json:"remaining_quota_blocks"`
}

type claudeUsageWindow struct {
	used  float64
	reset time.Time
}
type claudeQuotaUsage map[string]claudeUsageWindow

// ReconcileClaudeQuota performs a non-spending, fixed-origin usage read. Neither
// token renewal nor a successful read is treated as an inference success.
func (m *Manager) ReconcileClaudeQuota(ctx context.Context, id string) (*QuotaReconcileResult, error) {
	return m.reconcileClaudeQuota(ctx, id, 0)
}

// ReconcileClaudeQuotaByIndex binds the management target to its registration
// before any provider read, including replacement between lookup and dispatch.
func (m *Manager) ReconcileClaudeQuotaByIndex(ctx context.Context, index string) (*QuotaReconcileResult, error) {
	m.mu.RLock()
	var id string
	var epoch uint64
	for _, a := range m.auths {
		if a.Index == index {
			id, epoch = a.ID, a.RegistrationEpoch
			break
		}
	}
	m.mu.RUnlock()
	if id == "" {
		return nil, ErrQuotaAuthNotFound
	}
	return m.reconcileClaudeQuota(ctx, id, epoch)
}

func claudeQuotaEligible(a *Auth) bool {
	return a != nil && strings.EqualFold(a.Provider, "claude") && a.AuthKind() == AuthKindOAuth &&
		!a.Disabled && a.Status != StatusDisabled && !hasUnauthorizedAuthFailure(a) &&
		!a.RequiresSignIn && a.RejectedAccessToken == "" && authAccessToken(a) != ""
}

func (m *Manager) reconcileClaudeQuota(ctx context.Context, id string, epoch uint64) (*QuotaReconcileResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, claudeQuotaReadTimeout)
	defer cancel()
	base, ok := m.GetByID(id)
	if !ok {
		return nil, ErrQuotaObservationStale
	}
	if m.HomeEnabled() || !claudeQuotaEligible(base) {
		return nil, ErrClaudeQuotaUnsupported
	}
	if epoch != 0 && base.RegistrationEpoch != epoch {
		return nil, ErrQuotaObservationStale
	}
	file, digest, errFile := m.claudeQuotaFileFence(base)
	if errFile != nil {
		return nil, ErrQuotaObservationStale
	}
	usage, errRead := m.readClaudeQuotaUsage(ctx, base)
	now := time.Now()
	// Serialize commit with failures, disablement, removal and replacement. The
	// provider read holds no manager or credential mutation lock.
	release, errLock := m.lockAuthMutationContext(ctx, id)
	if errLock != nil {
		return nil, errLock
	}
	defer release()
	if file != "" {
		data, errDisk := os.ReadFile(file)
		if errDisk != nil || canonicalAuthFileDigest(data) != digest {
			return nil, ErrQuotaObservationStale
		}
	}
	m.mu.Lock()
	current := m.auths[id]
	if current == nil || current.Generation != base.Generation || current.RegistrationEpoch != base.RegistrationEpoch ||
		current.CredentialVersion != base.CredentialVersion || CredentialsChanged(current, base) || !claudeQuotaEligible(current) {
		m.mu.Unlock()
		return nil, ErrQuotaObservationStale
	}
	current.QuotaReconcile.CheckedAt = now
	current.QuotaReconcile.NextCheckAt = now.Add(claudeQuotaRevalidateInterval)
	if errRead != nil {
		current.QuotaReconcile.Status = "provider_error"
		current.Generation++
		m.mu.Unlock()
		release()
		m.queueRefreshReschedule(id)
		return nil, errRead
	}
	models, changed := reconcileClaudeQuotaState(current, usage, now)
	remaining := claudeQuotaBlockCount(current, now)
	outcome := "unchanged"
	if changed {
		outcome = "reconciled"
	} else if remaining > 0 {
		outcome = "blocked"
	}
	current.QuotaReconcile.Status = outcome
	current.QuotaReconcile.PersistencePending = true
	current.Generation++
	current.UpdatedAt = now
	snapshot := current.Clone()
	m.mu.Unlock()

	// Publish the same generation to both routing owners. Their generation
	// fences reject this snapshot if a concurrent result has already advanced it.
	registered, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(id)
	projections := make([]registry.ClientModelProjection, 0, len(registered))
	for _, model := range registered {
		if model != nil {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(id, regEpoch, snapshot.Generation, projections)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	release()
	m.queueRefreshReschedule(id)
	result := &QuotaReconcileResult{AuthIndex: snapshot.Index, Outcome: outcome, ObservedAt: now, Models: models, RemainingQuotaBlocks: remaining}
	// Always retry the durable snapshot, even when this call changed no routing
	// fields. Otherwise an earlier failed deletion would survive a restart.
	m.configCooldownMu.Lock()
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	persisted := m.persistCooldownStatesToLocked(ctx, store)
	m.configCooldownMu.Unlock()
	m.mu.Lock()
	if latest := m.auths[id]; latest != nil && latest.RegistrationEpoch == snapshot.RegistrationEpoch && latest.CredentialVersion == snapshot.CredentialVersion && latest.Generation == snapshot.Generation {
		latest.QuotaReconcile.PersistencePending = !persisted
		if !persisted {
			latest.QuotaReconcile.Status = "persistence_failed"
		}
	}
	m.mu.Unlock()
	if !persisted {
		return result, ErrQuotaPersistence
	}
	return result, nil
}

func (m *Manager) claudeQuotaFileFence(a *Auth) (string, string, error) {
	if a.AuthSourceKind() != AuthSourceFile {
		return "", "", nil
	}
	path := authAttribute(a, AttributePath)
	if path == "" {
		path = a.FileName
	}
	if path == "" {
		return "", "", ErrQuotaObservationStale
	}
	if !filepath.IsAbs(path) {
		cfg, _ := m.runtimeConfig.Load().(*config.Config)
		if cfg != nil && cfg.AuthDir != "" {
			path = filepath.Join(cfg.AuthDir, path)
		}
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", "", errRead
	}
	// A file replacement not yet adopted by the watcher must not let the old
	// runtime token authorize clearing the new credential's cooldown.
	if ojson.GetBytes(data, "access_token").String() != authAccessToken(a) || ojson.GetBytes(data, "disabled").Bool() {
		return "", "", ErrQuotaObservationStale
	}
	return path, canonicalAuthFileDigest(data), nil
}

func (m *Manager) readClaudeQuotaUsage(ctx context.Context, a *Auth) (claudeQuotaUsage, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		return nil, ErrQuotaUsageUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+authAccessToken(a))
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	proxyURL := strings.TrimSpace(a.ProxyURL)
	if proxyURL == "" {
		cfg, _ := m.runtimeConfig.Load().(*config.Config)
		if cfg != nil {
			proxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	var transport http.RoundTripper
	ownedTransport := false
	if proxyURL != "" {
		var errProxy error
		transport, _, errProxy = proxyutil.BuildHTTPTransport(proxyURL)
		ownedTransport = true
		if errProxy != nil {
			return nil, ErrQuotaUsageUnavailable
		}
	} else {
		transport = m.roundTripperFor(a)
	}
	if transport == nil {
		direct := http.DefaultTransport.(*http.Transport).Clone()
		direct.Proxy = nil
		transport = direct
		ownedTransport = true
	}
	if ownedTransport {
		if idle, ok := transport.(interface{ CloseIdleConnections() }); ok {
			defer idle.CloseIdleConnections()
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, ErrQuotaUsageUnavailable
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("quota usage response close failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrQuotaUsageUnavailable
	}
	const maxUsageBody = 1024 * 1024
	body, errBody := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	if errBody != nil || len(body) > maxUsageBody {
		return nil, ErrQuotaUsageUnavailable
	}
	return parseClaudeQuotaUsage(body)
}

func parseClaudeQuotaUsage(body []byte) (claudeQuotaUsage, error) {
	root := ojson.ParseBytes(body)
	if !ojson.ValidBytes(body) || !root.IsObject() {
		return nil, ErrQuotaUsageUnavailable
	}
	usage := make(claudeQuotaUsage)
	valid := true
	seen := make(map[string]bool)
	root.ForEach(func(key, value ojson.Result) bool {
		name := key.String()
		if seen[name] {
			valid = false
			return false
		}
		seen[name] = true
		if !claudeUsageWindowKnown(name) || value.Type == ojson.Null {
			return true
		}
		fields := make(map[string]bool)
		value.ForEach(func(k, _ ojson.Result) bool {
			field := k.String()
			if fields[field] {
				valid = false
			}
			fields[field] = true
			return true
		})
		if !valid {
			return false
		}
		util := value.Get("utilization")
		if !value.IsObject() || util.Type != ojson.Number || util.Float() < 0 || util.Float() > 100 {
			valid = false
			return false
		}
		window := claudeUsageWindow{used: util.Float()}
		reset := value.Get("resets_at")
		if reset.Type != ojson.Null {
			var errTime error
			window.reset, errTime = time.Parse(time.RFC3339Nano, reset.String())
			if reset.Type != ojson.String || errTime != nil {
				valid = false
				return false
			}
		}
		usage[name] = window
		return true
	})
	if !valid || len(usage) == 0 {
		return nil, ErrQuotaUsageUnavailable
	}
	return usage, nil
}

func claudeUsageWindowKnown(name string) bool {
	switch name {
	case "five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps":
		return true
	}
	return false
}

func claudeUsageName(window string) string {
	switch window {
	case "5h":
		return "five_hour"
	case "7d":
		return "seven_day"
	}
	if suffix, ok := strings.CutPrefix(window, "7d_"); ok {
		return "seven_day_" + suffix
	}
	return window
}

func claudeQuotaProvenRestored(q QuotaState, usage claudeQuotaUsage, model string, now time.Time) bool {
	// A restored old window cannot override newly observed exhaustion. A broad
	// credential restriction remains conservative across model-specific windows.
	for name, window := range usage {
		applicable := model == "" || name != "seven_day_opus" && name != "seven_day_sonnet" || strings.Contains(strings.ToLower(model), strings.TrimPrefix(name, "seven_day_"))
		if applicable && window.used >= 100 {
			return false
		}
	}
	signals := q.cooldownSignals
	if len(signals) == 0 && !q.cooldownEvaluated {
		signals = q.Signals
	}
	claims := make(map[string]bool)
	for key, value := range signals {
		if window, ok := claudeUnifiedWindowName(key); ok && strings.EqualFold(value, "rejected") {
			claims[claudeUsageName(window)] = true
		}
	}
	if claim := strings.TrimSpace(signals["Anthropic-Ratelimit-Unified-Representative-Claim"]); claim != "" {
		claims[claim] = true
	}
	// No known attribution is not evidence of restored capacity. In particular,
	// a null short window cannot be inferred from a healthy weekly percentage.
	if len(claims) == 0 {
		return false
	}
	for claim := range claims {
		window, ok := usage[claim]
		if !ok || !claudeUsageWindowKnown(claim) || window.used >= 100 || !window.reset.IsZero() && !window.reset.After(now) {
			return false
		}
	}
	return true
}

func claudeUsageSignals(usage claudeQuotaUsage) map[string]string {
	signals := map[string]string{"Anthropic-Usage-Source": "oauth_usage"}
	for name, window := range usage {
		prefix := "Anthropic-Usage-" + name + "-"
		signals[prefix+"Utilization"] = strconv.FormatFloat(window.used, 'f', -1, 64)
		if !window.reset.IsZero() {
			signals[prefix+"Resets-At"] = window.reset.Format(time.RFC3339Nano)
		}
	}
	return signals
}

func reconcileClaudeQuotaState(a *Auth, usage claudeQuotaUsage, now time.Time) ([]string, bool) {
	models := make([]string, 0)
	credentialCleared := a.Quota.Exceeded && a.Quota.Reason == "credential_quota" &&
		!effectiveCredQuotaForceUntil(a).After(now) && claudeQuotaProvenRestored(a.Quota, usage, "", now)
	if credentialCleared {
		applyCooldownFields(&a.Quota, QuotaState{})
		a.credQuotaForceUntil = time.Time{}
		models = append(models, modelsForRegisteredAuth(a.ID)...)
		// Drop only the outer quota layer. Independent auth retry/force layers
		// are reapplied by updateAggregatedAvailability below.
		a.NextRetryAfter = time.Time{}
		a.Unavailable = false
		if statusCodeFromResult(a.LastError) == http.StatusTooManyRequests {
			a.LastError = nil
			a.StatusMessage = ""
		}
	}
	for model, state := range a.ModelStates {
		if state == nil {
			continue
		}
		if state.Status == StatusDisabled {
			continue
		}
		wrapped := state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota"
		if wrapped && credentialCleared {
			if shadow := state.credentialQuotaShadow; shadow != nil {
				*state = *shadow.cloneShallow()
			} else {
				resetModelState(state, now)
			}
			state.credentialQuotaPropagated = false
			state.credentialQuotaShadow = nil
			models = append(models, model)
		} else if !wrapped && state.Status != StatusDisabled && state.Quota.Exceeded && state.Quota.Reason == "quota" &&
			!effectiveModelForceUntil(state).After(now) && statusCodeFromResult(state.LastError) == http.StatusTooManyRequests &&
			claudeQuotaProvenRestored(state.Quota, usage, model, now) {
			resetModelState(state, now)
			models = append(models, model)
		}
		// Observations are snapshots of this read, never a merge with a prior
		// rejected/100% signal. This does not erase independent cooldown proof.
		state.Quota.ObservedAt = now
		state.Quota.Signals = claudeUsageSignals(usage)
	}
	a.Quota.ObservedAt = now
	a.Quota.Signals = claudeUsageSignals(usage)
	if credentialCleared || len(models) > 0 {
		if a.Quota.Reason == "quota" && len(a.ModelStates) > 0 {
			applyCooldownFields(&a.Quota, QuotaState{})
		}
		updateAggregatedAvailability(a, now)
		if !a.Unavailable && !hasModelError(a, now) && a.LastError == nil {
			a.Status = StatusActive
			a.StatusMessage = ""
		}
	}
	models = dedupeStrings(models)
	sort.Strings(models)
	return models, credentialCleared || len(models) > 0
}

func claudeQuotaBlockCount(a *Auth, now time.Time) int {
	count := 0
	if a.Quota.Exceeded && a.Quota.Reason == "credential_quota" {
		count++
	}
	for _, state := range a.ModelStates {
		if state != nil && state.Quota.Exceeded && isModelStateActiveCooldown(state, now) {
			count++
		}
	}
	return count
}

func (m *Manager) claudeQuotaRevalidateAt(a *Auth, now time.Time) (time.Time, bool) {
	if m.HomeEnabled() || !claudeQuotaEligible(a) || !a.HasValidAccessToken(now) || (!a.QuotaReconcile.PersistencePending && claudeQuotaBlockCount(a, now) == 0) {
		return time.Time{}, false
	}
	if !a.QuotaReconcile.NextCheckAt.IsZero() {
		return a.QuotaReconcile.NextCheckAt, true
	}
	since := a.Quota.ObservedAt
	if since.IsZero() {
		return now, true
	}
	return since.Add(claudeQuotaRevalidateInterval), true
}
