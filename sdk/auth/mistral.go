package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	mistralauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/mistral"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// MistralAuthenticator implements the Mistral Vibe browser sign-in. The sign-in
// provisions a long-lived API key used for inference on api.mistral.ai.
type MistralAuthenticator struct{}

// NewMistralAuthenticator constructs a new Mistral authenticator.
func NewMistralAuthenticator() Authenticator {
	return &MistralAuthenticator{}
}

// Provider returns the provider key for Mistral.
func (MistralAuthenticator) Provider() string {
	return "mistral"
}

// RefreshLead disables scheduled refresh: the provisioned key does not expire and
// has no refresh token. A rejected key is handled like any other invalid credential.
func (MistralAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// NewMistralAuthRecord builds the auth record persisted for a completed Mistral sign-in.
// The key stays in Metadata only, so it never reaches Attributes or usage records.
func NewMistralAuthRecord(cred *mistralauth.Credential) *coreauth.Auth {
	fileName := mistralauth.CredentialFileName(cred.APIKey)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "mistral",
		FileName: fileName,
		Label:    mistralauth.CredentialLabel(cred.APIKey),
		Metadata: cred.Metadata(),
	}
}

// Login runs the browser sign-in and returns the auth record to persist.
func (a MistralAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := mistralauth.NewMistralAuth(cfg)

	fmt.Println("Starting Mistral authentication...")
	signIn, err := authSvc.StartSignIn(ctx)
	if err != nil {
		return nil, fmt.Errorf("mistral: failed to start sign-in: %w", err)
	}

	signInURL := strings.TrimSpace(signIn.URL)
	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", signInURL)
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(signInURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			} else {
				fmt.Println("Browser opened automatically.")
			}
		} else {
			log.Warn("No browser available; please open the URL manually")
		}
	}

	fmt.Println("Waiting for authorization...")
	fmt.Printf("(This will time out in %d seconds if not approved)\n", int(signIn.Lifetime/time.Second))

	cred, err := authSvc.WaitForSignIn(ctx, signIn)
	if err != nil {
		return nil, fmt.Errorf("mistral: %w", err)
	}

	fmt.Println("Mistral authentication successful")
	return NewMistralAuthRecord(cred), nil
}
