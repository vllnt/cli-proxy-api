package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// mistralMetadataAuth mirrors an auth file written by the Mistral sign-in: the key and
// base URL live in Metadata only, with no api_key attribute.
func mistralMetadataAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "mistral-test.json",
		Provider: "mistral",
		Metadata: map[string]any{
			"type":      "mistral",
			"auth_kind": "oauth",
			"api_key":   "mistral-key",
			"base_url":  baseURL,
		},
	}
}

type capturedUpstream struct {
	mu      sync.Mutex
	path    string
	headers http.Header
	body    []byte
}

func (c *capturedUpstream) snapshot() (string, http.Header, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.headers, c.body
}

// newMistralUpstream answers like api.mistral.ai: JSON for plain requests, SSE for streams.
func newMistralUpstream(t *testing.T) (*httptest.Server, *capturedUpstream) {
	t.Helper()
	captured := &capturedUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.path, captured.headers, captured.body = r.URL.Path, r.Header.Clone(), body
		captured.mu.Unlock()
		if ojson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"mistral-large-latest\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"mistral-large-latest","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// withStreamFlag mirrors the API handlers, which put "stream":true in the payload of streaming requests.
func withStreamFlag(payload string, stream bool) []byte {
	if !stream {
		return []byte(payload)
	}
	return []byte(strings.Replace(payload, `{"model"`, `{"stream":true,"model"`, 1))
}

func runMistralRequest(t *testing.T, executor *OpenAICompatExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	t.Helper()
	if opts.Stream {
		result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk: %v", chunk.Err)
			}
		}
		return
	}
	if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestOpenAICompatExecutor_ResolvesCredentialsFromMetadata(t *testing.T) {
	executor := NewOpenAICompatExecutor("mistral", &config.Config{})

	tests := []struct {
		name        string
		auth        *cliproxyauth.Auth
		wantBaseURL string
		wantAPIKey  string
	}{
		{"nil auth", nil, "", ""},
		{"metadata only", mistralMetadataAuth("https://api.mistral.ai/v1"), "https://api.mistral.ai/v1", "mistral-key"},
		{"attributes win over metadata", &cliproxyauth.Auth{
			Attributes: map[string]string{"base_url": "https://attr.example/v1", "api_key": "attr-key"},
			Metadata:   map[string]any{"base_url": "https://meta.example/v1", "api_key": "meta-key"},
		}, "https://attr.example/v1", "attr-key"},
		{"each value falls back independently", &cliproxyauth.Auth{
			Attributes: map[string]string{"base_url": "https://attr.example/v1"},
			Metadata:   map[string]any{"base_url": "https://meta.example/v1", "api_key": " meta-key "},
		}, "https://attr.example/v1", "meta-key"},
		{"non-string metadata is ignored", &cliproxyauth.Auth{
			Metadata: map[string]any{"base_url": 42, "api_key": true},
		}, "", ""},
		{"empty auth", &cliproxyauth.Auth{}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseURL, apiKey := executor.resolveCredentials(tc.auth)
			if baseURL != tc.wantBaseURL || apiKey != tc.wantAPIKey {
				t.Fatalf("resolveCredentials = (%q, %q), want (%q, %q)", baseURL, apiKey, tc.wantBaseURL, tc.wantAPIKey)
			}
		})
	}
}

func TestOpenAICompatExecutor_MistralMetadataCredentialReachesUpstream(t *testing.T) {
	server, captured := newMistralUpstream(t)
	executor := NewOpenAICompatExecutor("mistral", &config.Config{})
	auth := mistralMetadataAuth(server.URL + "/v1")

	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "execute", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			runMistralRequest(t, executor, auth, cliproxyexecutor.Request{
				Model:   "mistral-large-latest",
				Payload: withStreamFlag(`{"model":"mistral-large-latest","messages":[{"role":"user","content":"hi"}]}`, stream),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream})

			path, headers, body := captured.snapshot()
			if path != "/v1/chat/completions" {
				t.Fatalf("upstream path = %q, want /v1/chat/completions", path)
			}
			if got := headers.Get("Authorization"); got != "Bearer mistral-key" {
				t.Fatalf("Authorization = %q, want Bearer mistral-key", got)
			}
			if got := ojson.GetBytes(body, "model").String(); got != "mistral-large-latest" {
				t.Fatalf("upstream model = %q; body=%s", got, body)
			}
		})
	}

	t.Run("PrepareRequest injects the metadata key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
		if err := executor.PrepareRequest(req, auth); err != nil {
			t.Fatalf("PrepareRequest: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer mistral-key" {
			t.Fatalf("Authorization = %q, want Bearer mistral-key", got)
		}
	})
}

// TestOpenAICompatExecutor_MistralPayloadConfigIsAppliedLast guards the AGENTS.md payload
// barrier on the mistral request path: translation, normalization and injection happen
// first, then user payload rules run once on the final body, so a rule always wins.
func TestOpenAICompatExecutor_MistralPayloadConfigIsAppliedLast(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "mistral-large-latest"}},
			Params: map[string]any{"temperature": 0.2, "safe_prompt": true, "max_tokens": 321},
		}},
		Filter: []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{{Name: "mistral-large-latest"}},
			Params: []string{"top_p", "stream_options"},
		}},
	}}
	unmatched := &config.Config{}

	formats := []struct {
		name    string
		source  sdktranslator.Format
		payload string
	}{
		{"openai", sdktranslator.FormatOpenAI, `{"model":"mistral-large-latest","temperature":1,"top_p":0.9,"max_tokens":50,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`},
		{"claude", sdktranslator.FormatClaude, `{"model":"mistral-large-latest","temperature":1,"top_p":0.9,"max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, format := range formats {
		for _, stream := range []bool{false, true} {
			name := format.name + "/" + map[bool]string{false: "execute", true: "stream"}[stream]
			t.Run(name, func(t *testing.T) {
				server, captured := newMistralUpstream(t)
				auth := mistralMetadataAuth(server.URL + "/v1")
				req := cliproxyexecutor.Request{Model: "mistral-large-latest", Payload: withStreamFlag(format.payload, stream)}
				opts := cliproxyexecutor.Options{SourceFormat: format.source, Stream: stream}

				runMistralRequest(t, NewOpenAICompatExecutor("mistral", cfg), auth, req, opts)
				_, _, body := captured.snapshot()
				if got := ojson.GetBytes(body, "temperature").Float(); got != 0.2 {
					t.Fatalf("temperature = %v, want the override 0.2; body=%s", got, body)
				}
				if got := ojson.GetBytes(body, "max_tokens").Int(); got != 321 {
					t.Fatalf("max_tokens = %d, want the override 321; body=%s", got, body)
				}
				if !ojson.GetBytes(body, "safe_prompt").Bool() {
					t.Fatalf("safe_prompt override missing; body=%s", body)
				}
				for _, filtered := range []string{"top_p", "stream_options"} {
					if ojson.GetBytes(body, filtered).Exists() {
						t.Fatalf("%s survived the filter rule; body=%s", filtered, body)
					}
				}

				// Same request without matching rules proves the rules, not the builtin path, caused the result.
				runMistralRequest(t, NewOpenAICompatExecutor("mistral", unmatched), auth, req, opts)
				_, _, plain := captured.snapshot()
				if got := ojson.GetBytes(plain, "temperature").Float(); got != 1 {
					t.Fatalf("baseline temperature = %v, want 1; body=%s", got, plain)
				}
				if ojson.GetBytes(plain, "safe_prompt").Exists() {
					t.Fatalf("baseline must not contain safe_prompt; body=%s", plain)
				}
			})
		}
	}
}
