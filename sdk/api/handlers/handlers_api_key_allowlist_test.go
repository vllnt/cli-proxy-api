package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func allowlistTestConfig() *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{
		APIKeys:      []string{"test-key-agents", "test-key-monitor"},
		APIKeyLabels: map[string]string{"agents": "test-key-agents", "monitor": "test-key-monitor"},
		APIKeyAuthAllowlist: map[string]sdkconfig.APIKeyAuthAllowlist{
			"monitor": {AuthIDs: []string{"ak-allowed.json"}, Tags: []string{"monitor-pool"}},
		},
	}
}

// ginContextForKey returns a ctx carrying a gin context whose principal is apiKey ("" = no principal).
func ginContextForKey(apiKey string, withRequest bool) context.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if withRequest {
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	}
	if apiKey != "" {
		c.Set("userApiKey", apiKey)
	}
	return context.WithValue(context.Background(), "gin", c)
}

func TestRequestExecutionMetadataCarriesAPIKeyAllowlist(t *testing.T) {
	cfg := allowlistTestConfig()
	tagged := &coreauth.Auth{ID: "ak-tagged.json", Attributes: map[string]string{"tags": "monitor-pool"}}
	allowedByID := &coreauth.Auth{ID: "ak-allowed.json"}
	other := &coreauth.Auth{ID: "ak-other.json"}

	t.Run("restricted key", func(t *testing.T) {
		for name, withRequest := range map[string]bool{"with request": true, "gin context without request": false} {
			meta := requestExecutionMetadata(ginContextForKey("test-key-monitor", withRequest), cfg)
			restriction := coreexecutor.AllowedAuthsFromMetadata(meta)
			if restriction == nil {
				t.Fatalf("%s: restricted key lost its restriction; metadata=%v", name, meta)
			}
			hasTag := func(a *coreauth.Auth) func(string) bool {
				return func(tag string) bool { return a.Attributes["tags"] == tag }
			}
			if !restriction.Allows(allowedByID.ID, hasTag(allowedByID)) || !restriction.Allows(tagged.ID, hasTag(tagged)) || restriction.Allows(other.ID, hasTag(other)) {
				t.Fatalf("%s: restriction does not match the configured allowlist", name)
			}
		}
	})

	for name, tt := range map[string]struct {
		ctx context.Context
		cfg *sdkconfig.SDKConfig
	}{
		"label without allowlist entry": {ginContextForKey("test-key-agents", true), cfg},
		"unlabelled key":                {ginContextForKey("test-key-other", true), cfg},
		"no resolved key":               {ginContextForKey("", true), cfg},
		"no gin context":                {context.Background(), cfg},
		"nil config":                    {ginContextForKey("test-key-monitor", true), nil},
		"allowlist not configured":      {ginContextForKey("test-key-monitor", true), &sdkconfig.SDKConfig{APIKeys: cfg.APIKeys, APIKeyLabels: cfg.APIKeyLabels}},
	} {
		t.Run(name, func(t *testing.T) {
			meta := requestExecutionMetadata(tt.ctx, tt.cfg)
			if _, present := meta[coreexecutor.AllowedAuthsMetadataKey]; present {
				t.Fatalf("metadata must carry no restriction, got %v", meta)
			}
		})
	}

	t.Run("live config snapshot", func(t *testing.T) {
		ctx := ginContextForKey("test-key-monitor", true)
		if coreexecutor.AllowedAuthsFromMetadata(requestExecutionMetadata(ctx, cfg)) == nil {
			t.Fatal("expected a restriction under the first snapshot")
		}
		reloaded := allowlistTestConfig()
		reloaded.APIKeyAuthAllowlist = nil
		if coreexecutor.AllowedAuthsFromMetadata(requestExecutionMetadata(ctx, reloaded)) != nil {
			t.Fatal("removing the allowlist must lift the restriction on the next request")
		}
	})

	t.Run("AuthSelectionMetadata", func(t *testing.T) {
		if coreexecutor.AllowedAuthsFromMetadata(AuthSelectionMetadata(ginContextForKey("test-key-monitor", true), cfg)) == nil {
			t.Fatal("direct selection metadata must carry the restriction")
		}
		if got := AuthSelectionMetadata(ginContextForKey("test-key-agents", true), cfg); len(got) != 0 {
			t.Fatalf("unrestricted key must yield empty metadata, got %v", got)
		}
	})
}

