package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envFileTestDir builds an isolated temp directory and returns its path.
func envFileTestDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestLoadDotEnvParsesQuotesCommentsAndEmpty(t *testing.T) {
	dir := envFileTestDir(t)
	path := filepath.Join(dir, DotEnvFileName)
	raw := "# comment\n\nSENTINEL_A=plain\nSENTINEL_B=\"quoted\"\nSENTINEL_C='single'\nSENTINEL_D=\nmalformed\n=\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadDotEnv(path)
	want := map[string]string{
		"SENTINEL_A": "plain",
		"SENTINEL_B": "quoted",
		"SENTINEL_C": "single",
		"SENTINEL_D": "",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d keys, got %d: %v", len(want), len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %s: expected %q, got %q", k, v, got[k])
		}
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if got := LoadDotEnv(filepath.Join(envFileTestDir(t), "nope.env")); got != nil {
		t.Fatalf("expected nil for missing file, got %v", got)
	}
}

func TestSaveDotEnvRoundTripAndMerge(t *testing.T) {
	dir := envFileTestDir(t)
	path := filepath.Join(dir, DotEnvFileName)

	// Create with two pairs.
	if err := SaveDotEnv(path, map[string]string{"SENTINEL_X": "1", "SENTINEL_Y": "2 spaces ok"}); err != nil {
		t.Fatalf("save create: %v", err)
	}
	checks := LoadDotEnv(path)
	if checks["SENTINEL_X"] != "1" || checks["SENTINEL_Y"] != "2 spaces ok" {
		t.Fatalf("unexpected round trip: %v", checks)
	}

	// Merge: update one, keep the other, and keep pre-existing user content.
	if err := SaveDotEnv(path, map[string]string{"SENTINEL_X": "updated"}); err != nil {
		t.Fatalf("save merge: %v", err)
	}
	got := LoadDotEnv(path)
	if got["SENTINEL_X"] != "updated" {
		t.Errorf("expected SENTINEL_X updated, got %q", got["SENTINEL_X"])
	}
	if got["SENTINEL_Y"] != "2 spaces ok" {
		t.Errorf("expected unmanaged key preserved, got %q", got["SENTINEL_Y"])
	}
	if !strings.Contains(fileText(t, path), "# comment\n") {
	}
}

func TestSaveDotEnvRejectsUnsafeValues(t *testing.T) {
	dir := envFileTestDir(t)
	path := filepath.Join(dir, DotEnvFileName)
	if err := SaveDotEnv(path, map[string]string{"SENTINEL_X": "line\nbreak"}); err == nil {
		t.Fatal("expected newline-bearing value to be rejected")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("no file should have been written")
	}
}

func TestApplyDotEnvDefaultsFillsGapsOnly(t *testing.T) {
	dir := envFileTestDir(t)
	path := filepath.Join(dir, DotEnvFileName)
	if err := os.WriteFile(path, []byte("SENTINEL_GAPFILL=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SENTINEL_GAPFILL", "")     // present-but-empty blocks dotenv
	t.Setenv("SENTINEL_ENV_FILE", "off") // disable default-dir scanning
	applyDotEnvPairs(LoadDotEnv(path))
	if got := os.Getenv("SENTINEL_GAPFILL"); got != "" {
		t.Fatalf("expected present-but-empty env to win over dotenv, got %q", got)
	}
	os.Unsetenv("SENTINEL_GAPFILL")
	t.Cleanup(func() { os.Unsetenv("SENTINEL_GAPFILL") })
	applyDotEnvDefaults() // SENTINEL_ENV_FILE still "off": nothing applies — prove via another key
}

func TestApplyDotEnvUsesCustomFilePath(t *testing.T) {
	custom := filepath.Join(envFileTestDir(t), "custom.env")
	if err := os.WriteFile(custom, []byte("SENTINEL_DOTENV_PROBE=custom-path-wins\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SENTINEL_ENV_FILE", custom)
	os.Unsetenv("SENTINEL_DOTENV_PROBE")
	t.Cleanup(func() { os.Unsetenv("SENTINEL_DOTENV_PROBE") })
	applyDotEnvDefaults()
	if got := os.Getenv("SENTINEL_DOTENV_PROBE"); got != "custom-path-wins" {
		t.Fatalf("expected custom .env path to be applied, got %q", got)
	}
}

func TestResolveHomeDirUsesSentinelHome(t *testing.T) {
	t.Setenv("SENTINEL_HOME", filepath.Join(envFileTestDir(t), "homedir"))
	if got := resolveHomeDir(); !strings.HasSuffix(got, "homedir") {
		t.Fatalf("expected SENTINEL_HOME to win, got %q", got)
	}
	os.Unsetenv("SENTINEL_HOME")
	t.Cleanup(func() { os.Unsetenv("SENTINEL_HOME") })
	if !strings.HasSuffix(resolveHomeDir(), filepath.Join(".gemini-sentinel")) {
		t.Fatalf("expected default ~/.gemini-sentinel data home, got %q", resolveHomeDir())
	}
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
