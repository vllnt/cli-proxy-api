package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	mistralE2EManagementKey = "mistral-e2e-management-key"
	mistralE2EClientKey     = "mistral-e2e-client-key"
	mistralE2EProvisioned   = "sk-e2e-provisioned-key"
	mistralE2EModel         = "mistral-large-4-0"
	mistralE2EReply         = "hello from fake mistral"
)

type mistralUpstreamRequest struct {
	Path          string
	Authorization string
	APIKeyHeader  string
	Body          map[string]any
}

// fakeMistralService plays both Mistral sides: the console sign-in API and the
// OpenAI-compatible inference API.
type fakeMistralService struct {
	server *httptest.Server
	// exchanged is closed once the sign-in exchange endpoint has been served.
	exchanged chan struct{}
	once      sync.Once

	mu         sync.Mutex
	pollBody   string
	chat       []mistralUpstreamRequest
	chatStatus int
}

func newFakeMistralService(t *testing.T) *fakeMistralService {
	t.Helper()
	fake := &fakeMistralService{exchanged: make(chan struct{}), pollBody: `{"status":"pending"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vibe/sign-in", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"process_id":"pid","sign_in_url":"`+fake.server.URL+`/vibe/sign-in/pid?complete_token=t","poll_url":"`+
			fake.server.URL+`/api/vibe/sign-in/pid/poll","expires_at":"`+time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339)+`"}`)
	})
	mux.HandleFunc("GET /api/vibe/sign-in/pid/poll", func(w http.ResponseWriter, _ *http.Request) {
		fake.mu.Lock()
		body := fake.pollBody
		fake.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST /api/vibe/sign-in/pid/exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"api_key":"`+mistralE2EProvisioned+`"}`)
		fake.once.Do(func() { close(fake.exchanged) })
	})
	mux.HandleFunc("POST /v1/chat/completions", fake.serveChat)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	t.Cleanup(mistralauth.OverrideEndpointsForTest(fake.server.URL+"/api", fake.server.URL+"/v1", 10*time.Millisecond))
	return fake
}

func (f *fakeMistralService) serveChat(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.chat = append(f.chat, mistralUpstreamRequest{
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		APIKeyHeader:  r.Header.Get("X-Api-Key"),
		Body:          body,
	})
	status := f.chatStatus
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"message":"Unauthorized","request_id":"r1"}`)
		return
	}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta, finish string) string {
			return fmt.Sprintf(`data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n",
				mistralE2EModel, delta, finish)
		}
		_, _ = io.WriteString(w, chunk(`{"role":"assistant","content":"hello "}`, "null"))
		_, _ = io.WriteString(w, chunk(`{"content":"from fake mistral"}`, "null"))
		_, _ = io.WriteString(w, chunk(`{}`, `"stop"`))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":"`+mistralE2EModel+
		`","choices":[{"index":0,"message":{"role":"assistant","content":"`+mistralE2EReply+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
}

func (f *fakeMistralService) setPollBody(body string) {
	f.mu.Lock()
	f.pollBody = body
	f.mu.Unlock()
}

func (f *fakeMistralService) setChatStatus(status int) {
	f.mu.Lock()
	f.chatStatus = status
	f.mu.Unlock()
}

func (f *fakeMistralService) chatRequests() []mistralUpstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mistralUpstreamRequest(nil), f.chat...)
}

// mistralProxy is the real API server (routes, access control, v8 management, executors)
// over a real auth manager and the real file store.
type mistralProxy struct {
	t       *testing.T
	url     string
	authDir string
	cfg     *config.Config
	manager *coreauth.Manager
}

func newMistralProxy(t *testing.T) *mistralProxy {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(mistralE2EManagementKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	raw := fmt.Sprintf("port: 8317\nauth-dir: %q\napi-keys: [%q]\nremote-management:\n  allow-remote: true\n  secret-key: %q\n",
		filepath.ToSlash(authDir), mistralE2EClientKey, string(hashed))
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	store := sdkAuth.GetTokenStore()
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(authDir)
	}
	manager := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	// The service binds this executor for the mistral provider (covered in sdk/cliproxy tests).
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("mistral", cfg))

	configaccess.Register(&cfg.SDKConfig)
	accessManager := sdkaccess.NewManager()
	accessManager.SetProviders(sdkaccess.RegisteredProviders())

	server := api.NewServer(cfg, manager, accessManager, configPath)
	if server == nil {
		t.Fatal("api.NewServer returned nil")
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &mistralProxy{t: t, url: httpServer.URL, authDir: authDir, cfg: cfg, manager: manager}
}

func (p *mistralProxy) do(method, path string, headers map[string]string, body string) (int, http.Header, string) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.url+path, strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

func (p *mistralProxy) management(method, path string) (int, map[string]any) {
	p.t.Helper()
	code, _, raw := p.do(method, path, map[string]string{"Authorization": "Bearer " + mistralE2EManagementKey}, "")
	var parsed map[string]any
	_ = json.Unmarshal([]byte(raw), &parsed)
	return code, parsed
}

