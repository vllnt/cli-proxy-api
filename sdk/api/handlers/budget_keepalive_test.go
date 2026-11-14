package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const keepAliveTestWait = 5 * time.Second

// flushSignalRecorder closes flushed on the first Flush, so a test can wait for a keep-alive write
// without sleeping.
type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (r *flushSignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.flushed) })
}

func budgetRequestOn(w http.ResponseWriter, apiKey string) (context.Context, *gin.Context) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if apiKey != "" {
		c.Set("userApiKey", apiKey)
	}
	return context.WithValue(context.Background(), "gin", c), c
}

// waitFor blocks until ch is closed or the test wait elapses.
func waitFor(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(keepAliveTestWait):
		return false
	}
}

// streamWithKeepAlive runs the pre-first-token keep-alive bootstrap the way the stream handlers do.
// keepAliveC may be nil. The returned recorder is the response the keep-alives are written to.
func streamWithKeepAlive(
	t *testing.T, handler *BaseAPIHandler, model string, keepAliveC <-chan time.Time, onKeepAlive func(),
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, c := budgetRequestOn(recorder, budgetKeyLabelled)
	writeKeepAlive := func() bool {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		c.Writer.Flush()
		if onKeepAlive != nil {
			onKeepAlive()
		}
		return true
	}
	data, _, errs, ok := handler.ExecuteStreamWithPreFTKeepAlive(
		c, ctx, func(error) {}, "claude", model, []byte(`{"model":"`+model+`"}`), "", keepAliveC, writeKeepAlive,
	)
	if !ok {
		t.Fatal("ExecuteStreamWithPreFTKeepAlive reported the request as canceled")
	}
	if chunks, errMsg := drainStream(data, errs); errMsg != nil || chunks != 1 {
		t.Fatalf("stream: chunks=%d err=%+v", chunks, errMsg)
	}
	return c, recorder
}

func okStream() (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

// Admission runs on the bootstrap goroutine while the handler goroutine writes keep-alives: the budget
// headers must not touch the response writer concurrently (a concurrent map write is a fatal error).
func TestStreamKeepAliveDoesNotRaceWithBudgetAdmission(t *testing.T) {
	const model = "claude-fable-keepalive-stream"
	installBudgetEngine(t, fableBudget("enforce", 1000))
	handler, exec := newBudgetHandlerFixture(t, model, budgetTestConfig(fableBudget("enforce", 1000)))
	keepAliveWritten := make(chan struct{})
	var once sync.Once
	exec.stream = func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
		if !waitFor(keepAliveWritten) {
			return nil, errors.New("no keep-alive was written")
		}
		return okStream()
	}

	keepAliveC := make(chan time.Time)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case keepAliveC <- time.Time{}:
			case <-done:
				return
			}
		}
	}()
	streamWithKeepAlive(t, handler, model, keepAliveC, func() { once.Do(func() { close(keepAliveWritten) }) })
}

func TestStreamKeepAliveKeepsBudgetHeadersUntilTheResponseStarts(t *testing.T) {
	const model = "claude-fable-keepalive-headers"
	installBudgetEngine(t, fableBudget("enforce", 1000))
	handler, _ := newBudgetHandlerFixture(t, model, budgetTestConfig(fableBudget("enforce", 1000)))

	c, recorder := streamWithKeepAlive(t, handler, model, make(chan time.Time), nil)

	if c.Writer.Written() {
		t.Fatal("no keep-alive was due, the response must not have started")
	}
	if got := c.Writer.Header().Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000", got)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want nothing written yet", recorder.Body.String())
	}
}

func nonStreamKeepAliveFixture(t *testing.T, model string) (*BaseAPIHandler, *budgetExecutor) {
	t.Helper()
	cfg := budgetTestConfig(fableBudget("enforce", 1000))
	cfg.NonStreamKeepAliveInterval = 1
	installBudgetEngine(t, fableBudget("enforce", 1000))
	return newBudgetHandlerFixture(t, model, cfg)
}

// The non-stream keep-alive ticker writes the response from its own goroutine while the handler runs
// admission.
func TestNonStreamKeepAliveDoesNotRaceWithBudgetAdmission(t *testing.T) {
	const model = "claude-fable-keepalive-nonstream"
	handler, exec := nonStreamKeepAliveFixture(t, model)
	recorder := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	exec.execute = func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
		if !waitFor(recorder.flushed) {
			return coreexecutor.Response{}, errors.New("no keep-alive was written")
		}
		return coreexecutor.Response{Payload: []byte("ok")}, nil
	}
	ctx, c := budgetRequestOn(recorder, budgetKeyLabelled)

	stop := handler.StartNonStreamingKeepAlive(c, ctx)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "claude", model, []byte(`{"model":"`+model+`"}`), "")
	stop()

	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() = %+v", errMsg)
	}
	if !c.Writer.Written() {
		t.Fatal("the keep-alive never started the response")
	}
}

func TestNonStreamKeepAliveKeepsBudgetHeadersUntilTheResponseStarts(t *testing.T) {
	const model = "claude-fable-keepalive-nonstream-headers"
	handler, _ := nonStreamKeepAliveFixture(t, model)
	recorder := httptest.NewRecorder()
	ctx, c := budgetRequestOn(recorder, budgetKeyLabelled)

	stop := handler.StartNonStreamingKeepAlive(c, ctx)
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "claude", model, []byte(`{"model":"`+model+`"}`), "")
	stop()

	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() = %+v", errMsg)
	}
	if c.Writer.Written() {
		t.Fatal("the request finished before the first keep-alive, the response must not have started")
	}
	if got := c.Writer.Header().Get("X-Budget-Remaining"); got != "1000" {
		t.Fatalf("X-Budget-Remaining = %q, want 1000", got)
	}
}
