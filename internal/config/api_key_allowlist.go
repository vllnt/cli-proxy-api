package config

import (
	"fmt"
	"sort"
	"strings"
)

// APIKeyAuthAllowlist restricts a labelled client API key to a set of credentials.
// A credential is allowed when its Auth.ID is listed in AuthIDs or it carries one of Tags
// (see the auth package: Attributes["tags"] or Metadata["tags"]). An entry with neither
// AuthIDs nor Tags allows nothing.
type APIKeyAuthAllowlist struct {
	AuthIDs []string `yaml:"auth-ids,omitempty" json:"auth-ids,omitempty"`
	Tags    []string `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// APIKeyAuthAllowlistFor resolves the allowlist that applies to a client API key.
// restricted is false (and the key keeps full access) when the key has no label or its
// label has no allowlist entry.
func (c *SDKConfig) APIKeyAuthAllowlistFor(apiKey string) (label string, entry APIKeyAuthAllowlist, restricted bool) {
	apiKey = strings.TrimSpace(apiKey)
	if c == nil || apiKey == "" || len(c.APIKeyAuthAllowlist) == 0 {
		return "", APIKeyAuthAllowlist{}, false
	}
	for name, key := range c.APIKeyLabels {
		if strings.TrimSpace(key) != apiKey {
			continue
		}
		entry, ok := c.APIKeyAuthAllowlist[name]
		if !ok {
			continue
		}
		return name, entry, true
	}
	return "", APIKeyAuthAllowlist{}, false
}

// ValidateAPIKeyAuthAllowlist checks api-key-labels and api-key-auth-allowlist.
// Errors name labels only and never contain a key value.
func (c *SDKConfig) ValidateAPIKeyAuthAllowlist() error {
	if c == nil || (len(c.APIKeyLabels) == 0 && len(c.APIKeyAuthAllowlist) == 0) {
		return nil
	}
	configured := make(map[string]struct{}, len(c.APIKeys))
	for _, key := range c.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			configured[key] = struct{}{}
		}
	}
	labels := make(map[string]struct{}, len(c.APIKeyLabels))
	labelByKey := make(map[string]string, len(c.APIKeyLabels))
	for _, label := range sortedKeys(c.APIKeyLabels) {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("access.api-key-labels: label must not be empty")
		}
		if label != strings.TrimSpace(label) {
			return fmt.Errorf("access.api-key-labels: label %q must not have leading or trailing whitespace", label)
		}
		labels[label] = struct{}{}
		key := strings.TrimSpace(c.APIKeyLabels[label])
		if key == "" {
			return fmt.Errorf("access.api-key-labels.%s: key must not be empty", label)
		}
		if _, ok := configured[key]; !ok {
			return fmt.Errorf("access.api-key-labels.%s: key is not listed in access.api-keys", label)
		}
		if other, dup := labelByKey[key]; dup {
			return fmt.Errorf("access.api-key-labels: labels %q and %q name the same key", other, label)
		}
		labelByKey[key] = label
	}
	for _, label := range sortedKeys(c.APIKeyAuthAllowlist) {
		if _, ok := labels[label]; !ok {
			return fmt.Errorf("access.api-key-auth-allowlist.%s: label is not defined in access.api-key-labels", label)
		}
	}
	return nil
}

// ValidateHomeAPIKeyAuthAllowlist rejects api-key-auth-allowlist in Home mode: the Home
// control plane picks credentials remotely, so the allowlist cannot be enforced.
func (cfg *Config) ValidateHomeAPIKeyAuthAllowlist() error {
	if cfg != nil && cfg.Home.Enabled && len(cfg.APIKeyAuthAllowlist) > 0 {
		return fmt.Errorf("access.api-key-auth-allowlist cannot be enforced in Home mode; remove it or disable Home")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
