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
// label has no allowlist entry. The returned label is for server logs only.
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

// WithRetiringAPIKeyRestrictions returns c plus the restrictions previous applied to client keys
// that c no longer lists. A reload publishes it while the access providers still admit those keys,
// so a key that is being revoked is never served unrestricted in between; the reload then
// publishes c itself. A key that stays listed follows c at once. It returns c itself when nothing
// is retiring and never modifies c or previous. The carried labels exist only in the returned
// value and are not valid configuration.
func (c *SDKConfig) WithRetiringAPIKeyRestrictions(previous *SDKConfig) *SDKConfig {
	if c == nil || previous == nil || len(previous.APIKeyAuthAllowlist) == 0 {
		return c
	}
	listed := make(map[string]struct{}, len(c.APIKeys))
	for _, key := range c.APIKeys {
		listed[strings.TrimSpace(key)] = struct{}{}
	}
	var merged *SDKConfig
	for _, label := range sortedKeys(previous.APIKeyAuthAllowlist) {
		key := strings.TrimSpace(previous.APIKeyLabels[label])
		if _, stillListed := listed[key]; key == "" || stillListed {
			continue
		}
		if merged == nil {
			copied := *c
			copied.APIKeyLabels = make(map[string]string, len(c.APIKeyLabels)+1)
			for name, value := range c.APIKeyLabels {
				copied.APIKeyLabels[name] = value
			}
			copied.APIKeyAuthAllowlist = make(map[string]APIKeyAuthAllowlist, len(c.APIKeyAuthAllowlist)+1)
			for name, entry := range c.APIKeyAuthAllowlist {
				copied.APIKeyAuthAllowlist[name] = entry
			}
			merged = &copied
		}
		carried := label + " (retiring)"
		for _, taken := merged.APIKeyLabels[carried]; taken; _, taken = merged.APIKeyLabels[carried] {
			carried += "!"
		}
		merged.APIKeyLabels[carried] = key
		merged.APIKeyAuthAllowlist[carried] = previous.APIKeyAuthAllowlist[label]
	}
	if merged == nil {
		return c
	}
	return merged
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
