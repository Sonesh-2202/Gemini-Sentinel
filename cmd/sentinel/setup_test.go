package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
)

func TestRandomHexKeyShape(t *testing.T) {
	seen := make(map[string]bool)
	for range 8 {
		key, err := randomHexKey()
		if err != nil {
			t.Fatalf("randomHexKey: %v", err)
		}
		if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
			t.Fatalf("expected 64 lowercase hex chars, got %q", key)
		}
		if !config.AdminKeyValid(key) {
			t.Fatalf("generated key must satisfy AdminKeyValid, got %q", key)
		}
		if seen[key] {
			t.Fatal("expected unique keys across generations")
		}
		seen[key] = true
	}
}

func setupCfgForTest(t *testing.T) config.Config {
	t.Helper()
	os.Unsetenv("SENTINEL_ENV_FILE")
	dir := t.TempDir()
	return config.Config{
		HomeDir:      dir,
		AccountsDir:  filepath.Join(dir, "accounts"),
		EnvFilePath:  filepath.Join(dir, ".env"),
		AutoAdminKey: true,
	}
}

func TestEnsureAdminKeyGeneratesPersistsAndReuses(t *testing.T) {
	cfg := setupCfgForTest(t)

	key, persisted, err := ensureAdminKey(cfg)
	if err != nil || !persisted || key == "" {
		t.Fatalf("first ensure should generate: key=%q persisted=%v err=%v", key, persisted, err)
	}
	if !config.AdminKeyValid(key) {
		t.Fatalf("generated key must pass AdminKeyValid")
	}
	raw, err := os.ReadFile(cfg.EnvFilePath)
	if err != nil {
		t.Fatalf("expected persisted .env: %v", err)
	}
	if !strings.Contains(string(raw), "SENTINEL_API_KEY="+key) {
		t.Fatalf(".env does not contain the generated key")
	}
	// Reuse on the next call with a fresh Config reading the same directory —
	// simulate config.Load() applying the persisted dotenv.
	if err := os.Setenv("SENTINEL_API_KEY", key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("SENTINEL_API_KEY") })
	reloaded := cfg
	reloaded.APIKey = key
	key2, persisted2, err := ensureAdminKey(reloaded)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if key2 != key || persisted2 {
		t.Fatalf("expected reuse without regeneration: key2=%q persisted=%v", key2, persisted2)
	}
}

func TestEnsureAdminKeyExplicitWins(t *testing.T) {
	cfg := setupCfgForTest(t)
	if err := os.WriteFile(cfg.EnvFilePath, []byte("SENTINEL_API_KEY=old-generated-key-placeholder-stored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.APIKey = "explicit-user-key-0123456789ab"
	key, persisted, err := ensureAdminKey(cfg)
	if err != nil || persisted || key != cfg.APIKey {
		t.Fatalf("explicit key must win untouched: key=%q persisted=%v err=%v", key, persisted, err)
	}
	if text := fileTextMain(t, cfg.EnvFilePath); strings.Contains(text, cfg.APIKey) {
		t.Fatal("explicit env key must never be re-written into the .env")
	}
}

func fileTextMain(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
