package configaccess

import (
	"context"
	"net/http"
	"strings"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	if len(keys) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keys, credentialAllowlists(cfg, keys)),
	)
}

// credentialAllowlists resolves, per client key, the credential allowlist in cfg. The provider
// keeps its own copy, so it answers for the configuration that admitted the key even after a
// later reload.
func credentialAllowlists(cfg *sdkconfig.SDKConfig, keys []string) map[string]sdkaccess.CredentialAllowlist {
	var lists map[string]sdkaccess.CredentialAllowlist
	for _, key := range keys {
		label, entry, restricted := cfg.APIKeyAuthAllowlistFor(key)
		if !restricted {
			continue
		}
		if lists == nil {
			lists = make(map[string]sdkaccess.CredentialAllowlist)
		}
		lists[key] = sdkaccess.CredentialAllowlist{
			Label:   label,
			AuthIDs: append([]string{}, entry.AuthIDs...),
			Tags:    append([]string{}, entry.Tags...),
		}
	}
	return lists
}

type provider struct {
	name      string
	keys      map[string]struct{}
	allowlist map[string]sdkaccess.CredentialAllowlist
}

func newProvider(name string, keys []string, allowlist map[string]sdkaccess.CredentialAllowlist) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	return &provider{name: providerName, keys: keySet, allowlist: allowlist}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if _, ok := p.keys[candidate.value]; ok {
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata: map[string]string{
					"source": candidate.source,
				},
				CredentialAllowlist: p.credentialAllowlist(candidate.value),
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

// credentialAllowlist returns a private copy of the key's allowlist, or nil when the key is unrestricted.
func (p *provider) credentialAllowlist(key string) *sdkaccess.CredentialAllowlist {
	list, ok := p.allowlist[key]
	if !ok {
		return nil
	}
	list.AuthIDs = append([]string{}, list.AuthIDs...)
	list.Tags = append([]string{}, list.Tags...)
	return &list
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
