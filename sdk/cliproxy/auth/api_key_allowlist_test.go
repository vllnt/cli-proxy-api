package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const allowlistTestModel = "allowlist-model"

// allowlistFixture owns one provider with two allowed credentials (a1 by id, a2 by tag) and
// three disallowed ones (d1, d2 and d3, whose tag only resembles the allowed one).
type allowlistFixture struct {
	manager  *Manager
	executor *allowlistRecordingExecutor
	provider string
	prefix   string
}

func (f *allowlistFixture) id(name string) string { return f.prefix + name }

func (f *allowlistFixture) allowed() map[string]struct{} {
	return map[string]struct{}{f.id("a1"): {}, f.id("a2"): {}}
}

func (f *allowlistFixture) opts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{f.id("a1")}, []string{"monitor-pool"}),
	}}
}

// allowlistRecordingExecutor records the auth.ID of every upstream invocation.
type allowlistRecordingExecutor struct {
	provider string
	failWith error
	mu       sync.Mutex
	calls    []string
}

func (e *allowlistRecordingExecutor) Identifier() string { return e.provider }

func (e *allowlistRecordingExecutor) record(auth *Auth) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()
}

func (e *allowlistRecordingExecutor) invoked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *allowlistRecordingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth)
	return cliproxyexecutor.Response{Payload: []byte("ok")}, e.failWith
}

func (e *allowlistRecordingExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(auth)
	if e.failWith != nil {
		return nil, e.failWith
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *allowlistRecordingExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth)
	return cliproxyexecutor.Response{Payload: []byte("ok")}, e.failWith
}

func (*allowlistRecordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*allowlistRecordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newAllowlistFixture(t *testing.T, selector Selector, providerSuffix string, failWith error) *allowlistFixture {
	t.Helper()
	base := strings.ToLower(strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(t.Name())) + providerSuffix
	f := &allowlistFixture{
		manager:  NewManager(nil, selector, nil),
		provider: "alp-" + base,
		prefix:   "alid-" + base + "-",
	}
	f.manager.SetRetryConfig(2, 0, 0)
	f.executor = &allowlistRecordingExecutor{provider: f.provider, failWith: failWith}
	f.manager.RegisterExecutor(f.executor)
	f.register(t, f.provider, f.prefix)
	return f
}

// register adds the five credentials of one provider to the manager.
func (f *allowlistFixture) register(t *testing.T, provider, prefix string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	auths := []*Auth{
		{ID: prefix + "a1"},
		{ID: prefix + "a2", Attributes: map[string]string{"tags": "other, monitor-pool"}},
		{ID: prefix + "d1"},
		{ID: prefix + "d2", Metadata: map[string]any{"tags": []string{"agents-pool"}}},
		{ID: prefix + "d3", Attributes: map[string]string{"tags": "monitor-pool-extra"}},
	}
	for _, auth := range auths {
		auth.Provider = provider
		reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: allowlistTestModel}})
		id := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := f.manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
}

func (f *allowlistFixture) cool(t *testing.T, ids ...string) {
	t.Helper()
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	retryAfter := 10 * time.Minute
	for _, id := range ids {
		f.manager.MarkResult(context.Background(), Result{
			AuthID: id, Provider: f.provider, Model: allowlistTestModel,
			Success: false, RetryAfter: &retryAfter, CredentialScope: true,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
		})
	}
}

func requireAllowlistUnavailable(t *testing.T, err error) {
	t.Helper()
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != ErrorCodeAPIKeyAllowlistUnavailable || authErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("error = %#v, want %s with HTTP 503", err, ErrorCodeAPIKeyAllowlistUnavailable)
	}
	if !IsAPIKeyAllowlistUnavailable(err) {
		t.Fatalf("IsAPIKeyAllowlistUnavailable(%v) = false", err)
	}
	for _, leak := range []string{"alid-", "monitor", "a1", "d1"} {
		if strings.Contains(authErr.Message, leak) {
			t.Fatalf("client-visible message %q leaks %q", authErr.Message, leak)
		}
	}
}

