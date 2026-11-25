package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type reconcileIntegrationTransport struct {
	t     *testing.T
	body  string
	calls int
}

func (p *reconcileIntegrationTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return p }
func (p *reconcileIntegrationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.calls++
	if r.Method != "GET" || r.URL.String() != "https://api.anthropic.com/api/oauth/usage" || r.Body != nil {
		p.t.Fatalf("unexpected upstream operation: %s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer synthetic-oauth-token" || r.Header.Get("X-Caller-Header") != "" {
		p.t.Fatal("provider request did not use only saved authorization")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(p.body))}, nil
}

type reconcileIntegrationExecutor struct{ ordinary, streamed []string }

func (*reconcileIntegrationExecutor) Identifier() string { return "claude" }
func (e *reconcileIntegrationExecutor) Execute(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
	e.ordinary = append(e.ordinary, a.ID)
	return executor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *reconcileIntegrationExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
	e.streamed = append(e.streamed, a.ID)
	chunks := make(chan executor.StreamChunk, 1)
	chunks <- executor.StreamChunk{Payload: []byte("data: ok\n\n")}
	close(chunks)
	return &executor.StreamResult{Chunks: chunks}, nil
}
func (*reconcileIntegrationExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (*reconcileIntegrationExecutor) CountTokens(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, errors.New("not used")
}
func (*reconcileIntegrationExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

type reconcileIntegrationStore struct {
	coreauth.CooldownStateStore
	fail bool
}

func (s *reconcileIntegrationStore) Save(ctx context.Context, records []coreauth.CooldownStateRecord) error {
	if s.fail {
		return errors.New("synthetic disk failure")
	}
	return s.CooldownStateStore.Save(ctx, records)
}

func TestManagementQuotaReconcileRoutingAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name          string
		partial       bool
		window, usage string
	}{
		{"durable success", false, "5h", `{"five_hour":{"utilization":0},"seven_day":{"utilization":42}}`},
		{"failed persistence then retry", true, "5h", `{"five_hour":{"utilization":0},"seven_day":{"utilization":42}}`},
		{"weekly restored short unknown", false, "7d", `{"five_hour":null,"seven_day":{"utilization":42}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial := tc.partial
			t.Setenv("MANAGEMENT_PASSWORD", "quota-management-test-key")
			ctx := context.Background()
			cfg := &config.Config{AuthDir: t.TempDir()}
			m := coreauth.NewManager(nil, &coreauth.HeadroomSelector{}, nil)
			m.SetConfig(cfg)
			store := &reconcileIntegrationStore{CooldownStateStore: coreauth.NewFileCooldownStateStoreWithAuthDir(cfg.AuthDir, cfg.AuthDir)}
			if !m.SwapCooldownStateStore(ctx, store, false) {
				t.Fatal("store setup failed")
			}
			id := "quota-route-" + strings.ReplaceAll(t.Name(), "/", "-")
			newAuth := func() *coreauth.Auth {
				return &coreauth.Auth{ID: id, FileName: id + ".json", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeRuntimeOnly: "true"}, Metadata: map[string]any{"access_token": "synthetic-oauth-token"}}
			}
			a, errRegister := m.Register(ctx, newAuth())
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-route"}, {ID: "claude-opus-route"}})
			t.Cleanup(func() { reg.UnregisterClient(id) })
			after := time.Hour
			m.MarkResult(ctx, coreauth.Result{AuthID: id, Provider: "claude", Model: "claude-opus-route", RetryAfter: &after, Error: &coreauth.Error{HTTPStatus: 503, Message: "independent model failure"}})
			rejectedCtx := internallogging.WithResponseHeadersHolder(ctx)
			internallogging.SetResponseHeaders(rejectedCtx, http.Header{"Anthropic-Ratelimit-Unified-" + tc.window + "-Status": []string{"rejected"}})
			after = 24 * time.Hour
			m.MarkResult(rejectedCtx, coreauth.Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-route", CredentialScope: true, RetryAfter: &after, Error: &coreauth.Error{HTTPStatus: 429, Message: "quota rejected"}})
			provider := &reconcileIntegrationTransport{t: t, body: tc.usage}
			m.SetRoundTripperProvider(provider)
			exec := &reconcileIntegrationExecutor{}
			m.RegisterExecutor(exec)
			dispatch := func(stream bool, model string) error {
				if !stream {
					_, errExecute := m.Execute(ctx, []string{"claude"}, executor.Request{Model: model}, executor.Options{})
					return errExecute
				}
				result, errExecute := m.ExecuteStream(ctx, []string{"claude"}, executor.Request{Model: model}, executor.Options{Stream: true})
				if result != nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
				}
				return errExecute
			}
			for _, stream := range []bool{false, true} {
				if errExecute := dispatch(stream, "claude-sonnet-route"); errExecute == nil {
					t.Fatal("locally exhausted credential dispatched before reconcile")
				}
			}
			if len(exec.ordinary)+len(exec.streamed) != 0 || provider.calls != 0 {
				t.Fatal("blocked request reached executor/provider")
			}
			h := management.NewHandlerWithoutConfigFilePath(cfg, m)
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(true)
			s.registerManagementRoutes()
			request := func(method, path, body, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.RemoteAddr = "127.0.0.1:1234"
				if key != "" {
					r.Header.Set("Authorization", "Bearer "+key)
				}
				r.Header.Set("X-Caller-Header", "untrusted")
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, r)
				return w
			}
			body := `{"auth_index":"` + a.Index + `"}`
			for _, key := range []string{"", "wrong-key"} {
				w := request("POST", "/v8/management/routing/quota/reconcile", body, key)
				if w.Code != 401 || provider.calls != 0 {
					t.Fatalf("unauthenticated request reached provider: status=%d calls=%d", w.Code, provider.calls)
				}
			}
			store.fail = partial
			w := request("POST", "/v8/management/routing/quota/reconcile", body, "quota-management-test-key")
			wantStatus := 200
			if partial {
				wantStatus = 503
			}
			if w.Code != wantStatus {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			response := ojson.ParseBytes(w.Body.Bytes())
			if response.Get("observed_at").String() == "" || response.Get("auth_index").String() != a.Index || strings.Contains(w.Body.String(), "synthetic-oauth-token") {
				t.Fatalf("unexpected result: %s", w.Body)
			}
			for _, stream := range []bool{false, true} {
				if errExecute := dispatch(stream, "claude-sonnet-route"); errExecute != nil {
					t.Fatalf("fresh authoritative usage still blocked dispatch stream=%v (ordinary executor calls=%d streamed=%d): %v", stream, len(exec.ordinary), len(exec.streamed), errExecute)
				}
			}
			if response.Get("outcome").String() != "reconciled" {
				t.Fatalf("unexpected reconcile outcome: %s", w.Body)
			}
			current, _ := m.GetByID(id)
			if current.Quota.Exceeded {
				t.Fatal("proven credential quota not cleared")
			}
			if state := current.ModelStates["claude-opus-route"]; state == nil || state.LastError == nil || state.LastError.HTTPStatus != 503 {
				t.Fatalf("independent model state lost: %+v", state)
			}
			if partial {
				oldRecords, errLoad := store.Load(ctx)
				if errLoad != nil {
					t.Fatal(errLoad)
				}
				oldStillBlocked := false
				for _, record := range oldRecords {
					if record.Quota.Reason == "credential_quota" {
						oldStillBlocked = true
					}
				}
				if !oldStillBlocked || response.Get("runtime_applied").Bool() != true || response.Get("persistence").String() != "failed" {
					t.Fatal("failed persistence was hidden")
				}
				negativeReload := coreauth.NewManager(nil, nil, nil)
				negativeReload.SetConfig(cfg)
				if _, errReload := negativeReload.Register(ctx, newAuth()); errReload != nil {
					t.Fatal(errReload)
				}
				if !negativeReload.SwapCooldownStateStore(ctx, store, false) {
					t.Fatal("negative reload setup")
				}
				if errRestore := negativeReload.RestoreCooldownStates(ctx); errRestore != nil {
					t.Fatal(errRestore)
				}
				negativeAuth, _ := negativeReload.GetByID(id)
				if !negativeAuth.Quota.Exceeded {
					t.Fatal("negative restart did not preserve failed durable write")
				}
				store.fail = false
				w = request("POST", "/v8/management/routing/quota/reconcile", body, "quota-management-test-key")
				if w.Code != 200 {
					t.Fatalf("persistence retry failed: %d %s", w.Code, w.Body)
				}
			}
			listed := request("GET", "/v8/management/credentials", "", "quota-management-test-key")
			list := ojson.ParseBytes(listed.Body.Bytes())
			if listed.Code != 200 || list.Get("snapshot_at").String() == "" {
				t.Fatalf("missing snapshot freshness: %s", listed.Body)
			}
			found := false
			list.Get("files").ForEach(func(_, entry ojson.Result) bool {
				if entry.Get("auth_index").String() == a.Index {
					found = true
					if entry.Get("quota.observed_at").String() == "" || entry.Get("quota.signals.Anthropic-Usage-Source").String() != "oauth_usage" || entry.Get("quota.signals.Anthropic-Ratelimit-Unified-5h-Status").String() != "" {
						t.Fatalf("stale list observation: %s", entry.Raw)
					}
				}
				return true
			})
			if !found {
				t.Fatal("restored credential missing from list")
			}
			for _, stream := range []bool{false, true} {
				if errExecute := dispatch(stream, "claude-opus-route"); errExecute == nil {
					t.Fatal("independent model failure became selectable")
				}
			}
			if len(exec.ordinary) != 1 || len(exec.streamed) != 1 || exec.ordinary[0] != id || exec.streamed[0] != id {
				t.Fatalf("dispatch attribution ordinary=%v stream=%v", exec.ordinary, exec.streamed)
			}
			reloaded := coreauth.NewManager(nil, &coreauth.HeadroomSelector{}, nil)
			reloaded.SetConfig(cfg)
			if _, errReload := reloaded.Register(ctx, newAuth()); errReload != nil {
				t.Fatal(errReload)
			}
			if !reloaded.SwapCooldownStateStore(ctx, store, false) {
				t.Fatal("reload store setup")
			}
			if errRestore := reloaded.RestoreCooldownStates(ctx); errRestore != nil {
				t.Fatal(errRestore)
			}
			reloaded.RegisterExecutor(exec)
			if _, errExecute := reloaded.Execute(ctx, []string{"claude"}, executor.Request{Model: "claude-sonnet-route"}, executor.Options{}); errExecute != nil {
				t.Fatalf("restart restored obsolete cooldown: %v", errExecute)
			}
			if _, errExecute := reloaded.Execute(ctx, []string{"claude"}, executor.Request{Model: "claude-opus-route"}, executor.Options{}); errExecute == nil {
				t.Fatal("restart erased independent model failure")
			}
		})
	}
}

func TestManagementQuotaReconcileRejectsCallerAuthority(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "quota-management-test-key")
	m := coreauth.NewManager(nil, nil, nil)
	a, errRegister := m.Register(context.Background(), &coreauth.Auth{ID: "quota-authority", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-oauth-token"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	p := &reconcileIntegrationTransport{t: t, body: `{"five_hour":{"utilization":0}}`}
	m.SetRoundTripperProvider(p)
	cfg := &config.Config{}
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandlerWithoutConfigFilePath(cfg, m)}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"auth_index":"` + a.Index + `","url":"https://example.invalid"}`, 400},
		{`{"auth_index":"` + a.Index + `","host":"example.invalid"}`, 400},
		{`{"auth_index":"` + a.Index + `","proxy":"http://example.invalid"}`, 400},
		{`{"auth_index":"` + a.Index + `","quota":{"utilization":0}}`, 400},
		{`{"auth_index":"` + a.Index + `","auth_index":"` + a.Index + `"}`, 400},
		{`{"auth_id":"quota-authority"}`, 400}, {`{"auth_index":"quota-authority"}`, 404},
		{`{"auth_index":123}`, 400}, {`null`, 400}, {`{"auth_index":"` + strings.Repeat("x", 4096) + `"}`, 400},
	} {
		r := httptest.NewRequest("POST", "/v8/management/routing/quota/reconcile", strings.NewReader(tc.body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer quota-management-test-key")
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		if w.Code != tc.status || p.calls != 0 {
			t.Fatalf("body=%s status=%d provider calls=%d", tc.body, w.Code, p.calls)
		}
	}
	for _, kind := range []string{"disabled", "api key", "other provider"} {
		t.Run(kind, func(t *testing.T) {
			updated, _ := m.GetByID(a.ID)
			updated.Disabled = kind == "disabled"
			updated.Provider = "claude"
			updated.Attributes = map[string]string{}
			if kind == "api key" {
				updated.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindAPIKey
			}
			if kind == "other provider" {
				updated.Provider = "codex"
			}
			if _, errUpdate := m.Update(context.Background(), updated); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			r := httptest.NewRequest("POST", "/v8/management/routing/quota/reconcile", strings.NewReader(`{"auth_index":"`+a.Index+`"}`))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Authorization", "Bearer quota-management-test-key")
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != 400 || p.calls != 0 {
				t.Fatalf("%s status=%d body=%s calls=%d", kind, w.Code, w.Body, p.calls)
			}
		})
	}
}
