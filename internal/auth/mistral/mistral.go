// Package mistral implements the Mistral Vibe browser sign-in used by Le Chat Pro
// and Vibe plans. The flow is PKCE based: the client starts a sign-in process,
// the user approves it in a browser, and the client polls until it can exchange
// the approval for a long-lived Mistral API key. The key never expires and has
// no refresh token.
package mistral

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultSignInBaseURL is the unauthenticated Mistral console API that runs the sign-in flow.
	DefaultSignInBaseURL = "https://console.mistral.ai/api"
	// DefaultAPIBaseURL is the OpenAI-compatible Mistral inference API.
	DefaultAPIBaseURL = "https://api.mistral.ai/v1"
	// DefaultPollInterval is how often the sign-in status is polled.
	DefaultPollInterval = 3 * time.Second
	// MaxSignInDuration bounds the wait for browser approval when the server
	// reports no usable expiry. Mistral sign-in processes live about ten minutes.
	MaxSignInDuration = 15 * time.Minute

	// WorkspaceNotEligibleMessage is shown when the chosen workspace cannot create Vibe keys.
	WorkspaceNotEligibleMessage = "Your current workspace can't be used for Mistral Vibe. " +
		"Sign in again and select another workspace in the browser, or contact your administrator."

	// httpClientTimeout bounds each credential-acquisition HTTP call.
	httpClientTimeout = 30 * time.Second
	// maxConsecutivePollFailures matches Vibe: transient poll failures are retried this many times.
	maxConsecutivePollFailures = 3
	// maxResponseBytes caps every response body read from the sign-in API.
	maxResponseBytes = 1 << 20
	// maxDetailLength caps server-provided text echoed into errors.
	maxDetailLength = 200
	// codeVerifierBytes matches Vibe's secrets.token_urlsafe(64).
	codeVerifierBytes = 64
	// workspaceNotEligibleCode is the exchange error_code for ineligible workspaces.
	workspaceNotEligibleCode = 9004
	// userAgent is a neutral client identifier; the sign-in API needs no vendor identity.
	userAgent = "cli-proxy-api"
)

// Sign-in outcomes callers can match with errors.Is.
var (
	ErrExpired              = errors.New("sign-in expired")
	ErrDenied               = errors.New("sign-in was denied")
	ErrProviderError        = errors.New("sign-in failed")
	ErrUnknownStatus        = errors.New("sign-in returned an unknown status")
	ErrMissingExchangeToken = errors.New("sign-in completed without an exchange token")
	ErrMissingAPIKey        = errors.New("exchange did not return an API key")
	ErrWorkspaceNotEligible = errors.New(WorkspaceNotEligibleMessage)
	ErrUntrustedURL         = errors.New("sign-in URL does not belong to the Mistral sign-in service")
)

// endpoints holds the process-wide sign-in settings. They exist so tests in other
// packages can point the real client at a fake server; production never changes them.
var (
	endpointMu    sync.RWMutex
	signInBaseURL = DefaultSignInBaseURL
	apiBaseURL    = DefaultAPIBaseURL
	pollInterval  = DefaultPollInterval
)

// OverrideEndpointsForTest points sign-in at a fake server, sets the inference base URL
// written into new credentials, and shortens the poll interval. It returns a restore function.
func OverrideEndpointsForTest(signIn, api string, interval time.Duration) func() {
	endpointMu.Lock()
	prevSignIn, prevAPI, prevInterval := signInBaseURL, apiBaseURL, pollInterval
	signInBaseURL, apiBaseURL, pollInterval = signIn, api, interval
	endpointMu.Unlock()
	return func() {
		endpointMu.Lock()
		signInBaseURL, apiBaseURL, pollInterval = prevSignIn, prevAPI, prevInterval
		endpointMu.Unlock()
	}
}

// SignIn is a started browser sign-in. Show URL to the user, then pass it to WaitForSignIn.
type SignIn struct {
	// ProcessID identifies the server-side sign-in process.
	ProcessID string
	// URL is the page the user opens to approve the sign-in. It is a bearer-like
	// link and must not be logged.
	URL string
	// PollURL is the status endpoint polled until approval.
	PollURL string
	// Lifetime is how long the user has to approve, clamped to a sane range.
	Lifetime time.Duration

	codeVerifier string
}