func TestAuthHasTag(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		tag  string
		want bool
	}{
		{"attributes comma string", &Auth{Attributes: map[string]string{"tags": "a, monitor-pool ,b"}}, "monitor-pool", true},
		{"attributes single", &Auth{Attributes: map[string]string{"tags": "monitor-pool"}}, "monitor-pool", true},
		{"metadata string slice", &Auth{Metadata: map[string]any{"tags": []string{"x", "monitor-pool"}}}, "monitor-pool", true},
		{"metadata any slice", &Auth{Metadata: map[string]any{"tags": []any{"x", " monitor-pool "}}}, "monitor-pool", true},
		{"metadata comma string", &Auth{Metadata: map[string]any{"tags": "x,monitor-pool"}}, "monitor-pool", true},
		{"case sensitive", &Auth{Attributes: map[string]string{"tags": "Monitor-Pool"}}, "monitor-pool", false},
		{"substring is not a match", &Auth{Attributes: map[string]string{"tags": "monitor-pool-extra"}}, "monitor-pool", false},
		{"empty tag never matches", &Auth{Attributes: map[string]string{"tags": "a,,b"}}, "", false},
		{"blank tag never matches", &Auth{Attributes: map[string]string{"tags": "a, ,b"}}, "  ", false},
		{"no tags", &Auth{}, "monitor-pool", false},
		{"non string metadata", &Auth{Metadata: map[string]any{"tags": 7}}, "monitor-pool", false},
		{"nil auth", nil, "monitor-pool", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authHasTag(tt.auth, tt.tag); got != tt.want {
				t.Fatalf("authHasTag() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthSelectionEligibilityAPIKeyAllowlist(t *testing.T) {
	byID := cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{"a.json"}, nil)
	byTag := cliproxyexecutor.NewAPIKeyAuthAllowlist(nil, []string{"pool"})
	union := cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{"a.json"}, []string{"pool"})
	empty := cliproxyexecutor.NewAPIKeyAuthAllowlist(nil, nil)
	blank := cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{" ", ""}, []string{" "})

	tagged := &Auth{ID: "b.json", Provider: "codex", Attributes: map[string]string{"tags": "pool"}}
	plain := &Auth{ID: "a.json", Provider: "codex"}
	other := &Auth{ID: "c.json", Provider: "codex"}
	freeTagged := &Auth{ID: "free.json", Provider: "codex", Attributes: map[string]string{"tags": "pool", "plan_type": "free"}}

	tests := []struct {
		name string
		e    authSelectionEligibility
		auth *Auth
		want bool
	}{
		{"unset allows all", authSelectionEligibility{}, other, true},
		{"id match", authSelectionEligibility{allowlist: byID}, plain, true},
		{"id miss", authSelectionEligibility{allowlist: byID}, other, false},
		{"id entry ignores tags", authSelectionEligibility{allowlist: byID}, tagged, false},
		{"tag match", authSelectionEligibility{allowlist: byTag}, tagged, true},
		{"tag miss", authSelectionEligibility{allowlist: byTag}, other, false},
		{"union by id", authSelectionEligibility{allowlist: union}, plain, true},
		{"union by tag", authSelectionEligibility{allowlist: union}, tagged, true},
		{"union miss", authSelectionEligibility{allowlist: union}, other, false},
		{"empty entry denies all", authSelectionEligibility{allowlist: empty}, plain, false},
		{"empty entry denies tagged", authSelectionEligibility{allowlist: empty}, tagged, false},
		{"blank entries deny all", authSelectionEligibility{allowlist: blank}, plain, false},
		{"nil auth", authSelectionEligibility{allowlist: union}, nil, false},
		{"free-auth policy still applies", authSelectionEligibility{allowlist: union, disallowFreeAuth: true}, freeTagged, false},
		{"free-auth policy allows others", authSelectionEligibility{allowlist: union, disallowFreeAuth: true}, tagged, true},
		{"required kind still applies", authSelectionEligibility{allowlist: union, requiredKind: AuthKindAPIKey}, tagged, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.allows(tt.auth); got != tt.want {
				t.Fatalf("allows() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAPIKeyAuthAllowlistFromMetadataFailsClosed(t *testing.T) {
	if cliproxyexecutor.APIKeyAuthAllowlistFromMetadata(nil) != nil {
		t.Fatal("nil metadata must be unrestricted")
	}
	if cliproxyexecutor.APIKeyAuthAllowlistFromMetadata(map[string]any{"other": 1}) != nil {
		t.Fatal("metadata without the key must be unrestricted")
	}
	e := authSelectionEligibilityForRequest(context.Background(), cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: "not-an-allowlist",
	}})
	if e.allowlist == nil || e.allows(&Auth{ID: "anything"}) {
		t.Fatal("malformed restriction metadata must deny everything")
	}
}

// TestAPIKeyAuthAllowlistRegressionUnset proves a request without the restriction key keeps
// the previous behaviour: no eligibility restriction and every credential is used.
func TestAPIKeyAuthAllowlistRegressionUnset(t *testing.T) {
	if e := authSelectionEligibilityForRequest(context.Background(), cliproxyexecutor.Options{}); e.allowlist != nil {
		t.Fatalf("allowlist = %v, want nil", e.allowlist)
	}
	f := newAllowlistFixture(t, &RoundRobinSelector{}, "", nil)
	seen := map[string]int{}
	for range 25 {
		picked, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext: %v", err)
		}
		seen[picked.ID]++
	}
	if len(seen) != 5 {
		t.Fatalf("unrestricted round-robin used %v, want all 5 credentials", seen)
	}
}

func TestAPIKeyAuthAllowlistPicksOnlyAllowedCredentials(t *testing.T) {
	selectors := map[string]func() Selector{
		"round-robin (scheduler)": func() Selector { return &RoundRobinSelector{} },
		"fill-first (scheduler)":  func() Selector { return &FillFirstSelector{} },
		"weighted (scheduler)":    func() Selector { return &WeightedRoundRobinSelector{} },
		"headroom (legacy)":       func() Selector { return &HeadroomSelector{} },
		"reset-aware (legacy)":    func() Selector { return &ResetAwareSelector{} },
		"session-affinity (legacy)": func() Selector {
			return NewSessionAffinitySelector(&RoundRobinSelector{})
		},
	}
	names := make([]string, 0, len(selectors))
	for name := range selectors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selectors[name](), "", nil)
			allowed := f.allowed()
			seen := map[string]int{}
			for range 60 {
				picked, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, f.opts(), nil)
				if err != nil {
					t.Fatalf("pickNext: %v", err)
				}
				if _, ok := allowed[picked.ID]; !ok {
					t.Fatalf("picked disallowed credential %s", picked.ID)
				}
				seen[picked.ID]++
			}
			if strings.HasPrefix(name, "round-robin") && len(seen) != 2 {
				t.Fatalf("round-robin must rotate across the whole allowed set, got %v", seen)
			}
		})
	}
}

