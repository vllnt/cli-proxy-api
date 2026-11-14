package musecode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A client key restricted by api-key-auth-allowlist must only reach the credentials it lists,
// even through the Muse Code relay, which selects Meta credentials itself.
func TestRelayHonorsClientKeyCredentialAllowlist(t *testing.T) {
	var deniedCalls, allowedCalls atomic.Int32
	deniedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deniedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer deniedUpstream.Close()
	allowedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer allowedUpstream.Close()

	cfg := &config.Config{}
	manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(executor.NewMetaExecutor(cfg))
	// Fill-first prefers the higher priority credential, which the key may not use.
	registerMetaTestCred(t, manager, &auth.Auth{
		ID: "meta-denied", Provider: "meta", Status: auth.StatusActive,
		Attributes: map[string]string{"api_key": "denied-key", "base_url": deniedUpstream.URL + "/v1", "priority": "10"},
	}, "muse-wonder-1.0")
	registerMetaTestCred(t, manager, &auth.Auth{
		ID: "meta-allowed", Provider: "meta", Status: auth.StatusActive,
		Attributes: map[string]string{"api_key": "allowed-key", "base_url": allowedUpstream.URL + "/v1", "priority": "1"},
	}, "muse-wonder-1.0")

	handler := NewHandler(manager, cfg)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(sdkaccess.CredentialAllowlistContextKey, &sdkaccess.CredentialAllowlist{AuthIDs: []string{"meta-allowed"}})
		c.Next()
	})
	engine.Any("/muse-code/*path", handler.Handle)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/muse-code/search", strings.NewReader(`{"q":"go"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if deniedCalls.Load() != 0 || allowedCalls.Load() != 1 {
		t.Fatalf("calls denied=%d allowed=%d, want 0 and 1", deniedCalls.Load(), allowedCalls.Load())
	}
}
