package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	interactionsBudgetKey  = "test-key-interactions-budget"
	interactionsBudgetWait = 5 * time.Second
)

// installInteractionsBudget makes a fresh engine with one enforced budget for interactionsBudgetKey the default.
func installInteractionsBudget(t *testing.T) {
	t.Helper()
	engine := budget.New()
	cfg := &sdkconfig.SDKConfig{
		APIKeys:      []string{interactionsBudgetKey},
		APIKeyLabels: map[string]string{"interactions": interactionsBudgetKey},
		APIKeyBudgets: &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
			"interactions": {{Models: "*", Window: "day", Limit: 1000, Mode: "enforce"}},
		}},
	}
	if err := engine.Apply(cfg); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	previous := budget.SetDefault(engine)
	t.Cleanup(func() { budget.SetDefault(previous) })
}

// flushSignalRecorder closes flushed on the first Flush, so a test can wait for a keep-alive write
// without sleeping.
type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func newFlushSignalRecorder() *flushSignalRecorder {
	return &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
}

func (r *flushSignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

// gatedInteractionsExecutor answers a stream with one payload. When gate is set the call waits for it,
// which keeps the request in its pre-first-token phase while keep-alives are written.
type gatedInteractionsExecutor struct {
	gate func() error
}

func (*gatedInteractionsExecutor) Identifier() string { return "gated-interactions-executor" }

func (*gatedInteractionsExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *gatedInteractionsExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if e.gate != nil {
		if err := e.gate(); err != nil {
			return nil, err
		}
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"interaction.completed"}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*gatedInteractionsExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*gatedInteractionsExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*gatedInteractionsExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// serveInteractionsStream runs one budgeted streaming Interactions request. With always set the
// pre-first-token keep-alive ticker fires as fast as the handler takes ticks, otherwise it never fires.
func serveInteractionsStream(t *testing.T, recorder http.ResponseWriter, exec *gatedInteractionsExecutor, always bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	installInteractionsBudget(t)

	ticks := make(chan time.Time)
	done := make(chan struct{})
	if always {
		go func() {
			for {
				select {
				case ticks <- time.Time{}:
				case <-done:
					return
				}
			}
		}()
	}
	restore := handlers.SetStreamingKeepAliveTickerFactoryForTest(func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() {}
	})
	t.Cleanup(func() {
		close(done)
		restore()
	})

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	model := "interactions-budget-model"
	auth := &coreauth.Auth{ID: "interactions-budget-auth", Provider: exec.Identifier(), Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 10, SendKeepAliveBeforeFT: true},
	}, manager)
	h := NewGeminiAPIHandler(base)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", interactionsBudgetKey) }, handlers.BudgetHeadersMiddleware())
	router.POST("/v1beta/interactions", h.Interactions)

	request := httptest.NewRequest(http.MethodPost, "/v1beta/interactions",
		strings.NewReader(`{"model":"`+model+`","input":"hi","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
}

// Interactions runs ExecuteProtocolStreamWithAuthManager, and so admission, on a goroutine while the
// request goroutine writes keep-alives.
func TestInteractionsStreamAdmissionDoesNotRaceWithKeepAlives(t *testing.T) {
	recorder := newFlushSignalRecorder()
	exec := &gatedInteractionsExecutor{gate: func() error {
		select {
		case <-recorder.flushed:
			return nil
		case <-time.After(interactionsBudgetWait):
			return errors.New("no keep-alive was written")
		}
	}}

	serveInteractionsStream(t, recorder, exec, true)

	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("no keep-alive reached the client: %q", recorder.Body.String())
	}
}

func TestInteractionsStreamCarriesBudgetHeadersWhenAdmissionPrecedesTheCommit(t *testing.T) {
	recorder := httptest.NewRecorder()

	serveInteractionsStream(t, recorder, &gatedInteractionsExecutor{}, false)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	header := recorder.Result().Header
	if got := header.Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000 (headers: %v)", got, header)
	}
	if header.Get("X-Budget-Reset") == "" || header.Get("X-Budget-Mode") != "enforce" {
		t.Fatalf("incomplete budget headers: %v", header)
	}
}
