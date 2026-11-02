package cliproxy

import (
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func allowlistServiceConfig() *internalconfig.Config {
	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{"test-key-monitor"}
	cfg.APIKeyLabels = map[string]string{"monitor": "test-key-monitor"}
	cfg.APIKeyAuthAllowlist = map[string]internalconfig.APIKeyAuthAllowlist{
		"monitor": {AuthIDs: []string{"known.json", "missing.json"}},
	}
	return cfg
}

func TestWarnUnknownAPIKeyAllowlistAuthIDs(t *testing.T) {
	_, hook := logtest.NewNullLogger()
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldLevel := logger.GetLevel()
	logger.AddHook(hook)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		logger.SetLevel(oldLevel)
		logger.ReplaceHooks(oldHooks)
	})

	cfg := allowlistServiceConfig()
	warnUnknownAPIKeyAllowlistAuthIDs(cfg, nil)
	if len(hook.AllEntries()) != 0 {
		t.Fatalf("no loaded credential yet must not warn, got %v", hook.AllEntries())
	}

	warnUnknownAPIKeyAllowlistAuthIDs(cfg, []*coreauth.Auth{{ID: "known.json"}, nil})
	entries := hook.AllEntries()
	if len(entries) != 1 || entries[0].Level != log.WarnLevel {
		t.Fatalf("want one warning, got %v", entries)
	}
	message := entries[0].Message
	if !strings.Contains(message, "missing.json") || strings.Contains(message, "known.json") || strings.Contains(message, "test-key") {
		t.Fatalf("warning must name only the unknown id and never a key: %q", message)
	}

	hook.Reset()
	warnUnknownAPIKeyAllowlistAuthIDs(cfg, []*coreauth.Auth{{ID: "known.json"}, {ID: "missing.json"}})
	warnUnknownAPIKeyAllowlistAuthIDs(&internalconfig.Config{}, []*coreauth.Auth{{ID: "known.json"}})
	if len(hook.AllEntries()) != 0 {
		t.Fatalf("known ids or no allowlist must not warn, got %v", hook.AllEntries())
	}
}

func TestCommitConfigUpdateRejectsInvalidAPIKeyAllowlist(t *testing.T) {
	service := &Service{}
	if commit := service.commitConfigUpdate(allowlistServiceConfig()); commit.cfg == nil {
		t.Fatal("a valid allowlist must be accepted (unknown auth-ids only warn)")
	}

	broken := allowlistServiceConfig()
	broken.APIKeyAuthAllowlist["ghost"] = internalconfig.APIKeyAuthAllowlist{Tags: []string{"x"}}
	if commit := service.commitConfigUpdate(broken); commit.cfg != nil {
		t.Fatal("an allowlist label without api-key-labels entry must be rejected")
	}

	home := allowlistServiceConfig()
	home.Home.Enabled = true
	if commit := service.commitConfigUpdate(home); commit.cfg != nil {
		t.Fatal("an allowlist in Home mode must be rejected")
	}
	service.cfgMu.RLock()
	current := service.cfg
	service.cfgMu.RUnlock()
	if current == nil || len(current.APIKeyAuthAllowlist) != 1 {
		t.Fatalf("a rejected update must keep the last valid config, got %+v", current)
	}
}