// Credential is the API key a completed sign-in provisions.
type Credential struct {
	APIKey  string
	BaseURL string
}

// MistralAuth runs the Mistral browser sign-in.
type MistralAuth struct {
	httpClient   *http.Client
	signInBase   string
	apiBase      string
	pollInterval time.Duration
}

// NewMistralAuth creates a MistralAuth that uses the configured proxy.
func NewMistralAuth(cfg *config.Config) *MistralAuth {
	return NewMistralAuthWithProxyURL(cfg, "")
}

// NewMistralAuthWithProxyURL creates a MistralAuth with an optional per-call proxy URL.
func NewMistralAuthWithProxyURL(cfg *config.Config, proxyURL string) *MistralAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL

	client := &http.Client{
		Timeout: httpClientTimeout,
		// Never follow redirects: the exchange request carries the PKCE verifier.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	endpointMu.RLock()
	defer endpointMu.RUnlock()
	return &MistralAuth{
		httpClient:   util.SetProxy(&sdkCfg, client),
		signInBase:   strings.TrimRight(strings.TrimSpace(signInBaseURL), "/"),
		apiBase:      strings.TrimSpace(apiBaseURL),
		pollInterval: pollInterval,
	}
}

// StartSignIn creates a sign-in process and returns the URL the user must open.
func (a *MistralAuth) StartSignIn(ctx context.Context) (*SignIn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	verifier, err := newCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: %w", err)
	}

	doc := ojson.NewDocString("{}")
	if err = doc.Set("code_challenge", codeChallenge(verifier)); err != nil {
		return nil, fmt.Errorf("mistral sign-in: encode request: %w", err)
	}
	if err = doc.Set("code_challenge_method", "S256"); err != nil {
		return nil, fmt.Errorf("mistral sign-in: encode request: %w", err)
	}

	status, body, err := a.doJSON(ctx, http.MethodPost, a.signInBase+"/vibe/sign-in", doc.Bytes())
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: start request failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mistral sign-in: start failed (HTTP %d)%s", status, detailSuffix(body))
	}
	payload, err := parseObject(body)
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: parse start response: %w", err)
	}

	processID := strings.TrimSpace(payload.Get("process_id").String())
	signInURL := strings.TrimSpace(payload.Get("sign_in_url").String())
	pollURL := strings.TrimSpace(payload.Get("poll_url").String())
	if processID == "" || signInURL == "" || pollURL == "" {
		return nil, errors.New("mistral sign-in: start response missing process_id, sign_in_url or poll_url")
	}
	if err = validateURL(signInURL, a.consoleBase(), ""); err != nil {
		return nil, fmt.Errorf("mistral sign-in: sign_in_url: %w", err)
	}
	if err = validateURL(pollURL, a.signInBase, a.signInBase); err != nil {
		return nil, fmt.Errorf("mistral sign-in: poll_url: %w", err)
	}

	return &SignIn{
		ProcessID:    processID,
		URL:          signInURL,
		PollURL:      pollURL,
		Lifetime:     lifetimeFromExpiry(payload.Get("expires_at").String(), time.Now()),
		codeVerifier: verifier,
	}, nil
}

// WaitForSignIn polls until the user approves, then exchanges the approval for an API key.
// It stops when ctx is canceled or the sign-in lifetime ends.
func (a *MistralAuth) WaitForSignIn(ctx context.Context, signIn *SignIn) (*Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if signIn == nil || signIn.codeVerifier == "" || strings.TrimSpace(signIn.ProcessID) == "" {
		return nil, errors.New("mistral sign-in: missing started sign-in")
	}
	if err := validateURL(signIn.PollURL, a.signInBase, a.signInBase); err != nil {
		return nil, fmt.Errorf("mistral sign-in: poll_url: %w", err)
	}

	lifetime := signIn.Lifetime
	if lifetime <= 0 || lifetime > MaxSignInDuration {
		lifetime = MaxSignInDuration
	}
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()

	timer := time.NewTimer(a.pollInterval)
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("mistral sign-in: timed out or canceled: %w", ctx.Err())
		case <-timer.C:
		}

		exchangeToken, err := a.pollOnce(ctx, signIn.PollURL)
		switch {
		case err == nil && exchangeToken != "":
			return a.exchange(ctx, signIn, exchangeToken)
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			return nil, fmt.Errorf("mistral sign-in: timed out or canceled: %w", ctx.Err())
		case isTerminal(err):
			return nil, err
		default:
			failures++
			if failures >= maxConsecutivePollFailures {
				return nil, fmt.Errorf("mistral sign-in: status check failed %d times in a row: %w", failures, err)
			}
			log.Warnf("mistral sign-in: status check failed (%d/%d), retrying: %v", failures, maxConsecutivePollFailures, err)
		}
		timer.Reset(a.pollInterval)
	}
}

