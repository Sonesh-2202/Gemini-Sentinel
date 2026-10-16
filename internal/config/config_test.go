package config

import (
	"testing"
)

func TestEnvPrecedenceSentinelOverLegacy(t *testing.T) {
	t.Setenv("SENTINEL_HOST", "127.0.0.2")
	t.Setenv("HEARTH_HOST", "127.0.0.3")
	t.Setenv("BRIDGE_HOST", "192.168.1.1")
	t.Setenv("SENTINEL_API_KEY", "0123456789abcdef")
	t.Setenv("HEARTH_API_KEY", "fedcba9876543210")
	t.Setenv("BRIDGE_API_KEY", "weak")
	cfg := Load()
	if cfg.Host != "127.0.0.2" {
		t.Fatalf("expected SENTINEL_HOST to win, got %q", cfg.Host)
	}
	if cfg.APIKey != "0123456789abcdef" {
		t.Fatalf("expected SENTINEL_API_KEY to win, got %q", cfg.APIKey)
	}
}

func TestEnvFallbackLegacyAccepted(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "20244")
	cfg := Load()
	if cfg.Port != 20244 {
		t.Fatalf("expected BRIDGE_PORT fallback, got %d", cfg.Port)
	}
	t.Setenv("HEARTH_PORT", "20333")
	cfg2 := Load()
	if cfg2.Port != 20333 {
		t.Fatalf("expected HEARTH_PORT to win over BRIDGE_PORT, got %d", cfg2.Port)
	}
	t.Setenv("SENTINEL_PORT", "20444")
	cfg3 := Load()
	if cfg3.Port != 20444 {
		t.Fatalf("expected SENTINEL_PORT to win, got %d", cfg3.Port)
	}
}

func TestDefaultHomeUsesSentinel(t *testing.T) {
	cfg := Load()
	for _, legacy := range LegacyHomeDirs() {
		if cfg.HomeDir == legacy {
			t.Fatalf("default home must be the new directory, got legacy %q", cfg.HomeDir)
		}
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
