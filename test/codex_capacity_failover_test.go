package test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const capacityTestModel = "gpt-6-astra"

// codexOverloadedStream is how the Codex upstream sheds load: an HTTP 200 stream whose handshake,
// heartbeats and an empty reasoning item arrive before the request is shed with
// server_is_overloaded. Nothing in it is generated output.
const codexOverloadedStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_a\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.in_progress\n" +
	"data: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":{\"id\":\"resp_a\",\"status\":\"in_progress\"}}\n\n" +
	": keep-alive\n\n" +
	"event: response.output_item.added\n" +
	"data: {\"type\":\"response.output_item.added\",\"sequence_number\":2,\"output_index\":0,\"item\":{\"id\":\"rs_a\",\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
	": keep-alive\n\n" +
	"event: error\n" +
	"data: {\"type\":\"error\",\"sequence_number\":3,\"error\":{\"message\":\"Our servers are currently overloaded. Please try again later.\",\"type\":\"service_unavailable_error\",\"param\":null,\"code\":\"server_is_overloaded\"}}\n\n"

// codexOverloadedAfterOutputStream sheds the request only after generated text was produced.
const codexOverloadedAfterOutputStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_a\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.output_item.added\n" +
	"data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_a\",\"type\":\"message\",\"content\":[]}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_a\",\"delta\":\"PARTIAL_A\"}\n\n" +
	"event: error\n" +
	"data: {\"type\":\"error\",\"sequence_number\":3,\"error\":{\"message\":\"Our servers are currently overloaded. Please try again later.\",\"type\":\"service_unavailable_error\",\"param\":null,\"code\":\"server_is_overloaded\"}}\n\n"

func codexCompletedStream(account string) string {
	return fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_%[1]s\",\"model\":%[2]q}}\n\n"+
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_%[1]s\",\"type\":\"message\"}}\n\n"+
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_%[1]s\",\"delta\":\"MOCK_%[1]s\"}\n\n"+
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%[1]s\",\"status\":\"completed\",\"output\":[]}}\n\n", account, capacityTestModel)
}

type capacityHarness struct {
	manager  *cliproxyauth.Manager
	cfg      *config.Config
	mu       sync.Mutex
	attempts []string
}

// newCapacityHarness registers codex credentials "a" and "b" (a is picked first) behind one
// upstream that serves streams[account], with session affinity and cooling enabled.
func newCapacityHarness(t *testing.T, cfg *config.Config, streams map[string]string) *capacityHarness {
	t.Helper()
	h := &capacityHarness{cfg: cfg}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer account-")
		h.mu.Lock()
		h.attempts = append(h.attempts, account)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, streams[account])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)

	h.manager = cliproxyauth.NewManager(nil, cliproxyauth.NewSessionAffinitySelector(&cliproxyauth.RoundRobinSelector{}), nil)
	h.manager.SetConfig(cfg)
	h.manager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	h.manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	for account, priority := range map[string]string{"a": "100", "b": "0"} {
		authID := "codex-capacity-" + account
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: capacityTestModel}, {ID: "gpt-6.1-sol"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := h.manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: authID, Provider: "codex", Status: cliproxyauth.StatusActive,
			Attributes: map[string]string{"priority": priority, "base_url": server.URL, "api_key": "account-" + account},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return h
}

// respond sends one Codex CLI style streaming request through the /v1/responses handler.
func (h *capacityHarness) respond(t *testing.T, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":true}`, capacityTestModel)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Session-Id", sessionID)
	openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&h.cfg.SDKConfig, h.manager)).Responses(c)
	return recorder
}

func (h *capacityHarness) takeAttempts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	attempts := h.attempts
	h.attempts = nil
	return attempts
}

// smallRetryConfig allows one extra retry round and two credentials per round.
func smallRetryConfig() *config.Config {
	cfg := &config.Config{}
	cfg.RequestRetry = 1
	cfg.MaxRetryCredentials = 2
	cfg.MaxRetryInterval = 10
	return cfg
}

// Characterizes the default: without bootstrap buffering the handshake is already
// committed when the overload arrives, so the client receives it inside an HTTP 200 stream (the
// Codex CLI reports "Selected model is at capacity") and no other credential is tried.
func TestCodexCapacityRejectionReachesClientWithoutBootstrapBuffering(t *testing.T) {
	h := newCapacityHarness(t, smallRetryConfig(), map[string]string{"a": codexOverloadedStream, "b": codexCompletedStream("b")})

	recorder := h.respond(t, "session-unbuffered")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected the overload inside a 200 stream, got %d: %s", recorder.Code, body)
	}
	if attempts := h.takeAttempts(); len(attempts) != 1 || attempts[0] != "a" {
		t.Fatalf("attempts = %v, want only [a]", attempts)
	}
}