func TestAPIKeyAuthAllowlistExhaustedAllowedSetNeverFallsThrough(t *testing.T) {
	selectors := map[string]Selector{
		"scheduler": &RoundRobinSelector{},
		"legacy":    &HeadroomSelector{},
	}
	for name, selector := range selectors {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selector, "", nil)
			f.cool(t, f.id("a1"), f.id("a2"))
			// The unrestricted caller still gets a credential, so a fall-through would be visible.
			if _, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, cliproxyexecutor.Options{}, nil); err != nil {
				t.Fatalf("unrestricted pick must still succeed on disallowed credentials: %v", err)
			}
			_, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, f.opts(), nil)
			requireAllowlistUnavailable(t, err)
		})
	}
}

func TestAPIKeyAuthAllowlistEmptyEntryDeniesAll(t *testing.T) {
	for name, selector := range map[string]Selector{"scheduler": &RoundRobinSelector{}, "legacy": &ResetAwareSelector{}} {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selector, "", nil)
			opts := cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist(nil, nil),
			}}
			_, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, opts, nil)
			requireAllowlistUnavailable(t, err)
		})
	}
}

func TestAPIKeyAuthAllowlistPinnedOutsideListFailsClosed(t *testing.T) {
	for name, selector := range map[string]Selector{"scheduler": &RoundRobinSelector{}, "legacy": &HeadroomSelector{}} {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selector, "", nil)
			opts := f.opts()
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = f.id("d1")
			_, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, opts, nil)
			requireAllowlistUnavailable(t, err)

			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = f.id("a2")
			picked, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, opts, nil)
			if err != nil || picked.ID != f.id("a2") {
				t.Fatalf("pinned allowed credential: picked=%v err=%v", picked, err)
			}
		})
	}
}

