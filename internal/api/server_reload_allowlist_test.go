package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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
		if admitted && result.CredentialAllowlist == nil {
			t.Fatalf("reload cut after %d cancellation checks: monitor-key is admitted without its allowlist bound to the request", allowed)
		}
		if admitted && !restricted {
			t.Fatalf("reload cut after %d cancellation checks: monitor-key is admitted but the handlers have no allowlist for it", allowed)
		}
		if allowed == total && !admitted {
			t.Fatal("an uncancelled reload must admit the new key (the test would otherwise pass vacuously)")
		}
	}
}

// A request that authenticated as a restricted key keeps that restriction for its whole life,
// even when a reload that revokes the key completes before the request selects a credential:
// the old configuration restricts the key and the new one rejects it, so no state in between may
// serve it unrestricted.
func TestReloadDoesNotUnrestrictAnInFlightRequest(t *testing.T) {
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

	authenticated := make(chan struct{})
	reloaded := make(chan struct{})
	type outcome struct {
		restricted bool
		allowed    []string
	}
	got := make(chan outcome, 1)
	server.engine.GET("/in-flight", AuthMiddleware(server.accessManager), func(c *gin.Context) {
		close(authenticated) // authenticated under the old configuration...
		<-reloaded           // ...paused while a reload revokes the key...
		ctx := context.WithValue(c.Request.Context(), "gin", c)
		list := coreexecutor.APIKeyAuthAllowlistFromMetadata(handlers.AuthSelectionMetadata(ctx, server.handlers.Cfg)) // ...then selects a credential
		var out outcome
		if list != nil {
			out.restricted = true
			for _, id := range []string{"claude-spare.json", "claude-other.json", "claude-agents.json"} {
				if list.Allows(id, nil) {
					out.allowed = append(out.allowed, id)
				}
			}
		}
		got <- out
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodGet, "/in-flight", nil)
	request.Header.Set("Authorization", "Bearer monitor-key-a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.engine.ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-authenticated
	if !server.UpdateClientsContext(context.Background(), withMonitor("monitor-key-b", "claude-other.json")) {
		t.Fatal("rotation reload failed")
	}
	close(reloaded)
	<-done

	out := <-got
	if !out.restricted || len(out.allowed) != 1 || out.allowed[0] != "claude-spare.json" {
		t.Fatalf("in-flight request after the key was revoked: restricted=%v allowed=%v, want restricted to claude-spare.json", out.restricted, out.allowed)
	}

	// New requests: the revoked key is rejected, the new key is admitted under the new entry.
	for key, want := range map[string]int{"monitor-key-a": http.StatusUnauthorized, "monitor-key-b": http.StatusOK} {
		next := httptest.NewRequest(http.MethodGet, "/models-probe", nil)
		next.Header.Set("Authorization", "Bearer "+key)
		recorder := httptest.NewRecorder()
		server.engine.GET("/models-probe-"+key, AuthMiddleware(server.accessManager), func(c *gin.Context) { c.Status(http.StatusOK) })
		next.URL.Path = "/models-probe-" + key
		server.engine.ServeHTTP(recorder, next)
		if recorder.Code != want {
			t.Fatalf("%s after the reload: status %d, want %d", key, recorder.Code, want)
		}
	}
}
