package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const mistralTestAPIKey = "sk-mgmt-test-key"

// mistralSignInFake serves the Mistral sign-in endpoints. The poll answer is switchable
// so a test decides when the user "approves" the sign-in.
type mistralSignInFake struct {
	server *httptest.Server
	// polled is closed once the first poll request has been served.
	polled   chan struct{}
	pollOnce sync.Once

	mu          sync.Mutex
	pollBody    string
	startStatus int
	exchanges   int
}

func newMistralSignInFake(t *testing.T, pollInterval time.Duration) *mistralSignInFake {
	t.Helper()
	fake := &mistralSignInFake{polled: make(chan struct{}), pollBody: `{"status":"pending"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vibe/sign-in", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fake.mu.Lock()
		status := fake.startStatus
		fake.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"Too many requests."}`, status)
			return
		}
		_, _ = io.WriteString(w, `{"process_id":"pid","sign_in_url":"`+fake.server.URL+`/vibe/sign-in/pid?complete_token=secret","poll_url":"`+
			fake.server.URL+`/api/vibe/sign-in/pid/poll","expires_at":"`+time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339)+`"}`)
	})
	mux.HandleFunc("GET /api/vibe/sign-in/pid/poll", func(w http.ResponseWriter, _ *http.Request) {
		fake.mu.Lock()
		body := fake.pollBody
		fake.mu.Unlock()
		_, _ = io.WriteString(w, body)
		fake.pollOnce.Do(func() { close(fake.polled) })
	})
	mux.HandleFunc("POST /api/vibe/sign-in/pid/exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fake.mu.Lock()
		fake.exchanges++
		fake.mu.Unlock()
		_, _ = io.WriteString(w, `{"api_key":"`+mistralTestAPIKey+`"}`)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	t.Cleanup(mistralauth.OverrideEndpointsForTest(fake.server.URL+"/api", fake.server.URL+"/v1", pollInterval))
	return fake
}

// waitForFirstPoll blocks until the sign-in waiter has made one real poll request.
func (f *mistralSignInFake) waitForFirstPoll(t *testing.T) {
	t.Helper()
	select {
	case <-f.polled:
	case <-time.After(10 * time.Second):
		t.Fatal("sign-in waiter never polled")
	}
}

func (f *mistralSignInFake) setPollBody(body string) {
	f.mu.Lock()
	f.pollBody = body
	f.mu.Unlock()
}

func (f *mistralSignInFake) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// observedMistralService wraps the real client and reports when the sign-in waiter ends.
type observedMistralService struct {
	mistralOAuthService
	waitReturned chan error
}

func (o *observedMistralService) WaitForSignIn(ctx context.Context, signIn *mistralauth.SignIn) (*mistralauth.Credential, error) {
	cred, err := o.mistralOAuthService.WaitForSignIn(ctx, signIn)
	o.waitReturned <- err
	return cred, err
}

func observeMistralWaiter(t *testing.T) chan error {
	t.Helper()
	waitReturned := make(chan error, 1)
	orig := newMistralOAuthService
	newMistralOAuthService = func(cfg *config.Config, proxyURL string) mistralOAuthService {
		return &observedMistralService{mistralOAuthService: orig(cfg, proxyURL), waitReturned: waitReturned}
	}
	t.Cleanup(func() { newMistralOAuthService = orig })
	return waitReturned
}

func newMistralOAuthRouter(authDir string, manager *coreauth.Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Port: 8317}, manager)
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/v8/management/oauth/status", h.GetAuthStatus)
	router.DELETE("/v8/management/oauth/session", h.CancelAuthSession)
	return router
}

func doMistralRequest(t *testing.T, router http.Handler, method, target, body string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec.Code, parsed, rec.Body.String()
}

func jsonFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestStartOAuthV8MistralSignInSavesPrivateAuthFile(t *testing.T) {
	fake := newMistralSignInFake(t, 10*time.Millisecond)
	authDir := t.TempDir()
	router := newMistralOAuthRouter(authDir, coreauth.NewManager(nil, nil, nil))

	code, start, rawStart := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral", "")
	if code != http.StatusOK || start["status"] != "ok" {
		t.Fatalf("auth-url = %d %s", code, rawStart)
	}
	state, _ := start["state"].(string)
	t.Cleanup(func() { CancelOAuthSession(state) })
	if !strings.HasPrefix(state, "mistral-") {
		t.Fatalf("state = %q, want a mistral- prefix", state)
	}
	if want := fake.server.URL + "/vibe/sign-in/pid?complete_token=secret"; start["url"] != want {
		t.Fatalf("url = %v, want %q", start["url"], want)
	}
	if start["flow"] != "device" {
		t.Fatalf("flow = %v, want device", start["flow"])
	}
	if expiresIn, _ := start["expires_in"].(float64); expiresIn <= 0 || expiresIn > 900 {
		t.Fatalf("expires_in = %v, want within (0, 900]", start["expires_in"])
	}
	if _, hasUserCode := start["user_code"]; hasUserCode {
		t.Fatalf("Mistral sign-in has no user code: %s", rawStart)
	}

	// Not approved yet: after at least one real poll the session is still waiting.
	fake.waitForFirstPoll(t)
	_, waiting, rawWaiting := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/status?state="+state, "")
	if waiting["status"] != "wait" {
		t.Fatalf("status before approval = %s, want wait", rawWaiting)
	}
	if files := jsonFilesIn(t, authDir); len(files) != 0 {
		t.Fatalf("auth files before approval: %v", files)
	}

	fake.setPollBody(`{"status":"completed","exchange_token":"exch"}`)
	done := pollOAuthStatusUntilDone(t, router, state)
	if done["status"] != "ok" {
		t.Fatalf("status after approval = %v", done)
	}
	if fake.exchangeCount() != 1 {
		t.Fatalf("exchange calls = %d, want 1", fake.exchangeCount())
	}

	// The session completes only after the save, so the file is already on disk.
	files := jsonFilesIn(t, authDir)
	if len(files) != 1 || !regexp.MustCompile(`^mistral-[0-9a-f]{16}\.json$`).MatchString(files[0]) {
		t.Fatalf("auth files = %v, want exactly one mistral-<hash>.json", files)
	}
	savedPath := filepath.Join(authDir, files[0])
	info, err := os.Stat(savedPath)
	if err != nil {
		t.Fatalf("auth file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(savedPath)
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("auth file is not JSON: %v", err)
	}
	wantFields := map[string]any{"type": "mistral", "auth_kind": "oauth", "api_key": mistralTestAPIKey, "base_url": fake.server.URL + "/v1"}
	for key, want := range wantFields {
		if saved[key] != want {
			t.Fatalf("saved[%q] = %v, want %v", key, saved[key], want)
		}
	}
	if label, _ := saved["label"].(string); !strings.HasPrefix(label, "Mistral (") || strings.Contains(label, mistralTestAPIKey) {
		t.Fatalf("saved label = %q", label)
	}

	// The provisioned key must never be echoed to the management client.
	for _, body := range []string{rawStart, rawWaiting} {
		if strings.Contains(body, mistralTestAPIKey) {
			t.Fatalf("management response leaked the api key: %s", body)
		}
	}
	_, _, rawDone := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/status?state="+state, "")
	if strings.Contains(rawDone, mistralTestAPIKey) {
		t.Fatalf("completed status leaked the api key: %s", rawDone)
	}
}

func TestStartOAuthV8MistralSignInFailuresReportErrorAndWriteNothing(t *testing.T) {
	cases := []struct {
		name     string
		pollBody string
		wantErr  string
	}{
		{name: "denied", pollBody: `{"status":"denied"}`, wantErr: "sign-in was denied"},
		{name: "expired", pollBody: `{"status":"expired"}`, wantErr: "sign-in expired"},
		{name: "provider error", pollBody: `{"status":"error","message":"boom"}`, wantErr: "sign-in failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newMistralSignInFake(t, 10*time.Millisecond)
			fake.setPollBody(tc.pollBody)
			authDir := t.TempDir()
			router := newMistralOAuthRouter(authDir, coreauth.NewManager(nil, nil, nil))

			code, start, rawStart := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral", "")
			if code != http.StatusOK {
				t.Fatalf("auth-url = %d %s", code, rawStart)
			}
			state, _ := start["state"].(string)
			t.Cleanup(func() { CancelOAuthSession(state) })

			done := pollOAuthStatusUntilDone(t, router, state)
			message, _ := done["error"].(string)
			if done["status"] != "error" || !strings.Contains(message, tc.wantErr) {
				t.Fatalf("status = %v, want an error containing %q", done, tc.wantErr)
			}
			if fake.exchangeCount() != 0 {
				t.Fatalf("a failed sign-in must not reach the exchange step, got %d calls", fake.exchangeCount())
			}
			if files := jsonFilesIn(t, authDir); len(files) != 0 {
				t.Fatalf("failed sign-in wrote auth files: %v", files)
			}
		})
	}
}