// pollOnce checks the sign-in status. It returns a non-empty exchange token when the
// user approved, an empty token when still pending, and a terminal error for any
// other final outcome.
func (a *MistralAuth) pollOnce(ctx context.Context, pollURL string) (string, error) {
	status, body, err := a.doJSON(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusGone {
		return "", fmt.Errorf("mistral sign-in: %w", ErrExpired)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("status check returned HTTP %d", status)
	}
	payload, err := parseObject(body)
	if err != nil {
		return "", fmt.Errorf("parse status response: %w", err)
	}

	switch state := payload.Get("status").String(); state {
	case "pending":
		return "", nil
	case "completed":
		token := strings.TrimSpace(payload.Get("exchange_token").String())
		if token == "" {
			return "", fmt.Errorf("mistral sign-in: %w", ErrMissingExchangeToken)
		}
		return token, nil
	case "expired":
		return "", fmt.Errorf("mistral sign-in: %w", ErrExpired)
	case "denied":
		return "", fmt.Errorf("mistral sign-in: %w", ErrDenied)
	case "error":
		return "", fmt.Errorf("mistral sign-in: %w%s", ErrProviderError, messageSuffix(payload.Get("message").String()))
	default:
		return "", fmt.Errorf("mistral sign-in: %w", ErrUnknownStatus)
	}
}

// exchange trades the approved sign-in for an API key.
func (a *MistralAuth) exchange(ctx context.Context, signIn *SignIn, exchangeToken string) (*Credential, error) {
	doc := ojson.NewDocString("{}")
	if err := doc.Set("exchange_token", exchangeToken); err != nil {
		return nil, fmt.Errorf("mistral sign-in: encode exchange request: %w", err)
	}
	if err := doc.Set("code_verifier", signIn.codeVerifier); err != nil {
		return nil, fmt.Errorf("mistral sign-in: encode exchange request: %w", err)
	}

	endpoint := a.signInBase + "/vibe/sign-in/" + url.PathEscape(signIn.ProcessID) + "/exchange"
	status, body, err := a.doJSON(ctx, http.MethodPost, endpoint, doc.Bytes())
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: exchange request failed: %w", err)
	}
	if status == http.StatusForbidden && ojson.GetBytes(body, "error_code").Int() == workspaceNotEligibleCode {
		return nil, fmt.Errorf("mistral sign-in: %w", ErrWorkspaceNotEligible)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mistral sign-in: exchange failed (HTTP %d)%s", status, detailSuffix(body))
	}
	payload, err := parseObject(body)
	if err != nil {
		return nil, fmt.Errorf("mistral sign-in: parse exchange response: %w", err)
	}
	apiKey := strings.TrimSpace(payload.Get("api_key").String())
	if apiKey == "" {
		return nil, fmt.Errorf("mistral sign-in: %w", ErrMissingAPIKey)
	}
	return &Credential{APIKey: apiKey, BaseURL: a.apiBase}, nil
}