// With bootstrap buffering the overload fails the attempt before any byte reaches the client and the
// request is served by another credential. The overloaded credential is cooled only briefly and only
// for the model that was shed, and the session moves to the credential that served it.
func TestCodexCapacityRejectionFailsOverBeforeFirstByte(t *testing.T) {
	cfg := smallRetryConfig()
	cfg.Codex.StreamBootstrapBuffering = true
	cfg.OverloadCooldownSeconds = 5
	h := newCapacityHarness(t, cfg, map[string]string{"a": codexOverloadedStream, "b": codexCompletedStream("b")})

	recorder := h.respond(t, "session-buffered")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "MOCK_b") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected b's clean completion, got %d: %s", recorder.Code, body)
	}
	if attempts := h.takeAttempts(); len(attempts) != 2 || attempts[0] != "a" || attempts[1] != "b" {
		t.Fatalf("attempts = %v, want [a b]", attempts)
	}

	overloaded, ok := h.manager.GetByID("codex-capacity-a")
	if !ok {
		t.Fatal("credential a disappeared")
	}
	state := overloaded.ModelStates[capacityTestModel]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected a short cooldown on (a, %s), got %+v", capacityTestModel, state)
	}
	if remaining := time.Until(state.NextRetryAfter); remaining <= 0 || remaining > 6*time.Second {
		t.Fatalf("(a, %s) cooldown = %v, want at most the 5s overload cooldown", capacityTestModel, remaining)
	}
	if overloaded.Quota.Reason == "credential_quota" || overloaded.ModelStates["gpt-6.1-sol"] != nil {
		t.Fatalf("overload on %s must not cool credential a for other models: quota=%+v states=%v", capacityTestModel, overloaded.Quota, overloaded.ModelStates)
	}

	// Even once a has recovered and outranks b again, the session stays on b, which served it.
	if _, _, errReset := h.manager.ResetQuota(context.Background(), "codex-capacity-a"); errReset != nil {
		t.Fatal(errReset)
	}
	if recorder := h.respond(t, "session-buffered"); !strings.Contains(recorder.Body.String(), "MOCK_b") {
		t.Fatalf("expected the session to stay on b, got %s", recorder.Body.String())
	}
	if attempts := h.takeAttempts(); len(attempts) != 1 || attempts[0] != "b" {
		t.Fatalf("attempts = %v, want [b]", attempts)
	}
}

// When every credential is shed, the client receives the overload as a pre-stream 503 rather than
// inside a 200 stream, so clients that retry 5xx (the Codex CLI transport does) retry the request.
func TestCodexCapacityRejectionOnEveryCredentialIsPreStream503(t *testing.T) {
	cfg := smallRetryConfig()
	cfg.Codex.StreamBootstrapBuffering = true
	cfg.OverloadCooldownSeconds = 5
	cfg.MaxRetryInterval = 0 // skip the wall-clock wait for the overload cooldown between rounds
	h := newCapacityHarness(t, cfg, map[string]string{"a": codexOverloadedStream, "b": codexOverloadedStream})

	recorder := h.respond(t, "session-exhausted")

	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "server_is_overloaded") {
		t.Fatalf("expected a pre-stream 503 carrying the overload, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if attempts := h.takeAttempts(); len(attempts) < 2 {
		t.Fatalf("attempts = %v, want both credentials tried", attempts)
	}
}

// Once generated output has reached the client the request is never replayed elsewhere: the overload
// is delivered in-stream after the partial output.
func TestCodexCapacityRejectionAfterOutputIsNotRetried(t *testing.T) {
	cfg := smallRetryConfig()
	cfg.Codex.StreamBootstrapBuffering = true
	cfg.OverloadCooldownSeconds = 5
	h := newCapacityHarness(t, cfg, map[string]string{"a": codexOverloadedAfterOutputStream, "b": codexCompletedStream("b")})

	recorder := h.respond(t, "session-after-output")

	body := recorder.Body.String()
	if !strings.Contains(body, "PARTIAL_A") || !strings.Contains(body, "server_is_overloaded") || strings.Contains(body, "MOCK_b") {
		t.Fatalf("expected a's partial output followed by the in-stream overload, got %s", body)
	}
	if attempts := h.takeAttempts(); len(attempts) != 1 || attempts[0] != "a" {
		t.Fatalf("attempts = %v, want only [a]", attempts)
	}
}