func TestAPIKeyAuthAllowlistTriedSetNeverLeavesAllowedSet(t *testing.T) {
	for name, selector := range map[string]Selector{"scheduler": &RoundRobinSelector{}, "legacy": &HeadroomSelector{}} {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selector, "", nil)
			tried := map[string]struct{}{}
			for range 2 {
				picked, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, f.opts(), tried)
				if err != nil {
					t.Fatalf("pickNext: %v", err)
				}
				if _, ok := f.allowed()[picked.ID]; !ok {
					t.Fatalf("picked disallowed credential %s", picked.ID)
				}
				tried[picked.ID] = struct{}{}
			}
			_, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, f.opts(), tried)
			requireAllowlistUnavailable(t, err)
		})
	}
}

func TestAPIKeyAuthAllowlistMixedProviders(t *testing.T) {
	for name, selector := range map[string]Selector{"scheduler": &RoundRobinSelector{}, "legacy": &HeadroomSelector{}} {
		t.Run(name, func(t *testing.T) {
			f := newAllowlistFixture(t, selector, "", nil)
			secondProvider := f.provider + "-two"
			secondPrefix := f.prefix + "two-"
			f.manager.RegisterExecutor(&allowlistRecordingExecutor{provider: secondProvider})
			f.register(t, secondProvider, secondPrefix)

			opts := cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{f.id("a1"), secondPrefix + "a1"}, nil),
			}}
			providers := []string{f.provider, secondProvider}
			seen := map[string]int{}
			for range 40 {
				picked, _, _, err := f.manager.pickNextMixed(context.Background(), providers, allowlistTestModel, opts, nil)
				if err != nil {
					t.Fatalf("pickNextMixed: %v", err)
				}
				if picked.ID != f.id("a1") && picked.ID != secondPrefix+"a1" {
					t.Fatalf("mixed pick left the allowed set: %s", picked.ID)
				}
				seen[picked.ID]++
			}
			if len(seen) != 2 {
				t.Fatalf("mixed round-robin should use both allowed credentials, got %v", seen)
			}

			f.cool(t, f.id("a1"), secondPrefix+"a1")
			_, _, _, err := f.manager.pickNextMixed(context.Background(), providers, allowlistTestModel, opts, nil)
			requireAllowlistUnavailable(t, err)
		})
	}
}

// A session bound to a now-disallowed credential must not pull the restricted request back to it.
func TestAPIKeyAuthAllowlistSessionAffinityIgnoresDisallowedBinding(t *testing.T) {
	f := newAllowlistFixture(t, NewSessionAffinitySelector(&FillFirstSelector{}), "", nil)
	headers := http.Header{"X-Session-Id": []string{"sess-allowlist-1"}}

	// An unrestricted request binds the session to the first credential (a disallowed one is fine to bind).
	bound, _, err := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, cliproxyexecutor.Options{Headers: headers}, nil)
	if err != nil {
		t.Fatalf("unrestricted pick: %v", err)
	}
	// Restrict to a credential other than the bound one.
	other := f.id("a1")
	if bound.ID == other {
		other = f.id("a2")
	}
	opts := cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{
		cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{other}, nil),
	}}
	for range 5 {
		picked, _, errPick := f.manager.pickNext(context.Background(), f.provider, allowlistTestModel, opts, nil)
		if errPick != nil || picked.ID != other {
			t.Fatalf("restricted session pick = %v, %v; want %s", picked, errPick, other)
		}
	}
}

