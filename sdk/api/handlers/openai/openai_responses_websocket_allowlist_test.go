package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type allowlistWebsocketAccessProvider struct {
	allowlist *sdkaccess.CredentialAllowlist
}

func (p *allowlistWebsocketAccessProvider) Identifier() string { return "allowlist-test" }

func (p *allowlistWebsocketAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return &sdkaccess.Result{Principal: "key-1", Provider: p.Identifier(), CredentialAllowlist: p.allowlist}, nil
}

// A reload that narrows a key's credential allowlist must reach websocket turns that re-authenticate
// on an open socket; a turn whose key has no allowlist keeps the stricter one bound at connect.
func TestResponsesWebsocketTurnAuthRebindsCredentialAllowlist(t *testing.T) {
	provider := &allowlistWebsocketAccessProvider{allowlist: &sdkaccess.CredentialAllowlist{AuthIDs: []string{"narrowed.json"}}}
	accessManager := sdkaccess.NewManager()
	accessManager.SetProviders([]sdkaccess.Provider{provider})
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	h.AccessManager = accessManager

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set(sdkaccess.CredentialAllowlistContextKey, &sdkaccess.CredentialAllowlist{AuthIDs: []string{"wide-a.json", "narrowed.json"}})
	turn := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	boundIDs := func() []string {
		bound, _ := c.Get(sdkaccess.CredentialAllowlistContextKey)
		list, _ := bound.(*sdkaccess.CredentialAllowlist)
		if list == nil {
			return nil
		}
		return list.AuthIDs
	}

	if _, errAuth := h.authenticateResponsesWebsocketTurn(c, turn, []byte(`{"type":"response.create"}`), "gpt-5.5", true); errAuth != nil {
		t.Fatalf("turn auth: %v", errAuth.Error)
	}
	if got := boundIDs(); !slices.Equal(got, []string{"narrowed.json"}) {
		t.Fatalf("bound allowlist after narrowing reload = %v, want [narrowed.json]", got)
	}

	provider.allowlist = nil
	if _, errAuth := h.authenticateResponsesWebsocketTurn(c, turn, []byte(`{"type":"response.create"}`), "gpt-5.5", true); errAuth != nil {
		t.Fatalf("turn auth: %v", errAuth.Error)
	}
	if got := boundIDs(); !slices.Equal(got, []string{"narrowed.json"}) {
		t.Fatalf("bound allowlist after unrestricted turn = %v, want the stricter [narrowed.json] kept", got)
	}
}
