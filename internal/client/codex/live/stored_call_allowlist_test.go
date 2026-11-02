package live

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A stored Realtime call is pinned to the credential that created it, and every later request on the
// call (hangup, refer, accept, reject) is dispatched through the same selection as a new call. Reusing
// the call must therefore stay inside the allowlist of the principal making the request: a pin never
// widens it, and an unreachable pin fails closed instead of falling back to another credential.
func TestStoredCallReuseStaysInsideTheCallersAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		pinned   = "codex-oauth"
		spare    = "codex-spare"
		callID   = "call-123"
		unlimit  = "test-key-agents"
		limited  = "test-key-monitor"
		upstream = "https://api.openai.com/v1/realtime/calls/call-123/hangup"
	)
	// The configuration lookup for the principal, when nothing was bound at authentication.
	configList := func(ids ...string) map[string]config.APIKeyAuthAllowlist {
		return map[string]config.APIKeyAuthAllowlist{"monitor": {AuthIDs: ids}}
	}
	// The restriction bound at authentication (a config key, a plugin provider, a client secret).
	bound := func(ids ...string) *sdkaccess.CredentialAllowlist {
		return &sdkaccess.CredentialAllowlist{AuthIDs: ids}
	}

	tests := []struct {
		name        string
		principal   string
		configList  map[string]config.APIKeyAuthAllowlist
		bound       any
		wantStatus  int
		wantUpcalls int32
	}{
		{"unrestricted principal keeps the pinned credential", unlimit, configList(spare), nil, http.StatusOK, 1},
		{"configured list contains the pinned credential", limited, configList(pinned), nil, http.StatusOK, 1},
		{"configured list changed after the call was stored", limited, configList(spare), nil, http.StatusServiceUnavailable, 0},
		{"bound list contains the pinned credential", "plugin-principal", nil, bound(pinned), http.StatusOK, 1},
		{"bound list excludes the pinned credential", "plugin-principal", nil, bound(spare), http.StatusServiceUnavailable, 0},
		{"bound list wins over a configured list that would allow it", limited, configList(pinned), bound(spare), http.StatusServiceUnavailable, 0},
		{"bound list wins over a configured list that would deny it", limited, configList(spare), bound(pinned), http.StatusOK, 1},
		{"empty bound list denies everything", "plugin-principal", nil, bound(), http.StatusServiceUnavailable, 0},
		{"malformed bound value denies everything", "plugin-principal", nil, "not-a-list", http.StatusServiceUnavailable, 0},
		{"typed nil bound value denies everything", "plugin-principal", nil, (*sdkaccess.CredentialAllowlist)(nil), http.StatusServiceUnavailable, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := auth.NewManager(nil, nil, nil)
			executor := &captureExecutor{
				statusCode:   http.StatusOK,
				responseBody: io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
			}
			manager.RegisterExecutor(executor)
			for _, id := range []string{pinned, spare} {
				registerCredential(t, manager, &auth.Auth{
					ID:       id,
					Provider: "codex",
					Status:   auth.StatusActive,
					Metadata: map[string]any{"access_token": "oauth-token"},
				})
			}
			cfg := &config.Config{}
			cfg.APIKeys = []string{unlimit, limited}
			cfg.APIKeyLabels = map[string]string{"monitor": limited}
			cfg.APIKeyAuthAllowlist = tt.configList
			handler := NewHandler(manager, cfg)
			handler.sessions.put(callID, liveSession{
				authID:         pinned,
				model:          defaultLiveModel,
				ownerPrincipal: tt.principal,
				ownerProvider:  "static",
			})

			router := gin.New()
			router.POST("/v1/realtime/calls/:call_id/hangup", func(c *gin.Context) {
				c.Set("userApiKey", tt.principal)
				c.Set("accessProvider", "static")
				if tt.bound != nil {
					c.Set(sdkaccess.CredentialAllowlistContextKey, tt.bound)
				}
				c.Next()
			}, handler.HandleHangup)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/realtime/calls/"+callID+"/hangup", nil))

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := executor.httpCalls.Load(); got != tt.wantUpcalls {
				t.Fatalf("upstream calls = %d, want %d", got, tt.wantUpcalls)
			}
			if tt.wantStatus == http.StatusOK {
				if executor.selectedAuth == nil || executor.selectedAuth.ID != pinned || executor.request.URL.String() != upstream {
					t.Fatalf("hangup went to %v %v, want %s on %s", executor.selectedAuth, executor.request, upstream, pinned)
				}
				return
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "no allowed account is currently available") {
				t.Fatalf("denial = %s, want the allowlist message", body)
			}
			for _, id := range []string{pinned, spare} {
				if strings.Contains(body, id) {
					t.Fatalf("denial names an account (%s): %s", id, body)
				}
			}
			if _, ok := handler.sessions.peek(callID); !ok {
				t.Fatal("a denied reuse must leave the stored call for its rightful holder")
			}
		})
	}
}