// Without any allowlist configured the metadata is exactly what it was before this feature.
func TestRequestExecutionMetadataUnchangedWithoutAllowlist(t *testing.T) {
	ctx := ginContextForKey("test-key-monitor", true)
	before := requestExecutionMetadata(ctx, nil)
	after := requestExecutionMetadata(ctx, &sdkconfig.SDKConfig{APIKeys: []string{"test-key-monitor"}})
	if len(before) != len(after) {
		t.Fatalf("metadata changed without an allowlist: %v vs %v", before, after)
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			t.Fatalf("unexpected metadata key %q without an allowlist", key)
		}
	}
}

func TestBuildErrorResponseBodyAllowlistUnavailableCode(t *testing.T) {
	err := &coreauth.Error{Code: coreauth.ErrorCodeAPIKeyAllowlistUnavailable, Message: "no allowed account is currently available for this API key", HTTPStatus: http.StatusServiceUnavailable}
	body := string(BuildErrorResponseBodyWithError(http.StatusServiceUnavailable, err.Message, err))
	if !strings.Contains(body, `"code":"api_key_allowlist_unavailable"`) || !strings.Contains(body, err.Message) {
		t.Fatalf("body = %s", body)
	}
	plain := string(BuildErrorResponseBodyWithError(http.StatusServiceUnavailable, "down", nil))
	if !strings.Contains(plain, `"code":"internal_server_error"`) {
		t.Fatalf("other 5xx errors must keep their code, body = %s", plain)
	}
}

// End to end: a restricted client key reaches only its allowed credentials through the
// handler for non-stream, stream and count requests.
func TestExecuteWithAuthManagerHonorsAPIKeyAllowlist(t *testing.T) {
	model := "handler-api-key-allowlist-model"
	var mu sync.Mutex
	var invoked []string
	record := func(auth *coreauth.Auth) {
		mu.Lock()
		invoked = append(invoked, auth.ID)
		mu.Unlock()
	}
	executor := &interceptorCaptureExecutor{
		provider: "handler-api-key-allowlist",
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
		{ID: "ak-allowed.json", Provider: executor.provider},
		{ID: "ak-tagged.json", Provider: executor.provider, Attributes: map[string]string{"tags": "monitor-pool"}},
		{ID: "ak-other-1.json", Provider: executor.provider},
		{ID: "ak-other-2.json", Provider: executor.provider},
	} {
		auth.Status = coreauth.StatusActive
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		id := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	handler := NewBaseAPIHandlers(allowlistTestConfig(), manager)
	body := []byte(`{"model":"` + model + `"}`)
	allowed := map[string]bool{"ak-allowed.json": true, "ak-tagged.json": true}

	ctx := ginContextForKey("test-key-monitor", true)
	for i := 0; i < 10; i++ {
		if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, body, ""); errMsg != nil {
			t.Fatalf("ExecuteWithAuthManager: %v", errMsg.Error)
		}
		if _, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", model, body, ""); errMsg != nil {
			t.Fatalf("ExecuteCountWithAuthManager: %v", errMsg.Error)
		}
		data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, "openai", model, body, "")
		for range data {
		}
		if errMsg := <-errs; errMsg != nil {
			t.Fatalf("ExecuteStreamWithAuthManager: %v", errMsg.Error)
		}
	}
	mu.Lock()
	for _, id := range invoked {
		if !allowed[id] {
			t.Fatalf("restricted key reached disallowed credential %s (all=%v)", id, invoked)
		}
	}
	if len(invoked) != 30 {
		t.Fatalf("invocations = %d, want 30", len(invoked))
	}
	invoked = nil
	mu.Unlock()

	// The unrestricted label keeps reaching every credential.
	agents := ginContextForKey("test-key-agents", true)
	for i := 0; i < 20; i++ {
		if _, _, errMsg := handler.ExecuteWithAuthManager(agents, "openai", model, body, ""); errMsg != nil {
			t.Fatalf("unrestricted ExecuteWithAuthManager: %v", errMsg.Error)
		}
	}
	mu.Lock()
	seen := map[string]bool{}
	for _, id := range invoked {
		seen[id] = true
	}
	mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("unrestricted key should rotate over all 4 credentials, got %v", seen)
	}
}

