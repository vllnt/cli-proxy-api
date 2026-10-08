package config

import "testing"

func TestParseConfigBytesOverloadFailoverKeys(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
config-version: 8
routing:
  cooldown:
    overload-cooldown-seconds: 5
oauth:
  providers:
    codex:
      stream-bootstrap-buffering: true
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.OverloadCooldownSeconds != 5 || !cfg.Codex.StreamBootstrapBuffering {
		t.Fatalf("overload-cooldown-seconds=%d stream-bootstrap-buffering=%v, want 5, true",
			cfg.OverloadCooldownSeconds, cfg.Codex.StreamBootstrapBuffering)
	}
}
