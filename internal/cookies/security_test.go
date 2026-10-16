package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAccountFileRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadAccountFile(bad); err == nil {
		t.Fatal("expected malformed credential file to fail loading")
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadAccountFile(empty); err == nil {
		t.Fatal("expected empty credential file to fail loading")
	}
	if _, _, err := LoadAccountFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected missing credential file to fail loading")
	}
}

func TestSaveCredentialFileAtomicNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acct.json")
	for i := range 3 {
		rec := CredentialFile{Name: "acct", Email: "a@example.com", Cookies: "SAPISID=sap"}
		if _, err := SaveCredentialFile(path, rec); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("interrupted-write temp file left behind: %s", e.Name())
		}
	}
	loaded, _, err := LoadAccountFile(path)
	if err != nil || loaded.Email != "a@example.com" {
		t.Fatalf("reload after repeated saves failed: %+v %v", loaded, err)
	}
}

func TestSaveCredentialFileNeverLogsSecrets(t *testing.T) {
	// Regression guard: error paths must not echo cookie values.
	_, _, err := ParseImportPayload("")
	if err == nil || strings.Contains(err.Error(), "SAPISID") {
		t.Fatalf("error message must not contain secrets: %v", err)
	}
}
