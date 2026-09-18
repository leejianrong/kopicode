package config

import (
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Host != "localhost" || cfg.Port != 8080 || cfg.Verbose {
		t.Errorf("Default = %+v", cfg)
	}
	if cfg.Retries != 3 {
		t.Errorf("Default().Retries = %d, want 3", cfg.Retries)
	}
}

func TestParseKnownKeys(t *testing.T) {
	cfg, err := Parse("host = example.com\nport = 9000\nverbose = true\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Host != "example.com" || cfg.Port != 9000 || !cfg.Verbose {
		t.Errorf("Parse = %+v", cfg)
	}
	if cfg.Retries != 3 {
		t.Errorf("Retries = %d, want the default 3 when the file omits it", cfg.Retries)
	}
}

func TestParseRetries(t *testing.T) {
	cfg, err := Parse("# how many times to retry\nretries = 5\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Retries != 5 {
		t.Errorf("Retries = %d, want 5", cfg.Retries)
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want the default to survive", cfg.Host)
	}
}

func TestParseRetriesRejectsNonInteger(t *testing.T) {
	if _, err := Parse("retries = often\n"); err == nil {
		t.Fatal("Parse accepted a non-integer retries value")
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	_, err := Parse("colour = blue\n")
	if err == nil {
		t.Fatal("Parse accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "colour") {
		t.Errorf("error %q does not name the offending key", err)
	}
}
