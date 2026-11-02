package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const allowlistTestBase = `
access:
  api-keys: ["test-key-a", "test-key-b"]
`

func TestAPIKeyAuthAllowlistParse(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels:
    agents: "test-key-a"
    monitor: "test-key-b"
  api-key-auth-allowlist:
    monitor:
      auth-ids: ["claude-0123abcd.json", "codex-9f8e7d6c.json"]
      tags: ["monitor-pool"]
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if got := cfg.APIKeyLabels; !reflect.DeepEqual(got, map[string]string{"agents": "test-key-a", "monitor": "test-key-b"}) {
		t.Fatalf("APIKeyLabels = %v", got)
	}
	want := APIKeyAuthAllowlist{AuthIDs: []string{"claude-0123abcd.json", "codex-9f8e7d6c.json"}, Tags: []string{"monitor-pool"}}
	if got := cfg.APIKeyAuthAllowlist["monitor"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist[monitor] = %+v, want %+v", got, want)
	}
	if _, ok := cfg.APIKeyAuthAllowlist["agents"]; ok {
		t.Fatal("label without an allowlist entry must stay absent")
	}
}

func TestAPIKeyAuthAllowlistUnsetIsNil(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(allowlistTestBase))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.APIKeyLabels != nil || cfg.APIKeyAuthAllowlist != nil {
		t.Fatalf("unset must stay nil: labels=%v allowlist=%v", cfg.APIKeyLabels, cfg.APIKeyAuthAllowlist)
	}
	if _, _, restricted := cfg.SDKConfig.APIKeyAuthAllowlistFor("test-key-a"); restricted {
		t.Fatal("unset allowlist must not restrict any key")
	}
}

func TestAPIKeyAuthAllowlistEmptyEntryExists(t *testing.T) {
	for name, entry := range map[string]string{
		"empty map":  "monitor: {}",
		"null value": "monitor:",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {monitor: "test-key-b"}
  api-key-auth-allowlist:
    ` + entry + "\n"))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			label, got, restricted := cfg.SDKConfig.APIKeyAuthAllowlistFor("test-key-b")
			if !restricted || label != "monitor" || len(got.AuthIDs) != 0 || len(got.Tags) != 0 {
				t.Fatalf("empty entry must exist and restrict: label=%q entry=%+v restricted=%v", label, got, restricted)
			}
		})
	}
}

func TestAPIKeyAuthAllowlistFor(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {agents: "test-key-a", monitor: "test-key-b"}
  api-key-auth-allowlist:
    monitor: {auth-ids: ["a.json"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, key, wantLabel string
		restricted           bool
	}{
		{"labelled with entry", "test-key-b", "monitor", true},
		{"padded key", "  test-key-b ", "monitor", true},
		{"label without entry", "test-key-a", "", false},
		{"unlabelled key", "test-key-z", "", false},
		{"empty key", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, _, restricted := cfg.SDKConfig.APIKeyAuthAllowlistFor(tt.key)
			if label != tt.wantLabel || restricted != tt.restricted {
				t.Fatalf("APIKeyAuthAllowlistFor(%q) = (%q, %v), want (%q, %v)", tt.key, label, restricted, tt.wantLabel, tt.restricted)
			}
		})
	}
	var nilCfg *SDKConfig
	if _, _, restricted := nilCfg.APIKeyAuthAllowlistFor("test-key-b"); restricted {
		t.Fatal("nil config must not restrict")
	}
}

func TestAPIKeyAuthAllowlistValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty label", `{"": "test-key-a"}`, "label must not be empty"},
		{"label with whitespace", `{" agents": "test-key-a"}`, "whitespace"},
		{"empty key", `{agents: ""}`, "key must not be empty"},
		{"key not in api-keys", `{agents: "test-key-missing"}`, "not listed in access.api-keys"},
		{"two labels one key", `{agents: "test-key-a", other: "test-key-a"}`, "name the same key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte(allowlistTestBase + "  api-key-labels: " + tt.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "test-key-") {
				t.Fatalf("error must not contain a key value: %v", err)
			}
		})
	}

	t.Run("allowlist label not defined", func(t *testing.T) {
		_, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {agents: "test-key-a"}
  api-key-auth-allowlist:
    ghost: {tags: ["x"]}
`))
		if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not defined in access.api-key-labels") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("allowlist without any label", func(t *testing.T) {
		_, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-auth-allowlist:
    monitor: {tags: ["x"]}