// waitForSignInResult polls the v8 status route until the session leaves "wait".
func (p *mistralProxy) waitForSignInResult(state string) map[string]any {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, status := p.management(http.MethodGet, "/v8/management/oauth/status?state="+state)
		if code != http.StatusOK {
			p.t.Fatalf("oauth status HTTP %d", code)
		}
		if status["status"] != "wait" {
			return status
		}
	}
	p.t.Fatalf("sign-in %s did not finish", state)
	return nil
}

func (p *mistralProxy) authFiles() []string {
	p.t.Helper()
	entries, err := os.ReadDir(p.authDir)
	if err != nil {
		p.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// adoptSavedCredentials loads the saved auth files the way the file watcher does and
// publishes the credential and its models to the runtime.
func (p *mistralProxy) adoptSavedCredentials() *coreauth.Auth {
	p.t.Helper()
	auths, err := synthesizer.NewFileSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      p.cfg,
		AuthDir:     p.authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 1 {
		p.t.Fatalf("synthesize = %v, %v; want exactly one credential", auths, err)
	}
	auth := auths[0]
	if auth.Provider != "mistral" {
		p.t.Fatalf("synthesized provider = %q, want mistral", auth.Provider)
	}
	if _, err := p.manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		p.t.Fatalf("register credential: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "mistral", registry.GetMistralModels())
	p.t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return auth
}

// signIn drives the v8 management API through an approved Mistral sign-in.
func (p *mistralProxy) signIn(fake *fakeMistralService) {
	p.t.Helper()
	code, start := p.management(http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral")
	if code != http.StatusOK || start["status"] != "ok" {
		p.t.Fatalf("auth-url = %d %v", code, start)
	}
	state, _ := start["state"].(string)
	if !strings.HasPrefix(state, "mistral-") || start["url"] == "" || start["flow"] != "device" {
		p.t.Fatalf("auth-url response = %v", start)
	}
	_, waiting := p.management(http.MethodGet, "/v8/management/oauth/status?state="+state)
	if waiting["status"] != "wait" {
		p.t.Fatalf("status before approval = %v, want wait", waiting)
	}
	fake.setPollBody(`{"status":"completed","exchange_token":"exch"}`)
	select {
	case <-fake.exchanged:
	case <-time.After(10 * time.Second):
		p.t.Fatal("sign-in was never exchanged")
	}
	if done := p.waitForSignInResult(state); done["status"] != "ok" {
		p.t.Fatalf("status after approval = %v, want ok", done)
	}
}

func TestMistralLoginE2E_SignInThenServeOpenAIAndClaudeRequests(t *testing.T) {
	fake := newFakeMistralService(t)
	proxy := newMistralProxy(t)

	// Management routes are access controlled.
	if code, _, _ := proxy.do(http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral", nil, ""); code != http.StatusUnauthorized {
		t.Fatalf("auth-url without a management key = %d, want 401", code)
	}

	proxy.signIn(fake)

	files := proxy.authFiles()
	if len(files) != 1 || !strings.HasPrefix(files[0], "mistral-") {
		t.Fatalf("auth files = %v, want one mistral-<hash>.json", files)
	}
	info, err := os.Stat(filepath.Join(proxy.authDir, files[0]))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file stat = %v, %v; want mode 0600", info, err)
	}

	auth := proxy.adoptSavedCredentials()
	if auth.Metadata["api_key"] != mistralE2EProvisioned || auth.Metadata["base_url"] != fake.server.URL+"/v1" {
		t.Fatalf("reloaded credential lost its key or base URL: %v", auth.Metadata)
	}

	t.Run("openai chat completions", func(t *testing.T) {
		headers := map[string]string{"Authorization": "Bearer " + mistralE2EClientKey}
		code, _, body := proxy.do(http.MethodPost, "/v1/chat/completions", headers,
			`{"model":"`+mistralE2EModel+`","messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d: %s", code, body)
		}
		var parsed struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil || len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != mistralE2EReply {
			t.Fatalf("response = %s (err %v), want content %q", body, err, mistralE2EReply)
		}
		assertLastMistralUpstreamRequest(t, fake, false)
	})

	t.Run("openai chat completions stream", func(t *testing.T) {
		headers := map[string]string{"Authorization": "Bearer " + mistralE2EClientKey}
		code, respHeaders, body := proxy.do(http.MethodPost, "/v1/chat/completions", headers,
			`{"model":"`+mistralE2EModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK || !strings.Contains(respHeaders.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status = %d content-type = %q: %s", code, respHeaders.Get("Content-Type"), body)
		}
		if !strings.Contains(body, "hello ") || !strings.Contains(body, "from fake mistral") || !strings.Contains(body, "[DONE]") {
			t.Fatalf("stream is missing the upstream text or the terminator: %s", body)
		}
		assertLastMistralUpstreamRequest(t, fake, true)
	})

	t.Run("claude messages", func(t *testing.T) {
		headers := map[string]string{"X-Api-Key": mistralE2EClientKey, "Anthropic-Version": "2023-06-01"}
		code, _, body := proxy.do(http.MethodPost, "/v1/messages", headers,
			`{"model":"`+mistralE2EModel+`","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d: %s", code, body)
		}
		var parsed struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Type != "message" || parsed.Role != "assistant" ||
			len(parsed.Content) == 0 || parsed.Content[0].Text != mistralE2EReply {
			t.Fatalf("claude response = %s (err %v), want an assistant message with %q", body, err, mistralE2EReply)
		}
		assertLastMistralUpstreamRequest(t, fake, false)
	})

	t.Run("claude messages stream", func(t *testing.T) {
		headers := map[string]string{"X-Api-Key": mistralE2EClientKey, "Anthropic-Version": "2023-06-01"}
		code, respHeaders, body := proxy.do(http.MethodPost, "/v1/messages", headers,
			`{"model":"`+mistralE2EModel+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusOK || !strings.Contains(respHeaders.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status = %d content-type = %q: %s", code, respHeaders.Get("Content-Type"), body)
		}
		for _, want := range []string{"event: message_start", "content_block_delta", "from fake mistral", "event: message_stop"} {
			if !strings.Contains(body, want) {
				t.Fatalf("claude stream is missing %q: %s", want, body)
			}
		}
		assertLastMistralUpstreamRequest(t, fake, true)
	})

	t.Run("client access key is required", func(t *testing.T) {
		before := len(fake.chatRequests())
		code, _, _ := proxy.do(http.MethodPost, "/v1/chat/completions", map[string]string{"Authorization": "Bearer wrong-key"},
			`{"model":"`+mistralE2EModel+`","messages":[{"role":"user","content":"hi"}]}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("status with a wrong client key = %d, want 401", code)
		}
		if after := len(fake.chatRequests()); after != before {
			t.Fatalf("a rejected request still reached Mistral (%d -> %d upstream calls)", before, after)
		}
	})
}

// assertLastMistralUpstreamRequest checks what the proxy sent to Mistral for the last client request.
func assertLastMistralUpstreamRequest(t *testing.T, fake *fakeMistralService, wantStream bool) {
	t.Helper()
	requests := fake.chatRequests()
	if len(requests) == 0 {
		t.Fatal("no request reached the fake Mistral inference API")
	}
	got := requests[len(requests)-1]
	if got.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", got.Path)
	}
	if got.Authorization != "Bearer "+mistralE2EProvisioned {
		t.Errorf("upstream Authorization = %q, want the key provisioned by sign-in", got.Authorization)
	}
	if got.APIKeyHeader != "" {
		t.Errorf("client key leaked upstream in X-Api-Key: %q", got.APIKeyHeader)
	}
	if got.Body["model"] != mistralE2EModel {
		t.Errorf("upstream model = %v, want %s", got.Body["model"], mistralE2EModel)
	}
	if stream, _ := got.Body["stream"].(bool); stream != wantStream {
		t.Errorf("upstream stream = %v, want %v", got.Body["stream"], wantStream)
	}
	if messages, _ := got.Body["messages"].([]any); len(messages) == 0 {
		t.Errorf("upstream body has no OpenAI messages: %v", got.Body)
	}
}

func TestMistralLoginE2E_DeniedSignInSavesNothingAndServesNothing(t *testing.T) {
	fake := newFakeMistralService(t)
	fake.setPollBody(`{"status":"denied"}`)
	proxy := newMistralProxy(t)

	code, start := proxy.management(http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral")
	if code != http.StatusOK {
		t.Fatalf("auth-url = %d %v", code, start)
	}
	state, _ := start["state"].(string)
	done := proxy.waitForSignInResult(state)
	if message, _ := done["error"].(string); done["status"] != "error" || !strings.Contains(message, "denied") {
		t.Fatalf("status = %v, want an error that mentions the denial", done)
	}
	if files := proxy.authFiles(); len(files) != 0 {
		t.Fatalf("denied sign-in wrote auth files: %v", files)
	}

	// No credential means no models: the proxy must not forward the request anywhere.
	code, _, body := proxy.do(http.MethodPost, "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + mistralE2EClientKey},
		`{"model":"`+mistralE2EModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if code == http.StatusOK {
		t.Fatalf("request succeeded without a credential: %s", body)
	}
	if got := len(fake.chatRequests()); got != 0 {
		t.Fatalf("%d requests reached Mistral without a credential", got)
	}
}

func TestMistralLoginE2E_UpstreamRejectionOfKeySurfacesToClient(t *testing.T) {
	fake := newFakeMistralService(t)
	proxy := newMistralProxy(t)
	proxy.signIn(fake)
	auth := proxy.adoptSavedCredentials()

	fake.setChatStatus(http.StatusUnauthorized)
	code, _, body := proxy.do(http.MethodPost, "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + mistralE2EClientKey},
		`{"model":"`+mistralE2EModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 to reach the client: %s", code, body)
	}

	updated, ok := proxy.manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("credential disappeared after an upstream 401")
	}
	// Same handling as other providers: the credential is held back with its error until the
	// retry window passes. Re-signing in replaces the key (see the management tests).
	if !updated.Unavailable || updated.Status != coreauth.StatusError || updated.LastError == nil {
		t.Fatalf("a rejected key must be marked unusable, got unavailable=%v status=%v lastError=%v",
			updated.Unavailable, updated.Status, updated.LastError)
	}
}
