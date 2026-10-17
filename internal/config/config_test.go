package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvPrecedence(t *testing.T) {
	t.Setenv("SENTINEL_HOST", "127.0.0.2")
	t.Setenv("HOST", "192.168.1.1")
	t.Setenv("SENTINEL_API_KEY", "0123456789abcdef")
	cfg := Load()
	if cfg.Host != "127.0.0.2" {
		t.Fatalf("expected SENTINEL_HOST to win, got %q", cfg.Host)
	}
	if cfg.APIKey != "0123456789abcdef" {
		t.Fatalf("expected SENTINEL_API_KEY to win, got %q", cfg.APIKey)
	}
}

func TestGenericHostPortFallback(t *testing.T) {
	t.Setenv("PORT", "20244")
	cfg := Load()
	if cfg.Port != 20244 {
		t.Fatalf("expected PORT fallback, got %d", cfg.Port)
	}
	t.Setenv("SENTINEL_PORT", "20444")
	cfg3 := Load()
	if cfg3.Port != 20444 {
		t.Fatalf("expected SENTINEL_PORT to win, got %d", cfg3.Port)
	}
}

func TestDefaultHomeUsesSentinel(t *testing.T) {
	t.Setenv("SENTINEL_HOME", "")
	cfg := Load()
	if !strings.HasSuffix(cfg.HomeDir, filepath.Join(".gemini-sentinel")) {
		t.Fatalf("default home must be ~/.gemini-sentinel, got %q", cfg.HomeDir)
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