func TestStartOAuthV8MistralStartFailureReturnsServerError(t *testing.T) {
	fake := newMistralSignInFake(t, 10*time.Millisecond)
	fake.mu.Lock()
	fake.startStatus = http.StatusTooManyRequests
	fake.mu.Unlock()
	authDir := t.TempDir()
	router := newMistralOAuthRouter(authDir, coreauth.NewManager(nil, nil, nil))

	code, _, raw := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral", "")
	if code != http.StatusInternalServerError || !strings.Contains(raw, "failed to start sign-in") {
		t.Fatalf("auth-url = %d %s, want 500 failed to start sign-in", code, raw)
	}
	if strings.Contains(raw, fake.server.URL) {
		t.Fatalf("error body leaked the upstream URL: %s", raw)
	}
	if files := jsonFilesIn(t, authDir); len(files) != 0 {
		t.Fatalf("failed start wrote auth files: %v", files)
	}
}

func TestCancelAuthSessionStopsMistralSignIn(t *testing.T) {
	fake := newMistralSignInFake(t, 10*time.Millisecond)
	waitReturned := observeMistralWaiter(t)
	authDir := t.TempDir()
	router := newMistralOAuthRouter(authDir, coreauth.NewManager(nil, nil, nil))

	code, start, rawStart := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?provider=mistral", "")
	if code != http.StatusOK {
		t.Fatalf("auth-url = %d %s", code, rawStart)
	}
	state, _ := start["state"].(string)
	t.Cleanup(func() { CancelOAuthSession(state) })
	fake.waitForFirstPoll(t)

	code, cancelled, raw := doMistralRequest(t, router, http.MethodDelete, "/v8/management/oauth/session?state="+state, "")
	if code != http.StatusOK || cancelled["status"] != "ok" || cancelled["cancelled"] != true {
		t.Fatalf("cancel = %d %s", code, raw)
	}

	// The session watcher notices the cancellation within a couple of seconds and stops polling.
	select {
	case err := <-waitReturned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter ended with %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sign-in waiter did not stop after the session was cancelled")
	}

	// An approval arriving after the cancel must not produce credentials.
	fake.setPollBody(`{"status":"completed","exchange_token":"exch"}`)
	_, after, rawAfter := doMistralRequest(t, router, http.MethodGet, "/v8/management/oauth/status?state="+state, "")
	if after["status"] == "ok" {
		t.Fatalf("cancelled session reports ok: %s", rawAfter)
	}
	if fake.exchangeCount() != 0 {
		t.Fatalf("cancelled sign-in reached the exchange step %d times", fake.exchangeCount())
	}
	if files := jsonFilesIn(t, authDir); len(files) != 0 {
		t.Fatalf("cancelled sign-in wrote auth files: %v", files)
	}
}

func TestTargetedOAuthReloginMistralReplacesRejectedKeyInPlace(t *testing.T) {
	fake := newMistralSignInFake(t, 10*time.Millisecond)
	fake.setPollBody(`{"status":"completed","exchange_token":"exch"}`)
	authDir := t.TempDir()

	targetFile := "mistral-0123456789abcdef.json"
	targetPath := filepath.Join(authDir, targetFile)
	oldMetadata := map[string]any{"type": "mistral", "auth_kind": "oauth", "api_key": "sk-old-rejected-key", "base_url": fake.server.URL + "/v1"}
	rawOld, _ := json.Marshal(oldMetadata)
	if err := os.WriteFile(targetPath, rawOld, 0o600); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:             targetFile,
		FileName:       targetFile,
		Provider:       "mistral",
		RequiresSignIn: true,
		Metadata:       oldMetadata,
		Attributes:     map[string]string{coreauth.AttributePath: targetPath},
	}); err != nil {
		t.Fatalf("register target: %v", err)
	}
	router := newMistralOAuthRouter(authDir, manager)

	code, start, raw := doMistralRequest(t, router, http.MethodPost, "/v8/management/oauth/auth-url", `{"provider":"mistral","name":"`+targetFile+`"}`)
	if code != http.StatusOK || start["target_auth_id"] != targetFile {
		t.Fatalf("targeted auth-url = %d %s", code, raw)
	}
	state, _ := start["state"].(string)
	t.Cleanup(func() { CancelOAuthSession(state) })

	done := pollOAuthStatusUntilDone(t, router, state)
	if done["status"] != "ok" || done["auth_id"] != targetFile || done["saved"] != true {
		t.Fatalf("targeted status = %v", done)
	}
	if files := jsonFilesIn(t, authDir); len(files) != 1 || files[0] != targetFile {
		t.Fatalf("auth files = %v, want only %s", files, targetFile)
	}
	rawSaved, _ := os.ReadFile(targetPath)
	if !strings.Contains(string(rawSaved), mistralTestAPIKey) || strings.Contains(string(rawSaved), "sk-old-rejected-key") {
		t.Fatalf("target file was not updated in place: %s", rawSaved)
	}
}