func TestAPIKeyAuthAllowlistExecutePathsNeverInvokeDisallowedCredential(t *testing.T) {
	type invoker func(*allowlistFixture) error
	paths := map[string]invoker{
		"Execute": func(f *allowlistFixture) error {
			_, err := f.manager.Execute(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: allowlistTestModel}, f.opts())
			return err
		},
		"ExecuteCount": func(f *allowlistFixture) error {
			_, err := f.manager.ExecuteCount(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: allowlistTestModel}, f.opts())
			return err
		},
		"ExecuteStream": func(f *allowlistFixture) error {
			result, err := f.manager.ExecuteStream(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: allowlistTestModel}, f.opts())
			if err == nil && result != nil {
				for range result.Chunks {
				}
			}
			return err
		},
	}
	selectors := map[string]func() Selector{
		"scheduler": func() Selector { return &RoundRobinSelector{} },
		"legacy":    func() Selector { return &HeadroomSelector{} },
	}
	for pathName, invoke := range paths {
		for selName, newSelector := range selectors {
			t.Run(pathName+"/"+selName+"/success", func(t *testing.T) {
				f := newAllowlistFixture(t, newSelector(), "", nil)
				for range 12 {
					if err := invoke(f); err != nil {
						t.Fatalf("%s: %v", pathName, err)
					}
				}
				requireInvokedWithin(t, f, f.executor.invoked(), true)
			})
			t.Run(pathName+"/"+selName+"/upstream failure retries stay inside", func(t *testing.T) {
				f := newAllowlistFixture(t, newSelector(), "", &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failure"})
				_ = invoke(f)
				requireInvokedWithin(t, f, f.executor.invoked(), false)
			})
			t.Run(pathName+"/"+selName+"/allowed set cooling", func(t *testing.T) {
				f := newAllowlistFixture(t, newSelector(), "", nil)
				f.cool(t, f.id("a1"), f.id("a2"))
				requireAllowlistUnavailable(t, invoke(f))
				if calls := f.executor.invoked(); len(calls) != 0 {
					t.Fatalf("no credential may be invoked while the allowed set is cooling, got %v", calls)
				}
			})
			t.Run(pathName+"/"+selName+"/unrestricted still reaches every credential", func(t *testing.T) {
				f := newAllowlistFixture(t, newSelector(), "", nil)
				unrestricted := f.opts()
				unrestricted.Metadata = nil
				seen := map[string]struct{}{}
				for range 30 {
					_, err := f.manager.Execute(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: allowlistTestModel}, unrestricted)
					if err != nil {
						t.Fatalf("Execute: %v", err)
					}
				}
				for _, id := range f.executor.invoked() {
					seen[id] = struct{}{}
				}
				if len(seen) < 3 {
					t.Fatalf("unrestricted traffic should spread over all credentials, got %v", seen)
				}
			})
		}
	}
}

func requireInvokedWithin(t *testing.T, f *allowlistFixture, calls []string, wantAny bool) {
	t.Helper()
	if wantAny && len(calls) == 0 {
		t.Fatal("executor was never invoked")
	}
	for _, id := range calls {
		if _, ok := f.allowed()[id]; !ok {
			t.Fatalf("disallowed credential %s was invoked (calls=%v)", id, calls)
		}
	}
}

func TestAPIKeyAuthAllowlistAntigravityCreditsFallbackStaysInsideAllowlist(t *testing.T) {
	m := &Manager{
		auths: map[string]*Auth{
			"ag-allowed":  {ID: "ag-allowed", Provider: "antigravity"},
			"ag-tagged":   {ID: "ag-tagged", Provider: "antigravity", Attributes: map[string]string{"tags": "monitor-pool"}},
			"ag-disallow": {ID: "ag-disallow", Provider: "antigravity"},
		},
		executors: map[string]ProviderExecutor{"antigravity": schedulerTestExecutor{}},
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{"ag-allowed"}, []string{"monitor-pool"}),
	}}
	candidates, err := m.findAllAntigravityCreditsCandidateAuths(context.Background(), "claude-sonnet-4-6", opts)
	if err != nil {
		t.Fatalf("findAllAntigravityCreditsCandidateAuths() error = %v", err)
	}
	got := map[string]bool{}
	for _, candidate := range candidates {
		got[candidate.auth.ID] = true
	}
	if len(got) != 2 || !got["ag-allowed"] || !got["ag-tagged"] || got["ag-disallow"] {
		t.Fatalf("credits candidates = %v, want only ag-allowed and ag-tagged", got)
	}

	all, err := m.findAllAntigravityCreditsCandidateAuths(context.Background(), "claude-sonnet-4-6", cliproxyexecutor.Options{})
	if err != nil || len(all) != 3 {
		t.Fatalf("unrestricted credits candidates = %d (err %v), want 3", len(all), err)
	}
}

// Home picks credentials remotely, so a restricted request must fail closed instead of being unrestricted.
func TestAPIKeyAuthAllowlistHomeDispatchFailsClosed(t *testing.T) {
	m := NewManager(nil, nil, nil)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.APIKeyAuthAllowlistMetadataKey: cliproxyexecutor.NewAPIKeyAuthAllowlist([]string{"a"}, nil),
	}}
	selection, err := m.pickHomeDispatchSelection(context.Background(), allowlistTestModel, opts)
	if selection != nil {
		t.Fatalf("selection = %v, want nil", selection)
	}
	requireAllowlistUnavailable(t, err)
}