// doJSON sends one request and returns the status and a size-capped body.
func (a *MistralAuth) doJSON(ctx context.Context, method, target string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", stripURL(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, nil, stripURL(err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("mistral sign-in: close response body error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// consoleBase is the origin of the sign-in API. The browser page lives on the same
// origin but outside the /api path.
func (a *MistralAuth) consoleBase() string {
	parsed, err := url.Parse(a.signInBase)
	if err != nil || parsed.Host == "" {
		return a.signInBase
	}
	return parsed.Scheme + "://" + parsed.Host
}

// validateURL accepts target only when it shares the origin of base and, when
// pathBase is set, lives under that base path.
func validateURL(target, base, pathBase string) error {
	got, err := url.Parse(target)
	if err != nil {
		return ErrUntrustedURL
	}
	want, err := url.Parse(base)
	if err != nil {
		return ErrUntrustedURL
	}
	if !sameOrigin(got, want) {
		return ErrUntrustedURL
	}
	if pathBase == "" {
		return nil
	}
	basePath := normalizePath(want.Path)
	gotPath := normalizePath(got.Path)
	if basePath == "/" || gotPath == basePath || strings.HasPrefix(gotPath, basePath+"/") {
		return nil
	}
	return ErrUntrustedURL
}

func sameOrigin(a, b *url.URL) bool {
	if a.Hostname() == "" || b.Hostname() == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// normalizePath decodes and cleans a URL path so "..", "//" and encoded dots cannot
// escape the base path.
func normalizePath(p string) string {
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	if p == "" {
		return "/"
	}
	return path.Clean("/" + p)
}

// lifetimeFromExpiry converts the server expiry into a wait budget. A missing,
// malformed, past or too-distant expiry (for example from clock skew) falls back to
// MaxSignInDuration; the server still ends the sign-in with an expired status.
func lifetimeFromExpiry(expiresAt string, now time.Time) time.Duration {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt))
	if err != nil {
		return MaxSignInDuration
	}
	if lifetime := parsed.Sub(now); lifetime > 0 && lifetime <= MaxSignInDuration {
		return lifetime
	}
	return MaxSignInDuration
}

// newCodeVerifier returns a PKCE verifier of 64 random bytes, base64url without padding.
func newCodeVerifier() (string, error) {
	buf := make([]byte, codeVerifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// codeChallenge returns the S256 PKCE challenge for verifier.
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func parseObject(body []byte) (ojson.Result, error) {
	if !ojson.ValidBytes(body) {
		return ojson.Result{}, errors.New("response is not valid JSON")
	}
	parsed := ojson.ParseBytes(body)
	if !parsed.IsObject() {
		return ojson.Result{}, errors.New("response is not a JSON object")
	}
	return parsed, nil
}

// isTerminal reports whether err is a final sign-in outcome that must not be retried.
func isTerminal(err error) bool {
	for _, target := range []error{
		ErrExpired, ErrDenied, ErrProviderError, ErrUnknownStatus, ErrMissingExchangeToken, ErrUntrustedURL,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// stripURL removes the request URL from transport errors so URLs never reach logs.
func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// detailSuffix returns ": <server detail>" for the first textual detail field, or "".
func detailSuffix(body []byte) string {
	if !ojson.ValidBytes(body) {
		return ""
	}
	for _, key := range []string{"detail", "message", "error"} {
		if value := ojson.GetBytes(body, key); value.Type == ojson.String {
			return messageSuffix(value.String())
		}
	}
	return ""
}

func messageSuffix(message string) string {
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(message))
	if message == "" {
		return ""
	}
	if runes := []rune(message); len(runes) > maxDetailLength {
		message = string(runes[:maxDetailLength-3]) + "..."
	}
	return ": " + message
}

// CredentialFileName derives a stable file name from the API key hash, so signing in
// again with the same key overwrites its file and different keys never collide. The
// key itself never appears in the name.
func CredentialFileName(apiKey string) string {
	return fmt.Sprintf("mistral-%s.json", keyFingerprint(apiKey))
}

// CredentialLabel returns a display name with a short non-secret suffix.
func CredentialLabel(apiKey string) string {
	return fmt.Sprintf("Mistral (%s)", keyFingerprint(apiKey)[:6])
}

func keyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:8])
}

// Metadata returns the auth file content for this credential. The key is stored as an
// "oauth" account credential: it comes from an account sign-in, never expires and has
// no refresh token.
func (c *Credential) Metadata() map[string]any {
	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	return map[string]any{
		"type":      "mistral",
		"auth_kind": "oauth",
		"api_key":   c.APIKey,
		"base_url":  baseURL,
		"label":     CredentialLabel(c.APIKey),
	}
}
