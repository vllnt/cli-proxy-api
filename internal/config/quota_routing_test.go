package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuotaRoutingConfigParseAndPersist(t *testing.T) {
	for _, prefix := range []string{"", "config-version: 8\n"} {
		t.Run(prefix, func(t *testing.T) {
			raw := prefix + "routing:\n  strategy: weighted-round-robin\n  session-affinity: true\n  quota-aware: true\n  quota-max-age: 2m\n"
			cfg, errParse := ParseConfigBytes([]byte(raw))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if !cfg.Routing.QuotaAware || cfg.Routing.QuotaMaxAge != "2m" || !cfg.Routing.SessionAffinity || cfg.Routing.Strategy != "weighted-round-robin" {
				t.Fatalf("parsed routing = %+v", cfg.Routing)
			}
			path := filepath.Join(t.TempDir(), "quota-config.yaml")
			if errWrite := os.WriteFile(path, []byte(raw), 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
				t.Fatal(errSave)
			}
			loaded, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if loaded.Routing.QuotaAware != cfg.Routing.QuotaAware || loaded.Routing.QuotaMaxAge != cfg.Routing.QuotaMaxAge {
				t.Fatalf("persisted routing = %+v", loaded.Routing)
			}
		})
	}
	cfg, errParse := ParseConfigBytes([]byte("routing: {strategy: round-robin}"))
	if errParse != nil || cfg.Routing.QuotaAware || cfg.Routing.QuotaMaxAge != "" {
		t.Fatalf("quota routing must remain opt-in: config=%v err=%v", cfg, errParse)
	}
}
