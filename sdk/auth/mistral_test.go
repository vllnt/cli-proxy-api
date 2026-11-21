package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// newMistralSignInServer fakes the Mistral sign-in API. pollStatus is the poll body.
func newMistralSignInServer(t *testing.T, pollBody string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vibe/sign-in", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"process_id":"pid","sign_in_url":"`+server.URL+`/vibe/sign-in/pid?complete_token=t","poll_url":"`+
			server.URL+`/api/vibe/sign-in/pid/poll","expires_at":"`+time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339)+`"}`)
	})
	mux.HandleFunc("GET /api/vibe/sign-in/pid/poll", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, pollBody)
	})
	mux.HandleFunc("POST /api/vibe/sign-in/pid/exchange", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"api_key":"sk-login-test-key"}`)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Cleanup(mistralauth.OverrideEndpointsForTest(server.URL+"/api", server.URL+"/v1", time.Millisecond))
	return server
}

func TestMistralLoginReturnsRecordWithKeyOnlyInMetadata(t *testing.T) {
	server := newMistralSignInServer(t, `{"status":"completed","exchange_token":"exch"}`)

	record, err := NewMistralAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if record.Provider != "mistral" || record.Storage != nil {
		t.Fatalf("record provider/storage = %q/%v", record.Provider, record.Storage)
	}
	if !regexp.MustCompile(`^mistral-[0-9a-f]{16}\.json$`).MatchString(record.FileName) || record.ID != record.FileName {
		t.Fatalf("record id/file = %q/%q", record.ID, record.FileName)
	}
	if !strings.HasPrefix(record.Label, "Mistral (") || strings.Contains(record.Label, "sk-login") {
		t.Fatalf("label = %q", record.Label)
	}
	want := map[string]any{"type": "mistral", "auth_kind": "oauth", "api_key": "sk-login-test-key", "base_url": server.URL + "/v1", "label": record.Label}
	for key, value := range want {
		if record.Metadata[key] != value {
			t.Fatalf("metadata[%q] = %v, want %v", key, record.Metadata[key], value)
		}
	}
	if len(record.Attributes) != 0 {
		t.Fatalf("attributes must stay empty so the raw key never reaches usage records: %v", record.Attributes)
	}
	if kind, value := record.AccountInfo(); kind != "oauth" || value != "" {
		t.Fatalf("AccountInfo = (%q, %q), want (oauth, empty)", kind, value)
	}
}

func TestMistralLoginViaManagerWritesPrivateAuthFile(t *testing.T) {
	server := newMistralSignInServer(t, `{"status":"completed","exchange_token":"exch"}`)
	authDir := t.TempDir()
	cfg := &config.Config{AuthDir: authDir}
	manager := NewManager(NewFileTokenStore(), NewMistralAuthenticator())

	record, savedPath, err := manager.Login(context.Background(), "mistral", cfg, &LoginOptions{NoBrowser: true})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if savedPath != filepath.Join(authDir, record.FileName) {
		t.Fatalf("saved path = %q, want it inside %q", savedPath, authDir)
	}
	info, errStat := os.Stat(savedPath)
	if errStat != nil {
		t.Fatalf("auth file missing: %v", errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(savedPath)
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
		t.Fatalf("auth file is not JSON: %v", errUnmarshal)
	}
	if saved["type"] != "mistral" || saved["api_key"] != "sk-login-test-key" || saved["base_url"] != server.URL+"/v1" || saved["auth_kind"] != "oauth" {
		t.Fatalf("saved file = %v", saved)
	}
	if _, hasExpiry := saved["expired"]; hasExpiry {
		t.Fatalf("a key that never expires must not carry an expiry: %v", saved)
	}

	// The file must round-trip through the store the server uses at startup.
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	auths, errList := store.List(context.Background())
	if errList != nil || len(auths) != 1 || auths[0].Provider != "mistral" || auths[0].Metadata["api_key"] != "sk-login-test-key" {
		t.Fatalf("store.List = %v, %v", auths, errList)
	}
}

func TestMistralLoginDeniedSavesNothing(t *testing.T) {
	newMistralSignInServer(t, `{"status":"denied"}`)
	authDir := t.TempDir()
	manager := NewManager(NewFileTokenStore(), NewMistralAuthenticator())

	_, savedPath, err := manager.Login(context.Background(), "mistral", &config.Config{AuthDir: authDir}, &LoginOptions{NoBrowser: true})
	if err == nil || !strings.Contains(err.Error(), "denied") || savedPath != "" {
		t.Fatalf("err = %v, savedPath = %q; want a denied error and no saved path", err, savedPath)
	}
	if entries, _ := os.ReadDir(authDir); len(entries) != 0 {
		t.Fatalf("denied sign-in must not write files, found %d", len(entries))
	}
}

func TestMistralLoginRequiresConfig(t *testing.T) {
	if _, err := NewMistralAuthenticator().Login(context.Background(), nil, nil); err == nil {
		t.Fatal("expected an error for a nil config")
	}
}
