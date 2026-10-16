package config

import (
	"testing"
)

func TestEnvPrecedenceHearthOverBridge(t *testing.T) {
	t.Setenv("HEARTH_HOST", "127.0.0.2")
	t.Setenv("BRIDGE_HOST", "192.168.1.1")
	t.Setenv("HEARTH_API_KEY", "0123456789abcdef")
	t.Setenv("BRIDGE_API_KEY", "weak")
	cfg := Load()
	if cfg.Host != "127.0.0.2" {
		t.Fatalf("expected HEARTH_HOST to win, got %q", cfg.Host)
	}
	if cfg.APIKey != "0123456789abcdef" {
		t.Fatalf("expected HEARTH_API_KEY to win, got %q", cfg.APIKey)
	}
}

func TestEnvFallbackBridgeAccepted(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "20244")
	cfg := Load()
	if cfg.Port != 20244 {
		t.Fatalf("expected BRIDGE_PORT fallback, got %d", cfg.Port)
	}
	t.Setenv("HEARTH_PORT", "20333")
	cfg2 := Load()
	if cfg2.Port != 20333 {
		t.Fatalf("expected HEARTH_PORT to win, got %d", cfg2.Port)
	}
}

func TestDefaultHomeUsesHearth(t *testing.T) {
	cfg := Load()
	if cfg.HomeDir == LegacyHomeDir() {
		t.Fatalf("default home must be the new directory, got legacy %q", cfg.HomeDir)
	}
}

func TestAdminKeyValid(t *testing.T) {
	if AdminKeyValid("public") || AdminKeyValid("short") || AdminKeyValid("") {
		t.Fatal("expected placeholder/short/empty keys to be invalid")
	}
	if !AdminKeyValid("0123456789abcdef") {
		t.Fatal("expected 16+ char random key to be valid")
	}
}
