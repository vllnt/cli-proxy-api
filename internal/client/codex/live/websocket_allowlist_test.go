package live

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// recordingRealtimeUpstream is a Realtime server that records the bearer token of every handshake it
// accepts. Each credential in these tests has its own token, so the log says which account was used.
type recordingRealtimeUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
}

func newRecordingRealtimeUpstream(t *testing.T) *recordingRealtimeUpstream {
	t.Helper()
	upstream := &recordingRealtimeUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Record before the upgrade: the 101 reaches the proxy first, so a handshake that was made and then
		// followed by a refusal would otherwise race with the assertion that nothing was dialed.
		upstream.mu.Lock()
		upstream.tokens = append(upstream.tokens, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		upstream.mu.Unlock()
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *recordingRealtimeUpstream) handshakes() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.tokens...)
}

func (u *recordingRealtimeUpstream) wsBase() string {
	return "ws" + strings.TrimPrefix(u.URL, "http") + "/v1"
}

// dialRealtime opens a downstream websocket. A refused handshake returns its HTTP response and body.
func dialRealtime(t *testing.T, server *httptest.Server, path string) (*websocket.Conn, int, string) {
	t.Helper()
	connection, response, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+path, nil)
	if errDial == nil {
		return connection, http.StatusSwitchingProtocols, ""
	}
	if response == nil {
		t.Fatalf("dial %s: %v", path, errDial)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return nil, response.StatusCode, string(body)
}

// The websocket entry points (direct Realtime and call sideband) each select an account themselves, so
// each must honor the restriction of the principal that opened the socket: only an allowed account is
// ever dialed, a list that nothing satisfies is refused before any upstream connection, and a sideband
// join of a call pinned to an account outside the list is refused the same way.
func TestRealtimeWebsocketEntryPointsHonorTheAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		callID  = "call-123"
		pinned  = "ws-pinned"
		spare   = "ws-spare"
		limited = "test-key-monitor"
	)
	tokenOf := map[string]string{pinned: "token-pinned", spare: "token-spare"}
	boundTo := func(ids ...string) *sdkaccess.CredentialAllowlist {
		return &sdkaccess.CredentialAllowlist{AuthIDs: ids}
	}

	tests := []struct {
		name      string
		path      string          // direct: /v1/realtime?model=...; sideband: /v1/realtime?call_id=...
		config    []string        // configured allowlist for the limited key (nil = none)
		bound     any             // restriction bound at authentication (nil = none)
		principal string          // userApiKey
		wantToken map[string]bool // tokens the upstream may see; empty = refused before any dial
	}{
		{"direct, unrestricted principal", "/v1/realtime?model=gpt-realtime", nil, nil, "test-key-agents", map[string]bool{"token-pinned": true, "token-spare": true}},
		{"direct, configured list", "/v1/realtime?model=gpt-realtime", []string{spare}, nil, limited, map[string]bool{"token-spare": true}},
		{"direct, bound list", "/v1/realtime?model=gpt-realtime", nil, boundTo(pinned), "plugin-principal", map[string]bool{"token-pinned": true}},
		{"direct, bound list wins over config", "/v1/realtime?model=gpt-realtime", []string{pinned}, boundTo(spare), limited, map[string]bool{"token-spare": true}},
		{"direct, list matches nothing", "/v1/realtime?model=gpt-realtime", []string{"ws-gone"}, nil, limited, nil},
		{"direct, empty bound list", "/v1/realtime?model=gpt-realtime", nil, boundTo(), "plugin-principal", nil},
		{"direct, malformed bound value", "/v1/realtime?model=gpt-realtime", nil, "not-a-list", "plugin-principal", nil},
		{"direct, typed nil bound value", "/v1/realtime?model=gpt-realtime", nil, (*sdkaccess.CredentialAllowlist)(nil), "plugin-principal", nil},

		{"sideband, unrestricted principal keeps the pinned account", "/v1/realtime?call_id=" + callID, nil, nil, "test-key-agents", map[string]bool{"token-pinned": true}},
		{"sideband, bound list contains the pinned account", "/v1/realtime?call_id=" + callID, nil, boundTo(pinned), "plugin-principal", map[string]bool{"token-pinned": true}},
		{"sideband, bound list excludes the pinned account", "/v1/realtime?call_id=" + callID, nil, boundTo(spare), "plugin-principal", nil},
		{"sideband, configured list excludes the pinned account", "/v1/realtime?call_id=" + callID, []string{spare}, nil, limited, nil},
		{"sideband, empty bound list", "/v1/realtime?call_id=" + callID, nil, boundTo(), "plugin-principal", nil},
		{"sideband, malformed bound value", "/v1/realtime?call_id=" + callID, nil, "not-a-list", "plugin-principal", nil},
		{"sideband, typed nil bound value", "/v1/realtime?call_id=" + callID, nil, (*sdkaccess.CredentialAllowlist)(nil), "plugin-principal", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newRecordingRealtimeUpstream(t)
			manager := auth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&captureExecutor{})
			for _, id := range []string{pinned, spare} {
				registerCredential(t, manager, &auth.Auth{
					ID:       id,
					Provider: "codex",
					Status:   auth.StatusActive,
					Metadata: map[string]any{"access_token": tokenOf[id]},
				})
			}
			cfg := &config.Config{}
			cfg.APIKeys = []string{"test-key-agents", limited}
			cfg.APIKeyLabels = map[string]string{"monitor": limited}
			if tt.config != nil {
				cfg.APIKeyAuthAllowlist = map[string]config.APIKeyAuthAllowlist{"monitor": {AuthIDs: tt.config}}
			}
			handler := NewHandler(manager, cfg)
			handler.sidebandAPIBaseURL = upstream.wsBase()
			handler.sessions.put(callID, liveSession{authID: pinned, model: defaultLiveModel})

			router := gin.New()
			router.GET("/v1/realtime", func(c *gin.Context) {
				c.Set("userApiKey", tt.principal)
				c.Set("accessProvider", "static")
				if tt.bound != nil {
					c.Set(sdkaccess.CredentialAllowlistContextKey, tt.bound)
				}
				c.Next()
			}, handler.HandleRealtimeWebsocket)
			downstream := httptest.NewServer(router)
			defer downstream.Close()

			connection, status, body := dialRealtime(t, downstream, tt.path)
			if connection != nil {
				defer func() { _ = connection.Close() }()
				_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, _, errRead := connection.ReadMessage(); errRead != nil {
					t.Fatalf("read first upstream frame: %v", errRead)
				}
			}

			got := upstream.handshakes()
			if len(tt.wantToken) == 0 {
				if status != http.StatusServiceUnavailable || !strings.Contains(body, "no allowed account is currently available for this API key") {
					t.Fatalf("refusal = %d %q, want 503 with the allowlist message", status, body)
				}
				if len(got) != 0 {
					t.Fatalf("upstream was dialed with %v although no allowed account exists", got)
				}
				for _, token := range tokenOf {
					if strings.Contains(body, token) {
						t.Fatalf("refusal leaks a credential: %s", body)
					}
				}
				for _, id := range []string{pinned, spare} {
					if strings.Contains(body, id) {
						t.Fatalf("refusal names an account (%s): %s", id, body)
					}
				}
				return
			}
			if connection == nil {
				t.Fatalf("handshake refused with %d %q, want an allowed account to serve it", status, body)
			}
			if len(got) != 1 || !tt.wantToken[got[0]] {
				t.Fatalf("upstream handshakes = %v, want one of %v", got, tt.wantToken)
			}
		})
	}
}
