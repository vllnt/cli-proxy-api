package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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
	}
}
