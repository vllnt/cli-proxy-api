package config

import "testing"

func TestParseConfigBytesOverloadFailoverKeys(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
config-version: 8
routing:
  retry:
    max-retry-duration: 90
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
	if cfg.MaxRetryDuration != 90 || cfg.OverloadCooldownSeconds != 5 || !cfg.Codex.StreamBootstrapBuffering {
		t.Fatalf("max-retry-duration=%d overload-cooldown-seconds=%d stream-bootstrap-buffering=%v, want 90, 5, true",
			cfg.MaxRetryDuration, cfg.OverloadCooldownSeconds, cfg.Codex.StreamBootstrapBuffering)
	}
}
