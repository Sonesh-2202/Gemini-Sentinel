package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
)

func TestLoadCredentialsSkipsTraversalNames(t *testing.T) {
	dir := t.TempDir()
	acctsDir := filepath.Join(dir, "accounts")
	if err := os.MkdirAll(acctsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Only separator-containing names are traversal risks; ReadDir can never
	// return those, and reSafeID rejects them. Dot-only names stay inside
	// AccountsDir when joined, so they are not escapes.
	for _, bad := range []string{"../evil", "a/b", "a\\b", "a/b.json", "..\\evil"} {
		id := strings.TrimSuffix(bad, ".json")
		if reSafeID.MatchString(id) {
			t.Errorf("unsafe account id %q passes reSafeID", bad)
		}
	}
	if !reSafeID.MatchString("acc1") || !reSafeID.MatchString("a.b-c_d") {
		t.Error("expected safe account names to pass reSafeID")
	}

	cfg := config.Load()
	cfg.HomeDir = dir
	cfg.AccountsDir = acctsDir
	srv := New(cfg)
	if n := srv.LoadCredentials(); n != 0 {
		t.Fatalf("expected 0 accounts from empty dir, got %d", n)
	}
}

func TestRequestLimitsRejected(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	_ = acct
	// Too many messages.
	msgs := make([]any, 0, 600)
	for range 600 {
		msgs = append(msgs, map[string]any{"role": "user", "content": "hi"})
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, map[string]any{
		"model":    testModel.ModelName,
		"messages": msgs,
	}, AuthScope{OK: true, AccountID: "acct"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "too many messages") {
		t.Fatalf("expected 400 too many messages, got %d: %s", rec.Code, rec.Body.String())
	}
	// Too many tools.
	tools := make([]any, 0, 70)
	for i := range 70 {
		tools = append(tools, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "tool"},
		})
		_ = i
	}
	rec2 := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec2, map[string]any{
		"model":    testModel.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    tools,
	}, AuthScope{OK: true, AccountID: "acct"})
	if rec2.Code == 200 {
		t.Fatalf("expected tool-count limit rejection, got 200")
	}
	// Malformed JSON body.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{bad json`))
	req.Header.Set("Authorization", "Bearer audit")
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, req)
	if rec3.Code != 400 {
		t.Fatalf("expected 400 malformed JSON, got %d", rec3.Code)
	}
	// Oversized body.
	big := strings.Repeat("x", 33*1024*1024)
	reqBig := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	reqBig.Header.Set("Authorization", "Bearer audit")
	recBig := httptest.NewRecorder()
	srv.ServeHTTP(recBig, reqBig)
	if recBig.Code != 413 && recBig.Code != 400 {
		t.Fatalf("expected 413/400 oversized body, got %d", recBig.Code)
	}
}