// A restricted key keeps the usual model-not-found error for a model nobody serves; the allowlist
// error is only for a model that exists but is served by no allowed credential.
func TestRestrictedKeyModelErrors(t *testing.T) {
	servedElsewhere := "handler-api-key-allowlist-elsewhere-model"
	executor := &interceptorCaptureExecutor{
		provider: "handler-api-key-allowlist-errors",
		execute: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
			t.Fatalf("a credential was invoked: %s", auth.ID)
			return coreexecutor.Response{}, nil
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	outside := &coreauth.Auth{ID: "ak-outside.json", Provider: executor.provider, Status: coreauth.StatusActive}
	registry.GetGlobalRegistry().RegisterClient(outside.ID, outside.Provider, []*registry.ModelInfo{{ID: servedElsewhere}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(outside.ID) })
	if _, err := manager.Register(context.Background(), outside); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := NewBaseAPIHandlers(allowlistTestConfig(), manager)
	ctx := ginContextForKey("test-key-monitor", true)

	errorCode := func(errMsg *interfaces.ErrorMessage) (int, string) {
		t.Helper()
		if errMsg == nil {
			t.Fatal("want an error")
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(BuildErrorResponseBodyWithError(errMsg.StatusCode, errMsg.Error.Error(), errMsg.Error), &body); err != nil {
			t.Fatalf("error body is not JSON: %v", err)
		}
		return errMsg.StatusCode, body.Error.Code
	}

	_, _, unknown := handler.ExecuteWithAuthManager(ctx, "openai", "handler-api-key-allowlist-nobody-serves-this", []byte(`{}`), "")
	if status, code := errorCode(unknown); status != http.StatusBadRequest || code != "model_not_found" {
		t.Fatalf("unknown model: got %d %q, want 400 model_not_found", status, code)
	}

	_, _, elsewhere := handler.ExecuteWithAuthManager(ctx, "openai", servedElsewhere, []byte(`{}`), "")
	if status, code := errorCode(elsewhere); status != http.StatusServiceUnavailable || code != "api_key_allowlist_unavailable" {
		t.Fatalf("model served only outside the allowlist: got %d %q, want 503 api_key_allowlist_unavailable", status, code)
	}
}

// The restriction bound to the authenticated principal wins over a later lookup: a request
// admitted under a configuration that restricted its key keeps that restriction even when the
// configuration it is dispatched under no longer names the key.
func TestAuthSelectionMetadataPrefersTheRestrictionBoundAtAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("userApiKey", "retired-key")
	c.Set(sdkaccess.CredentialAllowlistContextKey, &sdkaccess.CredentialAllowlist{AuthIDs: []string{"ak-allowed.json"}, Tags: []string{"monitor-pool"}})
	ctx := context.WithValue(context.Background(), "gin", c)

	for name, cfg := range map[string]*sdkconfig.SDKConfig{
		"nil config":               nil,
		"config without the key":   {APIKeys: []string{"other"}},
		"config with the key free": {APIKeys: []string{"retired-key"}},
	} {
		meta := AuthSelectionMetadata(ctx, cfg)
		got := coreexecutor.AllowedAuthsFromMetadata(meta)
		if got == nil {
			t.Fatalf("%s: bound restriction lost, metadata = %v", name, meta)
		}
		hasTag := func(tag string) bool { return tag == "monitor-pool" }
		if !got.Allows("ak-allowed.json", nil) || !got.Allows("x", hasTag) || got.Allows("other.json", nil) {
			t.Fatalf("%s: wrong allow set %+v", name, got)
		}
	}

	// Without a bound restriction the live configuration decides, as before.
	plain, _ := gin.CreateTestContext(httptest.NewRecorder())
	plain.Request = c.Request
	plain.Set("userApiKey", "test-key-monitor")
	live := allowlistTestConfig()
	if coreexecutor.AllowedAuthsFromMetadata(AuthSelectionMetadata(context.WithValue(context.Background(), "gin", plain), live)) == nil {
		t.Fatal("the live lookup no longer restricts a labelled key")
	}
	if len(AuthSelectionMetadata(context.WithValue(context.Background(), "gin", plain), &sdkconfig.SDKConfig{APIKeys: live.APIKeys})) != 0 {
		t.Fatal("an unrestricted key gained metadata")
	}
}

// A bound value that is not a credential allowlist must fail closed, never read as unrestricted.
func TestAuthSelectionMetadataFailsClosedOnAMalformedBoundValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, bound := range map[string]any{"wrong type": "monitor", "typed nil": (*sdkaccess.CredentialAllowlist)(nil)} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Set("userApiKey", "any-key")
		c.Set(sdkaccess.CredentialAllowlistContextKey, bound)
		got := coreexecutor.AllowedAuthsFromMetadata(AuthSelectionMetadata(context.WithValue(context.Background(), "gin", c), nil))
		if got == nil || got.Allows("any.json", func(string) bool { return true }) {
			t.Fatalf("%s: want an allowlist that allows nothing, got %+v", name, got)
		}
	}
}
