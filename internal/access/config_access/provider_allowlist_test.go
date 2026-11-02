package configaccess

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func authenticateWithKey(t *testing.T, p sdkaccess.Provider, key string) *sdkaccess.Result {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	result, authErr := p.Authenticate(request.Context(), request)
	if authErr != nil {
		t.Fatalf("Authenticate(%q) error = %v", key, authErr)
	}
	return result
}

// The credential allowlist of a client key travels in the authentication result, taken from the
// same configuration snapshot that admitted the key: a request can never be admitted under one
// configuration and restricted under another.
func TestProviderBindsTheCredentialAllowlistToTheAdmittedKey(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{
		APIKeys:      []string{"agents-key", "monitor-key"},
		APIKeyLabels: map[string]string{"agents": "agents-key", "monitor": "monitor-key"},
		APIKeyAuthAllowlist: map[string]sdkconfig.APIKeyAuthAllowlist{
			"monitor": {AuthIDs: []string{"claude-spare.json"}, Tags: []string{"monitor-pool"}},
		},
	}
	Register(cfg)
	t.Cleanup(func() { sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey) })
	provider := sdkaccess.RegisteredProviders()[0]

	monitor := authenticateWithKey(t, provider, "monitor-key")
	want := &sdkaccess.CredentialAllowlist{AuthIDs: []string{"claude-spare.json"}, Tags: []string{"monitor-pool"}}
	if !reflect.DeepEqual(monitor.CredentialAllowlist, want) {
		t.Fatalf("monitor allowlist = %+v, want %+v", monitor.CredentialAllowlist, want)
	}
	if agents := authenticateWithKey(t, provider, "agents-key"); agents.CredentialAllowlist != nil {
		t.Fatalf("a key without an entry must carry no allowlist, got %+v", agents.CredentialAllowlist)
	}

	// The snapshot is private to the provider: later edits to the config value do not reach it.
	cfg.APIKeyAuthAllowlist["monitor"].AuthIDs[0] = "tampered.json"
	cfg.APIKeyAuthAllowlist = nil
	if got := authenticateWithKey(t, provider, "monitor-key").CredentialAllowlist; got == nil || got.AuthIDs[0] != "claude-spare.json" {
		t.Fatalf("provider allowlist changed with the config value: %+v", got)
	}
	monitor.CredentialAllowlist.AuthIDs[0] = "tampered-result.json"
	if got := authenticateWithKey(t, provider, "monitor-key").CredentialAllowlist; got.AuthIDs[0] != "claude-spare.json" {
		t.Fatalf("a caller changed the provider's allowlist through a result: %+v", got)
	}
}

func TestProviderWithoutAllowlistCarriesNone(t *testing.T) {
	Register(&sdkconfig.SDKConfig{APIKeys: []string{"agents-key"}})
	t.Cleanup(func() { sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey) })
	if got := authenticateWithKey(t, sdkaccess.RegisteredProviders()[0], "agents-key"); got.CredentialAllowlist != nil {
		t.Fatalf("allowlist = %+v, want none", got.CredentialAllowlist)
	}
}
