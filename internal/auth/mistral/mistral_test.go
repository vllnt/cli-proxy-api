package mistral

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
)

type reply struct {
	status int
	body   string
}

// fakeMistral serves the sign-in endpoints. Poll replies are consumed in order and
// the last one repeats.
type fakeMistral struct {
	srv *httptest.Server

	mu            sync.Mutex
	startBody     []byte
	exchangeBody  []byte
	exchangePath  string
	polls         int
	pollHeaders   http.Header
	pollScript    []reply
	exchangeReply reply
	// startPayload overrides the start response; it receives the fake server URL.
	startPayload func(base string) string
	pollSeen     chan struct{}
	pollSeenOnce sync.Once
}

func newFakeMistral(t *testing.T) *fakeMistral {
	t.Helper()
	f := &fakeMistral{
		pollScript:    []reply{{200, `{"status":"completed","exchange_token":"exch-1"}`}},
		exchangeReply: reply{200, `{"api_key":"key-123"}`},
		pollSeen:      make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vibe/sign-in", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.startBody = body
		payload := f.startPayload
		f.mu.Unlock()
		if payload == nil {
			payload = func(base string) string {
				return `{"process_id":"pid-1","sign_in_url":"` + base + `/vibe/sign-in/pid-1?complete_token=tok",` +
					`"poll_url":"` + base + `/api/vibe/sign-in/pid-1/poll","expires_at":"` +
					time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`
			}
		}
		writeReply(w, reply{200, payload(f.srv.URL)})
	})
	mux.HandleFunc("GET /api/vibe/sign-in/pid-1/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		idx := f.polls
		f.polls++
		f.pollHeaders = r.Header.Clone()
		script := f.pollScript
		f.mu.Unlock()
		f.pollSeenOnce.Do(func() { close(f.pollSeen) })
		if idx >= len(script) {
			idx = len(script) - 1
		}
		writeReply(w, script[idx])
	})
	mux.HandleFunc("POST /api/vibe/sign-in/{id}/exchange", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.exchangeBody = body
		f.exchangePath = r.URL.Path
		rep := f.exchangeReply
		f.mu.Unlock()
		writeReply(w, rep)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeReply(w http.ResponseWriter, r reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status)
	_, _ = io.WriteString(w, r.body)
}

func (f *fakeMistral) auth() *MistralAuth {
	a := NewMistralAuth(nil)
	a.signInBase = f.srv.URL + "/api"
	a.apiBase = f.srv.URL + "/v1"
	a.pollInterval = time.Millisecond
	return a
}

func (f *fakeMistral) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeMistral) start() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startBody
}

func (f *fakeMistral) exchange() (body []byte, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchangeBody, f.exchangePath
}

func (f *fakeMistral) lastPollHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pollHeaders
}

func signInOrFatal(t *testing.T, a *MistralAuth) *SignIn {
	t.Helper()
	signIn, err := a.StartSignIn(context.Background())
	if err != nil {
		t.Fatalf("StartSignIn: %v", err)
	}
	return signIn
}

func TestMistralSignIn_SendsS256ChallengeAndVerifierOnExchange(t *testing.T) {
	f := newFakeMistral(t)
	a := f.auth()

	signIn := signInOrFatal(t, a)
	if signIn.ProcessID != "pid-1" || !strings.Contains(signIn.URL, "complete_token=tok") {
		t.Fatalf("unexpected sign-in: %+v", signIn)
	}
	if _, err := a.WaitForSignIn(context.Background(), signIn); err != nil {
		t.Fatalf("WaitForSignIn: %v", err)
	}

	startBody := f.start()
	exchangeBody, exchangePath := f.exchange()
	challenge := ojson.GetBytes(startBody, "code_challenge").String()
	if method := ojson.GetBytes(startBody, "code_challenge_method").String(); method != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", method)
	}
	verifier := ojson.GetBytes(exchangeBody, "code_verifier").String()
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Fatalf("challenge %q is not S256(verifier) %q", challenge, want)
	}
	if len(verifier) < 43 || len(verifier) > 128 || strings.ContainsAny(verifier, "=+/") {
		t.Fatalf("verifier %q is not RFC 7636 base64url", verifier)
	}
	if got := ojson.GetBytes(exchangeBody, "exchange_token").String(); got != "exch-1" {
		t.Fatalf("exchange_token = %q, want exch-1", got)
	}
	if exchangePath != "/api/vibe/sign-in/pid-1/exchange" {
		t.Fatalf("exchange path = %q", exchangePath)
	}
}

func TestMistralSignIn_SuccessAfterPendingPolls(t *testing.T) {
	f := newFakeMistral(t)
	f.pollScript = []reply{
		{200, `{"status":"pending"}`},
		{200, `{"status":"pending"}`},
		{200, `{"status":"completed","exchange_token":"exch-1"}`},
	}
	a := f.auth()

	cred, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
	if err != nil {
		t.Fatalf("WaitForSignIn: %v", err)
	}
	if cred.APIKey != "key-123" || cred.BaseURL != f.srv.URL+"/v1" {
		t.Fatalf("credential = %+v", cred)
	}
	if got := f.pollCount(); got != 3 {
		t.Fatalf("poll count = %d, want 3", got)
	}
	headers := f.lastPollHeaders()
	if ua := headers.Get("User-Agent"); ua != userAgent {
		t.Fatalf("User-Agent = %q, want %q", ua, userAgent)
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("poll must be unauthenticated")
	}
}

func TestMistralSignIn_TerminalPollOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		reply   reply
		want    error
		contain string
	}{
		{"http 410 is expired", reply{410, ``}, ErrExpired, ""},
		{"expired status", reply{200, `{"status":"expired"}`}, ErrExpired, ""},
		{"denied status", reply{200, `{"status":"denied"}`}, ErrDenied, ""},
		{"error status carries message", reply{200, `{"status":"error","message":"boom\nline"}`}, ErrProviderError, "boom line"},
		{"unknown status", reply{200, `{"status":"weird"}`}, ErrUnknownStatus, ""},
		{"completed without exchange token", reply{200, `{"status":"completed"}`}, ErrMissingExchangeToken, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMistral(t)
			f.pollScript = []reply{tc.reply}
			a := f.auth()

			_, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.contain != "" && !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.contain)
			}
			if f.pollCount() != 1 {
				t.Fatalf("terminal outcome must not be retried, polls = %d", f.pollCount())
			}
			if body, _ := f.exchange(); body != nil {
				t.Fatal("exchange must not run after a terminal poll outcome")
			}
		})
	}
}

func TestMistralSignIn_TransientPollFailures(t *testing.T) {
	t.Run("recovers within three consecutive failures", func(t *testing.T) {
		f := newFakeMistral(t)
		f.pollScript = []reply{
			{500, `oops`}, {200, `not json`}, {200, `{"status":"pending"}`}, {500, ``}, {500, ``},
			{200, `{"status":"completed","exchange_token":"exch-1"}`},
		}
		a := f.auth()
		if _, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a)); err != nil {
			t.Fatalf("WaitForSignIn: %v", err)
		}
	})
	t.Run("gives up after three consecutive failures", func(t *testing.T) {
		f := newFakeMistral(t)
		f.pollScript = []reply{{500, ``}}
		a := f.auth()
		_, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("err = %v, want HTTP 500 failure", err)
		}
		if got := f.pollCount(); got != maxConsecutivePollFailures {
			t.Fatalf("poll count = %d, want %d", got, maxConsecutivePollFailures)
		}
	})
}

func TestMistralSignIn_ExchangeFailures(t *testing.T) {
	t.Run("workspace not eligible", func(t *testing.T) {
		f := newFakeMistral(t)
		f.exchangeReply = reply{403, `{"error_code":9004,"message":"nope"}`}
		a := f.auth()
		_, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
		if !errors.Is(err, ErrWorkspaceNotEligible) || !strings.Contains(err.Error(), WorkspaceNotEligibleMessage) {
			t.Fatalf("err = %v, want workspace message", err)
		}
	})
	t.Run("other 403 is a generic failure", func(t *testing.T) {
		f := newFakeMistral(t)
		f.exchangeReply = reply{403, `{"error_code":1234,"message":"denied here"}`}
		a := f.auth()
		_, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
		if err == nil || errors.Is(err, ErrWorkspaceNotEligible) || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("err = %v, want generic HTTP 403 failure", err)
		}
	})
	t.Run("missing api_key", func(t *testing.T) {
		f := newFakeMistral(t)
		f.exchangeReply = reply{200, `{"api_key":"  "}`}
		a := f.auth()
		_, err := a.WaitForSignIn(context.Background(), signInOrFatal(t, a))
		if !errors.Is(err, ErrMissingAPIKey) {
			t.Fatalf("err = %v, want ErrMissingAPIKey", err)
		}
	})
	t.Run("exchange redirect is not followed", func(t *testing.T) {
		var leaked atomic.Bool
		foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
		defer foreign.Close()
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/steal", http.StatusTemporaryRedirect)
		}))
		defer redirecting.Close()

		a := NewMistralAuth(nil)
		a.signInBase = redirecting.URL + "/api"
		signIn := &SignIn{ProcessID: "pid-1", PollURL: redirecting.URL + "/api/p", Lifetime: time.Minute, codeVerifier: "v"}
		_, err := a.exchange(context.Background(), signIn, "exch-1")
		if err == nil || !strings.Contains(err.Error(), "HTTP 307") || leaked.Load() {
			t.Fatalf("err = %v, leaked = %v; redirect must fail without reaching the foreign host", err, leaked.Load())
		}
	})
}

func TestMistralSignIn_RejectsUntrustedURLs(t *testing.T) {
	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()

	tests := []struct {
		name    string
		payload func(base string) string
	}{
		{"foreign origin sign_in_url", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + foreign.URL + `/x","poll_url":"` + base + `/api/vibe/sign-in/p/poll","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"foreign origin poll_url", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x","poll_url":"` + foreign.URL + `/api/vibe/sign-in/p/poll","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"poll_url outside the sign-in path", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x","poll_url":"` + base + `/other/poll","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"poll_url escaping with dot segments", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x","poll_url":"` + base + `/api/../admin","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"poll_url escaping with encoded dot segments", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x","poll_url":"` + base + `/api/%2e%2e/admin","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"poll_url sibling prefix", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x","poll_url":"` + base + `/apix/poll","expires_at":"2099-01-01T00:00:00Z"}`
		}},
		{"scheme downgrade", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + strings.Replace(base, "http://", "https://", 1) + `/x","poll_url":"` + base + `/api/p","expires_at":"2099-01-01T00:00:00Z"}`
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMistral(t)
			f.startPayload = tc.payload
			_, err := f.auth().StartSignIn(context.Background())
			if !errors.Is(err, ErrUntrustedURL) {
				t.Fatalf("err = %v, want ErrUntrustedURL", err)
			}
		})
	}

	t.Run("sign_in_url on the console origin outside /api is accepted", func(t *testing.T) {
		f := newFakeMistral(t)
		signIn := signInOrFatal(t, f.auth())
		if !strings.HasPrefix(signIn.URL, f.srv.URL+"/vibe/sign-in/") {
			t.Fatalf("sign-in URL = %q", signIn.URL)
		}
	})
	t.Run("poll URL is revalidated before polling", func(t *testing.T) {
		f := newFakeMistral(t)
		a := f.auth()
		signIn := signInOrFatal(t, a)
		signIn.PollURL = foreign.URL + "/poll"
		if _, err := a.WaitForSignIn(context.Background(), signIn); !errors.Is(err, ErrUntrustedURL) {
			t.Fatalf("err = %v, want ErrUntrustedURL", err)
		}
	})
}

