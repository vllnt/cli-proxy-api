package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Battle tests drive Manager.Execute with realistic Claude Code and Codex session
// signals across many credentials and assert that every logical session stays on
// one upstream credential, which is what keeps provider prompt caches warm.

const (
	battleClaudeModel = "battle-claude-model"
	battleCodexModel  = "battle-codex-model"
)

type battleRateLimitError struct{ retryAfter time.Duration }

func (e *battleRateLimitError) Error() string   { return "rate limited" }
func (e *battleRateLimitError) StatusCode() int { return http.StatusTooManyRequests }
func (e *battleRateLimitError) RetryAfter() *time.Duration {
	d := e.retryAfter
	return &d
}

type battleCall struct {
	session string
	authID  string
	failed  bool
}

type battleHarness struct {
	t       *testing.T
	manager *Manager
	mu      sync.Mutex
	calls   []battleCall
	failing map[string]bool
}

func newBattleHarness(t *testing.T, perProvider int) *battleHarness {
	t.Helper()
	withQuotaCooldownEnabled(t)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &QuotaAwareSelector{Fallback: &RoundRobinSelector{}}, TTL: time.Hour})
	t.Cleanup(selector.Stop)
	h := &battleHarness{t: t, manager: NewManager(nil, selector, nil), failing: map[string]bool{}}
	h.manager.SetRetryConfig(0, 0, 0)
	ctx := context.Background()
	for _, provider := range []string{"claude", "codex"} {
		model := battleClaudeModel
		if provider == "codex" {
			model = battleCodexModel
		}
		for i := 0; i < perProvider; i++ {
			id := fmt.Sprintf("%s-%s-%02d", t.Name(), provider, i)
			registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			if _, err := h.manager.Register(ctx, &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
				t.Fatal(err)
			}
		}
		h.manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: provider, executeFn: h.execute})
	}
	return h
}

func (h *battleHarness) execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	label, _ := opts.Metadata["battle_label"].(string)
	h.mu.Lock()
	failed := h.failing[auth.ID]
	h.calls = append(h.calls, battleCall{session: label, authID: auth.ID, failed: failed})
	h.mu.Unlock()
	if failed {
		return cliproxyexecutor.Response{}, &battleRateLimitError{retryAfter: time.Hour}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (h *battleHarness) setFailing(authID string, failing bool) {
	h.mu.Lock()
	h.failing[authID] = failing
	h.mu.Unlock()
}

func (h *battleHarness) resetCalls() {
	h.mu.Lock()
	h.calls = nil
	h.mu.Unlock()
}

func (h *battleHarness) snapshot() []battleCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]battleCall(nil), h.calls...)
}

// battleRequest mirrors what each client puts on the wire.
type battleRequest struct {
	label    string
	provider string
	headers  http.Header
	body     string
}

func claudeHeaderRequest(label, session, agent string) battleRequest {
	headers := http.Header{"X-Claude-Code-Session-Id": []string{session}}
	if agent != "" {
		headers.Set("X-Claude-Code-Agent-Id", agent)
	}
	return battleRequest{label: label, provider: "claude", headers: headers,
		body: `{"messages":[{"role":"user","content":"hi"}]}`}
}

func claudeUserIDRequest(label, session string) battleRequest {
	userID := fmt.Sprintf(`{"device_id":"dev","account_uuid":"acct","session_id":%q}`, session)
	return battleRequest{label: label, provider: "claude", headers: http.Header{},
		body: fmt.Sprintf(`{"metadata":{"user_id":%q},"messages":[{"role":"user","content":"hi"}]}`, userID)}
}

func codexHeaderRequest(label, session, thread string, subagent bool) battleRequest {
	headers := http.Header{"Session_id": []string{session}}
	if thread != "" {
		headers.Set("Thread-Id", thread)
	}
	if subagent {
		headers.Set("X-Openai-Subagent", "true")
	}
	return battleRequest{label: label, provider: "codex", headers: headers, body: `{"input":"hi"}`}
}

func codexCacheKeyRequest(label, session string) battleRequest {
	return battleRequest{label: label, provider: "codex", headers: http.Header{},
		body: fmt.Sprintf(`{"prompt_cache_key":%q,"input":"hi"}`, session)}
}

func (h *battleHarness) send(r battleRequest) (string, error) {
	model := battleClaudeModel
	if r.provider == "codex" {
		model = battleCodexModel
	}
	opts := cliproxyexecutor.Options{
		Headers:         r.headers.Clone(),
		OriginalRequest: []byte(r.body),
		Metadata:        map[string]any{"battle_label": r.label},
	}
	resp, err := h.manager.Execute(context.Background(), []string{r.provider}, cliproxyexecutor.Request{Model: model, Payload: []byte(r.body)}, opts)
	return string(resp.Payload), err
}

func (h *battleHarness) mustSend(r battleRequest) string {
	h.t.Helper()
	got, err := h.send(r)
	if err != nil {
		h.t.Fatalf("%s: unexpected error: %v", r.label, err)
	}
	return got
}

