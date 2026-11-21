package cliproxy

import (
	"context"
	"testing"
	"time"

	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// mistralSignInAuth is the shape of a credential produced by the Mistral sign-in: the key
// and base URL live in Metadata only.
func mistralSignInAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "mistral-0123456789abcdef.json",
		Provider: "mistral",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":      "mistral",
			"auth_kind": "oauth",
			"api_key":   "sk-service-test-key",
			"base_url":  "https://api.mistral.ai/v1",
		},
	}
}

func TestRegisterExecutorForAuth_MistralUsesOpenAICompatExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	service.ensureExecutorsForAuth(mistralSignInAuth())

	registered, ok := service.coreManager.Executor("mistral")
	if !ok || registered == nil {
		t.Fatal("expected a mistral executor after binding a mistral credential")
	}
	if _, isCompat := registered.(*executor.OpenAICompatExecutor); !isCompat {
		t.Fatalf("mistral executor type = %T, want *executor.OpenAICompatExecutor", registered)
	}
	if registered.Identifier() != "mistral" {
		t.Fatalf("mistral executor identifier = %q", registered.Identifier())
	}
}

func TestBaselineExecutorAuthsIncludeMistral(t *testing.T) {
	for _, auth := range baselineExecutorAuths() {
		if auth.Provider == "mistral" {
			return
		}
	}
	t.Fatal("baseline executors must include mistral so the executor exists before the first credential loads")
}

func TestRegisterModelsForAuth_MistralPublishesStaticModelsAndHonorsExclusions(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"mistral": {"mistral-small-latest"},
			},
		},
	}
	auth := mistralSignInAuth()

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)

	registered := map[string]bool{}
	for _, model := range registry.GetModelsForClient(auth.ID) {
		if model != nil {
			registered[model.ID] = true
		}
	}
	for _, model := range internalregistry.GetMistralModels() {
		excluded := model.ID == "mistral-small-latest"
		if excluded && registered[model.ID] {
			t.Fatalf("model %q is excluded by oauth-excluded-models but was registered", model.ID)
		}
		if !excluded && !registered[model.ID] {
			t.Fatalf("model %q was not registered for a mistral credential (registered: %v)", model.ID, registered)
		}
	}
}

// TestMistralCredentialFileIsServedAfterWatcherReload covers the seam the CLI sign-in relies
// on: a saved mistral auth file is picked up by the running service, which binds the
// executor, publishes the models, and keeps the key available to requests.
func TestMistralCredentialFileIsServedAfterWatcherReload(t *testing.T) {
	h := newIssue6631Harness(t)
	cfg := h.service.currentConfig()
	// The harness builds the watcher without starting it, and pauses its first config reload.
	// Release that pause, then start the watcher so file events flow.
	h.releaseFirst()
	watcherCtx, cancelWatcher := context.WithCancel(context.Background())
	t.Cleanup(cancelWatcher)
	if err := h.service.currentWatcher().Start(watcherCtx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}

	record := sdkAuth.NewMistralAuthRecord(&mistralauth.Credential{APIKey: "sk-watcher-test-key", BaseURL: "https://api.mistral.ai/v1"})
	registry := GlobalModelRegistry()
	t.Cleanup(func() { registry.UnregisterClient(record.ID) })

	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	if _, err := store.Save(context.Background(), record); err != nil {
		t.Fatalf("save credential: %v", err)
	}

	// The watcher debounces file events; this deadline only guards against an update that never arrives.
	deadline := time.After(15 * time.Second)
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for len(registry.GetModelsForClient(record.ID)) == 0 {
		select {
		case <-deadline:
			t.Fatal("the watcher never published models for the saved mistral credential")
		case <-poll.C:
		}
	}

	published := map[string]bool{}
	for _, model := range registry.GetModelsForClient(record.ID) {
		published[model.ID] = true
	}
	for _, model := range internalregistry.GetMistralModels() {
		if !published[model.ID] {
			t.Fatalf("model %q missing after reload (published: %v)", model.ID, published)
		}
	}
	if _, ok := h.service.coreManager.Executor("mistral"); !ok {
		t.Fatal("no mistral executor bound after reload")
	}
	loaded, ok := h.service.coreManager.GetByID(record.ID)
	if !ok || loaded.Provider != "mistral" || loaded.Metadata["api_key"] != "sk-watcher-test-key" || loaded.Metadata["base_url"] != "https://api.mistral.ai/v1" {
		t.Fatalf("reloaded credential = %+v, want the saved key and base URL in metadata", loaded)
	}
}
