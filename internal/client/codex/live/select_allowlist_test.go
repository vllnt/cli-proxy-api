package live

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Live selection picks its credential directly, so it must honor the client API key allowlist
// and must not widen a restriction the caller already set.
func TestSelectOAuthHonorsAPIKeyAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{statusCode: http.StatusOK})
	for _, id := range []string{"live-allowed", "live-other-1", "live-other-2"} {
		registerCredential(t, manager, &auth.Auth{
			ID:       id,
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "oauth-token"},
		})
	}
	cfg := &config.Config{}
	cfg.APIKeys = []string{"test-key-monitor", "test-key-agents"}
	cfg.APIKeyLabels = map[string]string{"monitor": "test-key-monitor"}
	cfg.APIKeyAuthAllowlist = map[string]config.APIKeyAuthAllowlist{"monitor": {AuthIDs: []string{"live-allowed"}}}
	handler := NewHandler(manager, cfg)

	ctxFor := func(apiKey string) context.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", nil)
		c.Set("userApiKey", apiKey)
		return context.WithValue(context.Background(), "gin", c)
	}

	for range 12 {
		_, selected, err := handler.selectOAuth(ctxFor("test-key-monitor"), "", coreexecutor.Options{})
		if err != nil || selected == nil || selected.ID != "live-allowed" {
			t.Fatalf("restricted key selection = %v, %v; want live-allowed", selected, err)
		}
	}

	seen := map[string]bool{}
	for range 12 {
		_, selected, err := handler.selectOAuth(ctxFor("test-key-agents"), "", coreexecutor.Options{})
		if err != nil {
			t.Fatalf("unrestricted selection: %v", err)
		}
		seen[selected.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("unrestricted key should keep using every credential, got %v", seen)
	}

	// An allowlist that matches nothing fails with the allowlist error, never another credential.
	cfg.APIKeyAuthAllowlist["monitor"] = config.APIKeyAuthAllowlist{AuthIDs: []string{"live-gone"}}
	_, selected, err := handler.selectOAuth(ctxFor("test-key-monitor"), "", coreexecutor.Options{})
	var authErr *auth.Error
	if selected != nil || !errors.As(err, &authErr) || authErr.Code != auth.ErrorCodeAPIKeyAllowlistUnavailable {
		t.Fatalf("selection = %v, %v; want %s", selected, err, auth.ErrorCodeAPIKeyAllowlistUnavailable)
	}
}