// Many concurrent multi-turn sessions across both providers and every supported
// session signal must each stay on one credential, while new sessions still spread.
func TestBattleAffinityConcurrentSessionsStayPinnedAndSpread(t *testing.T) {
	const accounts, sessionsPerKind, turns = 8, 64, 12
	h := newBattleHarness(t, accounts)

	var requests [][]battleRequest
	for i := 0; i < sessionsPerKind; i++ {
		kinds := []func(label string) battleRequest{
			func(l string) battleRequest { return claudeHeaderRequest(l, "cc-hdr-"+l, "") },
			func(l string) battleRequest { return claudeUserIDRequest(l, "cc-uid-"+l) },
			func(l string) battleRequest { return codexHeaderRequest(l, "cx-hdr-"+l, "", false) },
			func(l string) battleRequest { return codexCacheKeyRequest(l, "cx-pck-"+l) },
		}
		for k, build := range kinds {
			label := fmt.Sprintf("k%d-s%03d", k, i)
			session := make([]battleRequest, turns)
			for turn := range session {
				session[turn] = build(label)
			}
			requests = append(requests, session)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(requests)*turns)
	for _, session := range requests {
		wg.Add(1)
		go func(session []battleRequest) {
			defer wg.Done()
			for _, r := range session {
				if _, err := h.send(r); err != nil {
					errs <- fmt.Errorf("%s: %w", r.label, err)
				}
			}
		}(session)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	bySession := map[string]map[string]int{}
	for _, c := range h.snapshot() {
		if bySession[c.session] == nil {
			bySession[c.session] = map[string]int{}
		}
		bySession[c.session][c.authID]++
	}
	if len(bySession) != len(requests) {
		t.Fatalf("sessions observed = %d, want %d", len(bySession), len(requests))
	}
	perAuth := map[string]int{}
	for session, auths := range bySession {
		if len(auths) != 1 {
			t.Errorf("session %s split across credentials: %v", session, auths)
		}
		for id := range auths {
			perAuth[id]++
		}
	}
	// 128 sessions per provider over 8 credentials: round-robin binds 16 each.
	for id, n := range perAuth {
		if n < 8 || n > 24 {
			t.Errorf("credential %s bound %d sessions; distribution is badly skewed: %v", id, n, perAuth)
		}
	}
	if len(perAuth) != 2*accounts {
		t.Errorf("only %d of %d credentials received sessions: %v", len(perAuth), 2*accounts, perAuth)
	}
}

// A client may fire several requests for a brand-new session at the same time.
// Every one of them should land on the same credential, and the session must
// converge to a single credential for every following turn.
func TestBattleAffinityParallelColdStartConverges(t *testing.T) {
	const accounts, sessions, burst = 6, 24, 8
	h := newBattleHarness(t, accounts)

	splitBursts := 0
	for i := 0; i < sessions; i++ {
		label := fmt.Sprintf("cold-%02d", i)
		r := claudeHeaderRequest(label, "cold-session-"+label, "")
		if i%2 == 1 {
			r = codexHeaderRequest(label, "cold-session-"+label, "", false)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		burstAuths := make([]string, burst)
		for j := 0; j < burst; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				got, err := h.send(r)
				if err != nil {
					t.Errorf("%s burst %d: %v", label, j, err)
				}
				burstAuths[j] = got
			}(j)
		}
		close(start)
		wg.Wait()
		distinct := map[string]bool{}
		for _, id := range burstAuths {
			distinct[id] = true
		}
		if len(distinct) > 1 {
			splitBursts++
			t.Logf("%s: parallel cold-start requests used %d credentials: %v", label, len(distinct), burstAuths)
		}

		// Whatever happened during the burst, later turns must agree on one credential.
		first := h.mustSend(r)
		for turn := 0; turn < 5; turn++ {
			if got := h.mustSend(r); got != first {
				t.Fatalf("%s: post-burst turn %d moved from %s to %s", label, turn, first, got)
			}
		}
	}
	if splitBursts > 0 {
		t.Errorf("%d of %d sessions split their parallel cold-start requests across credentials", splitBursts, sessions)
	}
}

// When a bound credential gets rate-limited, its sessions must fail over within the
// same request, spread over healthy credentials, stay on the new credential, and not
// return when the old credential recovers. Unaffected sessions must not move.
func TestBattleAffinityFailoverIsStickyAndContained(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			const accounts, sessions = 5, 40
			h := newBattleHarness(t, accounts)
			build := func(i int) battleRequest {
				label := fmt.Sprintf("%s-fo-%02d", provider, i)
				if provider == "claude" {
					return claudeHeaderRequest(label, label, "")
				}
				return codexHeaderRequest(label, label, "", false)
			}
			bound := map[int]string{}
			for i := 0; i < sessions; i++ {
				bound[i] = h.mustSend(build(i))
			}
			victim := bound[0]
			h.setFailing(victim, true)
			h.resetCalls()

			moved := map[int]string{}
			for i := 0; i < sessions; i++ {
				got := h.mustSend(build(i))
				if got == victim {
					t.Fatalf("session %d still served by rate-limited credential %s", i, victim)
				}
				if bound[i] != victim && got != bound[i] {
					t.Errorf("session %d was not on the failing credential but moved %s -> %s", i, bound[i], got)
				}
				if bound[i] == victim {
					moved[i] = got
				}
			}
			failedAttempts := 0
			for _, c := range h.snapshot() {
				if c.failed {
					failedAttempts++
				}
			}
			if failedAttempts != 1 {
				t.Errorf("rate-limited credential was attempted %d times; cooldown should skip it after the first 429", failedAttempts)
			}
			targets := map[string]int{}
			for _, id := range moved {
				targets[id]++
			}
			if len(moved) > 2 && len(targets) < 2 {
				t.Errorf("all %d displaced sessions piled onto one credential: %v", len(moved), targets)
			}

			// Victim recovers; established bindings must not flap back.
			h.setFailing(victim, false)
			h.manager.MarkResult(context.Background(), Result{AuthID: victim, Provider: provider, Model: map[string]string{"claude": battleClaudeModel, "codex": battleCodexModel}[provider], Success: true})
			for turn := 0; turn < 3; turn++ {
				for i, want := range moved {
					if got := h.mustSend(build(i)); got != want {
						t.Fatalf("session %d flapped after recovery: %s -> %s", i, want, got)
					}
				}
			}
		})
	}
}

