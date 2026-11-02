package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// cancelAfterContext reports a cancellation from the nth call to Err() on, so a reload can be
// cut at every point where UpdateClientsContext checks for cancellation.
type cancelAfterContext struct {
	context.Context
	allowed int32
	calls   atomic.Int32
}

func (c *cancelAfterContext) Err() error {
	if c.calls.Add(1) > c.allowed {
		return context.Canceled
	}
	return nil
}

func reloadConfigWithMonitorKey(base *proxyconfig.Config) *proxyconfig.Config {
	next := *base
	next.SDKConfig = sdkconfig.SDKConfig{
		APIKeys:      []string{"test-key", "monitor-key"},
		APIKeyLabels: map[string]string{"monitor": "monitor-key"},
		APIKeyAuthAllowlist: map[string]sdkconfig.APIKeyAuthAllowlist{
			"monitor": {AuthIDs: []string{"claude-spare.json"}},
		},
	}
	return &next
}

// A key admitted by a reload must never be served under a policy older than the key: if the
// reload is cut between publishing the client keys and the allowlist, the new restricted key would
// authenticate with no restriction until the next reload.
func TestReloadNeverAdmitsAKeyBeforeItsAllowlist(t *testing.T) {
	probe := newTestServer(t)
	counter := &cancelAfterContext{Context: context.Background(), allowed: 1 << 20}
	probe.UpdateClientsContext(counter, reloadConfigWithMonitorKey(probe.cfg))
	total := int(counter.calls.Load())
	if total < 2 {
		t.Fatalf("UpdateClientsContext checks for cancellation %d times, expected several", total)
	}

	for allowed := 0; allowed <= total; allowed++ {
		server := newTestServer(t)
		server.UpdateClientsContext(&cancelAfterContext{Context: context.Background(), allowed: int32(allowed)}, reloadConfigWithMonitorKey(server.cfg))

		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer monitor-key")
		result, err := server.accessManager.Authenticate(context.Background(), request)
		admitted := err == nil && result != nil
		_, _, restricted := server.handlers.Cfg.APIKeyAuthAllowlistFor("monitor-key")
		if admitted && !restricted {
			t.Fatalf("reload cut after %d cancellation checks: monitor-key is admitted but the handlers have no allowlist for it", allowed)
		}
		if allowed == total && !admitted {
			t.Fatal("an uncancelled reload must admit the new key (the test would otherwise pass vacuously)")
		}
	}
}

// observeProvider is an access provider that never authenticates anyone. A reload asks every
// registered provider for its identifier after it has published the new handlers config and
// before the access manager switches to the new client keys, which is the window under test.
type observeProvider struct {
	inWindow func()
}

func (p *observeProvider) Identifier() string {
	if fn := p.inWindow; fn != nil {
		p.inWindow = nil
		fn()
	}
	return "test-observer"
}

func (p *observeProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, sdkaccess.NewNotHandledError()
}

// A key that a reload revokes must not be served unrestricted in the instant between the new
// handlers config and the new access providers: the access manager still admits it then.
func TestReloadKeepsARetiringKeyRestrictedUntilAccessStopsAdmittingIt(t *testing.T) {
	observer := &observeProvider{}
	sdkaccess.RegisterProvider("test-observer", observer)
	t.Cleanup(func() { sdkaccess.UnregisterProvider("test-observer") })

	server := newTestServer(t)
	withMonitor := func(key string, ids ...string) *proxyconfig.Config {
		next := *server.cfg
		next.SDKConfig = sdkconfig.SDKConfig{
			APIKeys:             []string{"test-key", key},
			APIKeyLabels:        map[string]string{"monitor": key},
			APIKeyAuthAllowlist: map[string]sdkconfig.APIKeyAuthAllowlist{"monitor": {AuthIDs: ids}},
		}
		return &next
	}
	if !server.UpdateClientsContext(context.Background(), withMonitor("monitor-key-a", "claude-spare.json")) {
		t.Fatal("initial reload failed")
	}

	type seen struct{ admitted, restricted bool }
	inWindow := map[string]seen{}
	observer.inWindow = func() {
		for _, key := range []string{"monitor-key-a", "monitor-key-b"} {
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer "+key)
			result, err := server.accessManager.Authenticate(context.Background(), request)
			_, _, restricted := server.handlers.Cfg.APIKeyAuthAllowlistFor(key)
			inWindow[key] = seen{admitted: err == nil && result != nil, restricted: restricted}
		}
	}
	if !server.UpdateClientsContext(context.Background(), withMonitor("monitor-key-b", "claude-other.json")) {
		t.Fatal("rotation reload failed")
	}

	if len(inWindow) != 2 {
		t.Fatalf("the window was not observed: %v", inWindow)
	}
	if got := inWindow["monitor-key-a"]; !got.admitted || !got.restricted {
		t.Fatalf("revoked key in the window: admitted=%v restricted=%v, want admitted and restricted", got.admitted, got.restricted)
	}
	if got := inWindow["monitor-key-b"]; got.admitted {
		t.Fatalf("new key admitted before the access providers switched: %+v", got)
	}

	// After the reload: the old key is rejected, the new one is admitted under the new entry, and
	// the handlers hold exactly the new config (no leftover restriction for the retired key).
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer monitor-key-a")
	if result, err := server.accessManager.Authenticate(context.Background(), request); err == nil && result != nil {
		t.Fatal("revoked key is still admitted after the reload")
	}
	if _, entry, ok := server.handlers.Cfg.APIKeyAuthAllowlistFor("monitor-key-b"); !ok || len(entry.AuthIDs) != 1 || entry.AuthIDs[0] != "claude-other.json" {
		t.Fatalf("new key after the reload: restricted=%v entry=%+v", ok, entry)
	}
	if _, _, ok := server.handlers.Cfg.APIKeyAuthAllowlistFor("monitor-key-a"); ok {
		t.Fatal("the handlers still carry the retired key's restriction after the reload")
	}
}
