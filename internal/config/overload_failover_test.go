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

func TestParseConfigBytesClampsRetryTiming(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("max-retry-duration: 999999999999\noverload-cooldown-seconds: 100000\n"))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.MaxRetryDuration != 86400 || cfg.OverloadCooldownSeconds != 86400 {
		t.Fatalf("max-retry-duration=%d overload-cooldown-seconds=%d, want both clamped to 86400", cfg.MaxRetryDuration, cfg.OverloadCooldownSeconds)
	}
}
