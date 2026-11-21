package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
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
