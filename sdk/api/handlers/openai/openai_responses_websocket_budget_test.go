package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// websocketBudgetProbeExecutor completes every turn and, on its first call, lets the test act while the
// turn is open: a follow-up frame on a duplex socket is authorized through the executor's context.
type websocketBudgetProbeExecutor struct {
	mu    sync.Mutex
	calls int
	probe func(ctx context.Context)
}

func (*websocketBudgetProbeExecutor) Identifier() string { return "codex" }

func (e *websocketBudgetProbeExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketBudgetProbeExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	if call == 1 && e.probe != nil {
		e.probe(ctx)
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-budget","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*websocketBudgetProbeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*websocketBudgetProbeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*websocketBudgetProbeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketBudgetProbeExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

const websocketBudgetModel = "claude-fable-5"

// websocketBudgetFixture serves /v1/responses for key-1 (the principal of revocableWebsocketAccessProvider),
// whose team-x label has a 1000 token enforced day budget on claude-fable-*.
type websocketBudgetFixture struct {
	t        *testing.T
	engine   *budget.Engine
	executor *websocketBudgetProbeExecutor
	conn     *websocket.Conn
}

func newWebsocketBudgetFixture(t *testing.T, steering bool) *websocketBudgetFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	engine := budget.New(budget.WithClock(func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }))
	if err := engine.Apply(&sdkconfig.SDKConfig{
		APIKeys:      []string{"key-1"},
		APIKeyLabels: map[string]string{"team-x": "key-1"},
		APIKeyBudgets: &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
			"team-x": {{Models: "claude-fable-*", Window: "day", Limit: 1000, Mode: "enforce"}},
		}},
	}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })

	executor := &websocketBudgetProbeExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:         "auth-ws-budget",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: websocketBudgetModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	access := sdkaccess.NewManager()
	access.SetProviders([]sdkaccess.Provider{&revocableWebsocketAccessProvider{}})
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexResponseSteering: steering}, manager))
	h.AccessManager = access
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer key-1"}})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return &websocketBudgetFixture{t: t, engine: engine, executor: executor, conn: conn}
}

// exhaust records enough usage to use up the day budget, as a finished request would.
func (f *websocketBudgetFixture) exhaust() {
	_, ticket := f.engine.Admit("key-1", websocketBudgetModel)
	f.engine.HandleUsage(budget.WithTicket(context.Background(), ticket), coreusage.Record{
		APIKey: "key-1", Provider: "codex", Model: websocketBudgetModel,
		Detail: coreusage.Detail{InputTokens: 5000, TotalTokens: 5000},
	})
}

// turn sends one response.create and returns the first event the client receives.
func (f *websocketBudgetFixture) turn() []byte {
	f.t.Helper()
	payload := `{"type":"response.create","model":"` + websocketBudgetModel + `","input":[{"type":"message","role":"user","content":"hi"}]}`
	if err := f.conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		f.t.Fatalf("write: %v", err)
	}
	_, msg, err := f.conn.ReadMessage()
	if err != nil {
		f.t.Fatalf("read: %v", err)
	}
	return msg
}

func requireWebsocketBudgetError(t *testing.T, what string, payload []byte) {
	t.Helper()
	if got := ojson.GetBytesNoCopy(payload, "type").String(); got != "error" {
		t.Fatalf("%s type = %q, want error (%s)", what, got, payload)
	}
	if got := int(ojson.GetBytesNoCopy(payload, "status").Int()); got != http.StatusTooManyRequests {
		t.Fatalf("%s status = %d, want 429 (%s)", what, got, payload)
	}
	if got := ojson.GetBytesNoCopy(payload, "error.code").String(); got != "budget_exhausted" {
		t.Fatalf("%s error.code = %q, want budget_exhausted (%s)", what, got, payload)
	}
	if got := ojson.GetBytesNoCopy(payload, "headers.Retry-After").String(); got != "43200" {
		t.Fatalf("%s headers.Retry-After = %q, want 43200 (%s)", what, got, payload)
	}
	message := ojson.GetBytesNoCopy(payload, "error.message").String()
	if !strings.Contains(message, "team-x") || !strings.Contains(message, "claude-fable-*") {
		t.Fatalf("%s error.message = %q, want the label and model pattern", what, message)
	}
	if strings.Contains(string(payload), "key-1") {
		t.Fatalf("%s leaks the client key: %s", what, payload)
	}
}

// A follow-up frame on a duplex socket never re-enters ExecuteStream: the turn authorizer must judge it.
func TestResponsesWebsocketDuplexFollowUpFrameIsBudgeted(t *testing.T) {
	f := newWebsocketBudgetFixture(t, true)

	var mu sync.Mutex
	var allowedPayload, blockedPayload []byte
	var allowedErr, blockedErr error
	f.executor.probe = func(ctx context.Context) {
		followUp := []byte(`{"type":"response.create","model":"` + websocketBudgetModel + `","input":[]}`)
		allowed, errAllowed := coreexecutor.AuthorizeWebsocketTurn(ctx, followUp)
		f.exhaust()
		blocked, errBlocked := coreexecutor.AuthorizeWebsocketTurn(ctx, followUp)
		mu.Lock()
		allowedPayload, allowedErr, blockedPayload, blockedErr = allowed, errAllowed, blocked, errBlocked
		mu.Unlock()
	}

	if got := ojson.GetBytesNoCopy(f.turn(), "type").String(); got != "response.completed" {
		t.Fatalf("turn 1 type = %q, want response.completed", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if allowedErr != nil || len(allowedPayload) != 0 {
		t.Fatalf("follow-up frame while budget remains = (%s, %v), want it allowed", allowedPayload, allowedErr)
	}
	if blockedErr != nil {
		t.Fatalf("follow-up frame after the budget is used up: error = %v, want a rejection payload", blockedErr)
	}
	requireWebsocketBudgetError(t, "follow-up frame", blockedPayload)
}

// The next response.create on an open socket is judged again, and the 429 keeps the socket open.
func TestResponsesWebsocketNextTurnIsBudgeted(t *testing.T) {
	f := newWebsocketBudgetFixture(t, false)

	if got := ojson.GetBytesNoCopy(f.turn(), "type").String(); got != "response.completed" {
		t.Fatalf("turn 1 type = %q, want response.completed", got)
	}
	f.exhaust()
	requireWebsocketBudgetError(t, "turn 2", f.turn())
	requireWebsocketBudgetError(t, "turn 3", f.turn())
	if got := f.executor.Calls(); got != 1 {
		t.Fatalf("executor calls = %d, want 1: a rejected turn must not reach an upstream", got)
	}
}
