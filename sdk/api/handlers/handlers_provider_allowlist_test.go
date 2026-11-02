package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// An access provider other than the configuration one (a plugin) admits a principal that the
// configuration knows nothing about and may return a CredentialAllowlist for it. That restriction,
// bound at authentication, must decide which credentials the handler dispatches to, for every request
// kind, and an allowed set that cannot serve must fail closed instead of reaching another credential.
func TestExecuteWithAuthManagerHonorsAProviderSuppliedAllowlist(t *testing.T) {
	const model = "handler-provider-allowlist-model"
	var mu sync.Mutex
	var invoked []string
	record := func(auth *coreauth.Auth) {
		mu.Lock()
		invoked = append(invoked, auth.ID)
		mu.Unlock()
	}
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		got := invoked
		invoked = nil
		return got
	}
	executor := &interceptorCaptureExecutor{
		provider: "handler-provider-allowlist",
		execute: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			record(auth)
			return coreexecutor.Response{Payload: []byte("ok")}, nil
		},
		executeCount: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			record(auth)
			return coreexecutor.Response{Payload: []byte("ok")}, nil
		},
		stream: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			record(auth)
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, auth := range []*coreauth.Auth{
		{ID: "pa-allowed.json", Provider: executor.provider},
		{ID: "pa-tagged.json", Provider: executor.provider, Attributes: map[string]string{"tags": "plugin-pool"}},
		{ID: "pa-other-1.json", Provider: executor.provider},
		{ID: "pa-other-2.json", Provider: executor.provider},
		{ID: "pa-disabled.json", Provider: executor.provider, Disabled: true},
	} {
		auth.Status = coreauth.StatusActive
		if auth.Disabled {
			auth.Status = coreauth.StatusDisabled
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		id := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	// The configuration labels and limits other keys: it knows nothing about the plugin's principal.
	handler := NewBaseAPIHandlers(allowlistTestConfig(), manager)
	body := []byte(`{"model":"` + model + `"}`)

	// A request as the access middleware leaves it: the plugin's principal, plus its bound restriction.
	asPlugin := func(list any) context.Context {
		ctx := ginContextForKey("plugin-principal", true)
		if list != nil {
			ctx.Value("gin").(*gin.Context).Set(sdkaccess.CredentialAllowlistContextKey, list)
		}
		return ctx
	}
	// Every request kind goes through the same selection.
	dispatch := func(ctx context.Context) []*interfaces.ErrorMessage {
		var failures []*interfaces.ErrorMessage
		if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, body, ""); errMsg != nil {
			failures = append(failures, errMsg)
		}
		if _, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", model, body, ""); errMsg != nil {
			failures = append(failures, errMsg)
		}
		data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, "openai", model, body, "")
		if data != nil { // nil when the call failed before any stream started
			for range data {
			}
		}
		if errMsg := <-errs; errMsg != nil {
			failures = append(failures, errMsg)
		}
		return failures
	}
	requireOnly := func(t *testing.T, name string, ctx context.Context, allowed ...string) {
		t.Helper()
		ok := map[string]bool{}
		for _, id := range allowed {
			ok[id] = true
		}
		for i := 0; i < 8; i++ {
			if failures := dispatch(ctx); len(failures) != 0 {
				t.Fatalf("%s: request failed: %v", name, failures[0].Error)
			}
		}
		got := take()
		if len(got) != 24 {
			t.Fatalf("%s: invocations = %d, want 24 (%v)", name, len(got), got)
		}
		for _, id := range got {
			if !ok[id] {
				t.Fatalf("%s: reached %s outside the provider's list %v", name, id, allowed)
			}
		}
	}
	requireDenied := func(t *testing.T, name string, ctx context.Context) {
		t.Helper()
		failures := dispatch(ctx)
		if len(failures) != 3 {
			t.Fatalf("%s: %d of 3 request kinds failed, want all", name, len(failures))
		}
		for _, errMsg := range failures {
			var out struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(BuildErrorResponseBodyWithError(errMsg.StatusCode, errMsg.Error.Error(), errMsg.Error), &out); err != nil {
				t.Fatalf("%s: error body is not JSON: %v", name, err)
			}
			if errMsg.StatusCode != http.StatusServiceUnavailable || out.Error.Code != "api_key_allowlist_unavailable" {
				t.Fatalf("%s: got %d %q, want 503 api_key_allowlist_unavailable", name, errMsg.StatusCode, out.Error.Code)
			}
		}
		if got := take(); len(got) != 0 {
			t.Fatalf("%s: a credential was invoked although none is allowed: %v", name, got)
		}
	}

	// Control: without a bound restriction the same principal is unrestricted and rotates over every
	// enabled credential, so the cases below prove the bound list is what restricts.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		if failures := dispatch(asPlugin(nil)); len(failures) != 0 {
			t.Fatalf("control: %v", failures[0].Error)
		}
	}
	for _, id := range take() {
		seen[id] = true
	}
	if len(seen) != 4 {
		t.Fatalf("control: the unrestricted principal should rotate over the 4 enabled credentials, got %v", seen)
	}

	requireOnly(t, "ids", asPlugin(&sdkaccess.CredentialAllowlist{AuthIDs: []string{"pa-allowed.json"}}), "pa-allowed.json")
	requireOnly(t, "tags", asPlugin(&sdkaccess.CredentialAllowlist{Tags: []string{"plugin-pool"}}), "pa-tagged.json")
	requireOnly(t, "ids and tags", asPlugin(&sdkaccess.CredentialAllowlist{AuthIDs: []string{"pa-allowed.json"}, Tags: []string{"plugin-pool"}}), "pa-allowed.json", "pa-tagged.json")

	// Healthy credentials exist outside the list; none of them may serve the request.
	requireDenied(t, "allowed credential disabled", asPlugin(&sdkaccess.CredentialAllowlist{AuthIDs: []string{"pa-disabled.json"}}))
	requireDenied(t, "allowed credential does not exist", asPlugin(&sdkaccess.CredentialAllowlist{AuthIDs: []string{"pa-gone.json"}}))
	requireDenied(t, "empty list", asPlugin(&sdkaccess.CredentialAllowlist{}))
	requireDenied(t, "malformed bound value", asPlugin("plugin-pool"))
	requireDenied(t, "typed nil bound value", asPlugin((*sdkaccess.CredentialAllowlist)(nil)))
}