`))
		if err == nil {
			t.Fatal("allowlist entry without api-key-labels must fail")
		}
	})
	t.Run("duplicate yaml label", func(t *testing.T) {
		_, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels:
    agents: "test-key-a"
    agents: "test-key-b"
`))
		if err == nil {
			t.Fatal("duplicate label keys must fail")
		}
	})
	t.Run("duplicate yaml allowlist entry", func(t *testing.T) {
		_, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {agents: "test-key-a"}
  api-key-auth-allowlist:
    agents: {tags: ["x"]}
    agents: {tags: ["y"]}
`))
		if err == nil {
			t.Fatal("duplicate allowlist entries must fail")
		}
	})
}

func TestAPIKeyAuthAllowlistUnknownAuthIDIsNotAnError(t *testing.T) {
	if _, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {monitor: "test-key-b"}
  api-key-auth-allowlist:
    monitor: {auth-ids: ["does-not-exist.json"]}
`)); err != nil {
		t.Fatalf("unknown auth-ids must only warn at runtime, got %v", err)
	}
}

func TestLoadConfigRejectsInvalidAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := allowlistTestBase + "  api-key-labels: {monitor: \"test-key-missing\"}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, optional := range []bool{false, true} {
		if _, err := LoadConfigOptional(path, optional); err == nil {
			t.Fatalf("LoadConfigOptional(optional=%v) must reject an invalid allowlist", optional)
		}
	}
}

func TestAPIKeyAuthAllowlistRejectedInHomeMode(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(allowlistTestBase + `
  api-key-labels: {monitor: "test-key-b"}
  api-key-auth-allowlist:
    monitor: {tags: ["x"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateHomeAPIKeyAuthAllowlist(); err != nil {
		t.Fatalf("non-Home mode must accept the allowlist, got %v", err)
	}
	cfg.Home.Enabled = true
	if err = cfg.ValidateHomeAPIKeyAuthAllowlist(); err == nil || !strings.Contains(err.Error(), "Home") {
		t.Fatalf("Home mode with an allowlist must fail, got %v", err)
	}
	cfg.APIKeyAuthAllowlist = nil
	if err = cfg.ValidateHomeAPIKeyAuthAllowlist(); err != nil {
		t.Fatalf("Home mode without an allowlist must pass, got %v", err)
	}
	var nilCfg *Config
	if err = nilCfg.ValidateHomeAPIKeyAuthAllowlist(); err != nil {
		t.Fatalf("nil config must pass, got %v", err)
	}
}

// Operators enable the feature by uncommenting the example block; it must load as written.
func TestAPIKeyAuthAllowlistExampleUncommentsAndLoads(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(example), "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  # api-key-labels:") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("config.example.yaml is missing the api-key-labels example")
	}
	for i := start; i < len(lines) && strings.HasPrefix(lines[i], "  #"); i++ {
		lines[i] = "  " + strings.TrimPrefix(lines[i], "  # ")
	}
	cfg, err := ParseConfigBytes([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("uncommented example must load: %v", err)
	}
	label, entry, restricted := cfg.SDKConfig.APIKeyAuthAllowlistFor("your-api-key-2")
	if !restricted || label != "monitor" || len(entry.AuthIDs) != 1 || len(entry.Tags) != 1 {
		t.Fatalf("example allowlist did not reach runtime config: label=%q entry=%+v restricted=%v", label, entry, restricted)
	}
	if _, _, restricted = cfg.SDKConfig.APIKeyAuthAllowlistFor("your-api-key-1"); restricted {
		t.Fatal("the agents label has no allowlist entry and must stay unrestricted")
	}
}
