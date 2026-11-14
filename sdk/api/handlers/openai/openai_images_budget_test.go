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

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	imagesBudgetKey       = "test-key-images-budget"
	imagesBudgetWait      = 5 * time.Second
	imagesBudgetCompleted = "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}\n\n"
)

// installImagesBudget makes a fresh engine with one enforced budget for imagesBudgetKey the default.
func installImagesBudget(t *testing.T) {
	t.Helper()
	engine := budget.New()
	cfg := &sdkconfig.SDKConfig{
		APIKeys:      []string{imagesBudgetKey},
		APIKeyLabels: map[string]string{"images": imagesBudgetKey},
		APIKeyBudgets: &sdkconfig.APIKeyBudgets{Labels: map[string][]sdkconfig.APIKeyBudgetRule{
			"images": {{Models: "*", Window: "day", Limit: 1000, Mode: "enforce"}},
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

// keepAliveTicks installs a pre-first-token keep-alive ticker that fires as fast as the handler takes
// ticks (always == true) or never, and returns its restore function.
func keepAliveTicks(t *testing.T, always bool) {
	t.Helper()
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
}

// waitForKeepAlive holds an executor until the handler has written a keep-alive, or fails it.
func waitForKeepAlive(recorder *flushSignalRecorder) error {
	select {
	case <-recorder.flushed:
		return nil
	case <-time.After(imagesBudgetWait):
		return errors.New("no keep-alive was written")
	}
}

func serveImagesWithBudget(t *testing.T, recorder http.ResponseWriter, path string, handler gin.HandlerFunc, body string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", imagesBudgetKey) }, handlers.BudgetHeadersMiddleware())
	router.POST(path, handler)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
}

func requireImagesBudgetHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000 (headers: %v)", got, header)
	}
	if header.Get("X-Budget-Reset") == "" || header.Get("X-Budget-Mode") != "enforce" {
		t.Fatalf("incomplete budget headers: %v", header)
	}
}

// gatedImageExecutor serves both image paths. Its calls wait for gate (nil means no wait), which a test
// closes once the handler has written a keep-alive.
type gatedImageExecutor struct {
	provider string
	gate     func() error
}

func (e *gatedImageExecutor) Identifier() string { return e.provider }

func (e *gatedImageExecutor) wait() error {
	if e.gate == nil {
		return nil
	}
	return e.gate()
}

func (e *gatedImageExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	if err := e.wait(); err != nil {
		return coreexecutor.Response{}, err
	}
	return coreexecutor.Response{
		Payload: []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U=","mime_type":"image/png"}]}`),
	}, nil
}

func (e *gatedImageExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if err := e.wait(); err != nil {
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(imagesBudgetCompleted)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *gatedImageExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *gatedImageExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *gatedImageExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// imagesBudgetHandler registers one credential serving model and returns the images handler.
func imagesBudgetHandler(t *testing.T, exec *gatedImageExecutor, model, modelType string, nonStreamKeepAlive int) *OpenAIAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "auth-images-budget-" + exec.provider, Provider: exec.provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: model, Object: "model", OwnedBy: exec.provider, Type: modelType},
	})
	manager.RefreshSchedulerEntry(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		NonStreamKeepAliveInterval: nonStreamKeepAlive,
		Streaming:                  sdkconfig.StreamingConfig{KeepAliveSeconds: 20, SendKeepAliveBeforeFT: true},
	}, manager)
	return NewOpenAIAPIHandler(base)
}

// The routed (codex) image stream runs admission on the goroutine of waitImagesStreamExecution while the
// request goroutine writes keep-alives.
func TestRoutedImagesStreamAdmissionDoesNotRaceWithKeepAlives(t *testing.T) {
	installImagesBudget(t)
	keepAliveTicks(t, true)
	recorder := newFlushSignalRecorder()
	exec := &gatedImageExecutor{provider: "codex", gate: func() error { return waitForKeepAlive(recorder) }}
	handler := imagesBudgetHandler(t, exec, defaultImagesToolModel, "codex", 0)

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations,
		`{"model":"`+defaultImagesToolModel+`","prompt":"a red fox","stream":true}`)

	if !strings.Contains(recorder.Body.String(), ": keep-alive") {
		t.Fatalf("no keep-alive reached the client: %q", recorder.Body.String())
	}
}

func TestRoutedImagesStreamCarriesBudgetHeadersWhenAdmissionPrecedesTheCommit(t *testing.T) {
	installImagesBudget(t)
	keepAliveTicks(t, false)
	handler := imagesBudgetHandler(t, &gatedImageExecutor{provider: "codex"}, defaultImagesToolModel, "codex", 0)
	recorder := httptest.NewRecorder()

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations,
		`{"model":"`+defaultImagesToolModel+`","prompt":"a red fox","stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	requireImagesBudgetHeaders(t, recorder.Result().Header)
}

// The xAI image stream runs ExecuteImageWithAuthManager, and so admission, on a goroutine of
// streamImagesWithModel while the request goroutine writes keep-alives.
func TestXAIImagesStreamAdmissionDoesNotRaceWithKeepAlives(t *testing.T) {
	installImagesBudget(t)
	keepAliveTicks(t, true)
	recorder := newFlushSignalRecorder()
	exec := &gatedImageExecutor{provider: "xai", gate: func() error { return waitForKeepAlive(recorder) }}
	handler := imagesBudgetHandler(t, exec, defaultXAIImagesModel, "xai", 0)

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations,
		`{"model":"`+defaultXAIImagesModel+`","prompt":"a red square","stream":true,"response_format":"b64_json"}`)

	if !strings.Contains(recorder.Body.String(), ": keep-alive") {
		t.Fatalf("no keep-alive reached the client: %q", recorder.Body.String())
	}
}

func TestXAIImagesStreamCarriesBudgetHeadersWhenAdmissionPrecedesTheCommit(t *testing.T) {
	installImagesBudget(t)
	keepAliveTicks(t, false)
	handler := imagesBudgetHandler(t, &gatedImageExecutor{provider: "xai"}, defaultXAIImagesModel, "xai", 0)
	recorder := httptest.NewRecorder()

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations,
		`{"model":"`+defaultXAIImagesModel+`","prompt":"a red square","stream":true,"response_format":"b64_json"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	requireImagesBudgetHeaders(t, recorder.Result().Header)
}

const xaiNonStreamImagesBody = `{"model":"` + defaultXAIImagesModel + `","prompt":"a red square","response_format":"b64_json"}`

// The non-stream keep-alive writes the response from its own goroutine while the handler runs admission.
func TestXAIImagesNonStreamAdmissionDoesNotRaceWithKeepAlives(t *testing.T) {
	installImagesBudget(t)
	recorder := newFlushSignalRecorder()
	exec := &gatedImageExecutor{provider: "xai", gate: func() error { return waitForKeepAlive(recorder) }}
	handler := imagesBudgetHandler(t, exec, defaultXAIImagesModel, "xai", 1)

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations, xaiNonStreamImagesBody)

	if !recorder.Flushed {
		t.Fatal("the keep-alive never started the response")
	}
}

func TestXAIImagesNonStreamCarriesBudgetHeadersWhenAdmissionPrecedesTheCommit(t *testing.T) {
	installImagesBudget(t)
	handler := imagesBudgetHandler(t, &gatedImageExecutor{provider: "xai"}, defaultXAIImagesModel, "xai", 0)
	recorder := httptest.NewRecorder()

	serveImagesWithBudget(t, recorder, imagesGenerationsPath, handler.ImagesGenerations, xaiNonStreamImagesBody)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	requireImagesBudgetHeaders(t, recorder.Result().Header)
}