// Subagents spawned by Claude Code and Codex must reuse their parent's credential,
// even when many parents are active so round-robin alone would scatter them.
func TestBattleAffinitySubagentsInheritParentCredential(t *testing.T) {
	const accounts, parents, subagents = 6, 18, 4
	h := newBattleHarness(t, accounts)
	var wg sync.WaitGroup
	for i := 0; i < parents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ccSession := fmt.Sprintf("cc-parent-%02d", i)
			cxSession := fmt.Sprintf("cx-parent-%02d", i)
			ccParent, errCC := h.send(claudeHeaderRequest(ccSession, ccSession, ""))
			cxParent, errCX := h.send(codexHeaderRequest(cxSession, cxSession, "", false))
			if errCC != nil || errCX != nil {
				t.Errorf("parent %d: claude=%v codex=%v", i, errCC, errCX)
				return
			}
			for s := 0; s < subagents; s++ {
				for turn := 0; turn < 3; turn++ {
					label := fmt.Sprintf("%s-agent-%d", ccSession, s)
					if got, err := h.send(claudeHeaderRequest(label, ccSession, fmt.Sprintf("agent-%d", s))); err != nil || got != ccParent {
						t.Errorf("claude subagent %s turn %d: auth=%s err=%v, want parent auth %s", label, turn, got, err, ccParent)
					}
					label = fmt.Sprintf("%s-thread-%d", cxSession, s)
					if got, err := h.send(codexHeaderRequest(label, cxSession, fmt.Sprintf("%s-thread-%d", cxSession, s), true)); err != nil || got != cxParent {
						t.Errorf("codex subagent %s turn %d: auth=%s err=%v, want parent auth %s", label, turn, got, err, cxParent)
					}
				}
			}
			// The parent must still be on its credential after its subagents ran.
			if got, _ := h.send(claudeHeaderRequest(ccSession, ccSession, "")); got != ccParent {
				t.Errorf("claude parent %s moved %s -> %s after subagents", ccSession, ccParent, got)
			}
			if got, _ := h.send(codexHeaderRequest(cxSession, cxSession, "", false)); got != cxParent {
				t.Errorf("codex parent %s moved %s -> %s after subagents", cxSession, cxParent, got)
			}
		}(i)
	}
	wg.Wait()
}

// Disabling a bound credential (for example after its auth file is removed) must
// rebind its sessions once and keep them on the replacement.
func TestBattleAffinityDisabledCredentialRebindsOnce(t *testing.T) {
	h := newBattleHarness(t, 4)
	r := codexHeaderRequest("disable", "disable-session", "", false)
	first := h.mustSend(r)
	auth, ok := h.manager.GetByID(first)
	if !ok {
		t.Fatalf("credential %s not found", first)
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	if _, err := h.manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	second := h.mustSend(r)
	if second == first {
		t.Fatalf("session still served by disabled credential %s", first)
	}
	for turn := 0; turn < 5; turn++ {
		if got := h.mustSend(r); got != second {
			t.Fatalf("turn %d moved %s -> %s after rebinding", turn, second, got)
		}
	}
}

// The same client session ID used against two providers must keep independent,
// stable bindings in each provider pool.
func TestBattleAffinityProvidersAreIndependent(t *testing.T) {
	h := newBattleHarness(t, 3)
	const session = "shared-session-id"
	claude := h.mustSend(claudeHeaderRequest("shared-claude", session, ""))
	codex := h.mustSend(codexHeaderRequest("shared-codex", session, "", false))
	for turn := 0; turn < 5; turn++ {
		if got := h.mustSend(claudeHeaderRequest("shared-claude", session, "")); got != claude {
			t.Fatalf("claude binding moved %s -> %s", claude, got)
		}
		if got := h.mustSend(codexHeaderRequest("shared-codex", session, "", false)); got != codex {
			t.Fatalf("codex binding moved %s -> %s", codex, got)
		}
	}
}