func TestMistralSignIn_StartFailures(t *testing.T) {
	tests := []struct {
		name    string
		payload func(base string) string
	}{
		{"missing process_id", func(base string) string {
			return `{"sign_in_url":"` + base + `/x","poll_url":"` + base + `/api/p"}`
		}},
		{"missing poll_url", func(base string) string {
			return `{"process_id":"p","sign_in_url":"` + base + `/x"}`
		}},
		{"not an object", func(string) string { return `[]` }},
		{"not json", func(string) string { return `<html>` }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMistral(t)
			f.startPayload = tc.payload
			if _, err := f.auth().StartSignIn(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestMistralSignIn_ContextCancelStopsPolling(t *testing.T) {
	f := newFakeMistral(t)
	f.pollScript = []reply{{200, `{"status":"pending"}`}}
	a := f.auth()
	signIn := signInOrFatal(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.WaitForSignIn(ctx, signIn)
		done <- err
	}()
	<-f.pollSeen
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForSignIn did not stop after cancel")
	}
	if body, _ := f.exchange(); body != nil {
		t.Fatal("exchange must not run after cancel")
	}
}

func TestMistralSignIn_LifetimeEndsPolling(t *testing.T) {
	f := newFakeMistral(t)
	f.pollScript = []reply{{200, `{"status":"pending"}`}}
	f.startPayload = func(base string) string {
		return `{"process_id":"pid-1","sign_in_url":"` + base + `/x","poll_url":"` + base + `/api/vibe/sign-in/pid-1/poll","expires_at":"` +
			time.Now().Add(100*time.Millisecond).UTC().Format(time.RFC3339Nano) + `"}`
	}
	a := f.auth()
	signIn := signInOrFatal(t, a)
	if signIn.Lifetime <= 0 || signIn.Lifetime > time.Second {
		t.Fatalf("lifetime = %v, want about 100ms", signIn.Lifetime)
	}
	if _, err := a.WaitForSignIn(context.Background(), signIn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestLifetimeFromExpiry(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt string
		want      time.Duration
	}{
		{"within range", "2026-11-21T12:10:00Z", 10 * time.Minute},
		{"offset timezone", "2026-11-21T14:05:00+02:00", 5 * time.Minute},
		{"already past (clock skew)", "2026-11-21T11:00:00Z", MaxSignInDuration},
		{"too far away", "2027-01-01T00:00:00Z", MaxSignInDuration},
		{"malformed", "tomorrow", MaxSignInDuration},
		{"empty", "", MaxSignInDuration},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lifetimeFromExpiry(tc.expiresAt, now); got != tc.want {
				t.Fatalf("lifetime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCredentialMetadataKeepsSecretOutOfNames(t *testing.T) {
	cred := &Credential{APIKey: "sk-secret-value"}
	name := CredentialFileName(cred.APIKey)
	label := CredentialLabel(cred.APIKey)
	if strings.Contains(name, "secret") || strings.Contains(label, "secret") {
		t.Fatalf("name %q or label %q leaks the key", name, label)
	}
	if name != CredentialFileName(" sk-secret-value ") || name == CredentialFileName("sk-other") {
		t.Fatalf("file name %q must be stable per key and distinct across keys", name)
	}
	if !strings.HasPrefix(label, "Mistral (") {
		t.Fatalf("label = %q", label)
	}

	meta := cred.Metadata()
	want := map[string]any{
		"type":      "mistral",
		"auth_kind": "oauth",
		"api_key":   "sk-secret-value",
		"base_url":  DefaultAPIBaseURL,
		"label":     label,
	}
	if len(meta) != len(want) {
		t.Fatalf("metadata = %v, want %v", meta, want)
	}
	for k, v := range want {
		if meta[k] != v {
			t.Fatalf("metadata[%q] = %v, want %v", k, meta[k], v)
		}
	}
	if custom := (&Credential{APIKey: "k", BaseURL: "http://x/v1"}).Metadata(); custom["base_url"] != "http://x/v1" {
		t.Fatalf("custom base_url lost: %v", custom)
	}
}

func TestOverrideEndpointsForTest(t *testing.T) {
	restore := OverrideEndpointsForTest("http://sign.example/api/", "http://api.example/v1", 7*time.Millisecond)
	a := NewMistralAuth(nil)
	if a.signInBase != "http://sign.example/api" || a.apiBase != "http://api.example/v1" || a.pollInterval != 7*time.Millisecond {
		t.Fatalf("override not applied: %+v", a)
	}
	restore()
	b := NewMistralAuth(nil)
	if b.signInBase != DefaultSignInBaseURL || b.apiBase != DefaultAPIBaseURL || b.pollInterval != DefaultPollInterval {
		t.Fatalf("defaults not restored: %+v", b)
	}
}
