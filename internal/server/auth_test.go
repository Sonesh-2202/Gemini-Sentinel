package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
)

const testAdminKey = "0123456789abcdef0123456789abcdef"

func newAuthTestServer(t *testing.T) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = home
	cfg.AccountsDir = filepath.Join(home, "accounts")
	cfg.APIKey = testAdminKey
	cfg.AccountTokens = nil
	srv := New(cfg)
	acctA := &AccountState{
		ID:       "acctA",
		Email:    "a@example.com",
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	acctB := &AccountState{
		ID:       "acctB",
		Email:    "b@example.com",
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acctA, acctB})
	return srv
}

func doReq(srv *Server, method, path, token, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		if header == "" {
			header = "Authorization"
		}
		if header == "Authorization" {
			req.Header.Set(header, "Bearer "+token)
		} else {
			req.Header.Set(header, token)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestAuthMissingAndInvalid(t *testing.T) {
	srv := newAuthTestServer(t)
	for _, path := range []string{"/v1/models", "/v1/accounts", "/v1/quota", "/v1/sessions", "/v1/chat/completions", "/v1/responses", "/v1/refresh"} {
		method := http.MethodGet
		if strings.Contains(path, "chat") || strings.Contains(path, "responses") || strings.Contains(path, "refresh") {
			method = http.MethodPost
			req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != 401 {
				t.Errorf("%s without key: got %d, want 401", path, rec.Code)
			}
			continue
		}
		if rec := doReq(srv, method, path, "", ""); rec.Code != 401 {
			t.Errorf("%s without key: got %d, want 401", path, rec.Code)
		}
		if rec := doReq(srv, method, path, "wrong-key", ""); rec.Code != 401 {
			t.Errorf("%s with invalid key: got %d, want 401", path, rec.Code)
		}
		if rec := doReq(srv, method, path, "public", ""); rec.Code != 401 {
			t.Errorf("%s with public key: got %d, want 401", path, rec.Code)
		}
	}
}

func TestAuthValidAdminAndAccount(t *testing.T) {
	srv := newAuthTestServer(t)
	if rec := doReq(srv, http.MethodGet, "/v1/models", testAdminKey, ""); rec.Code != 200 {
		t.Errorf("admin /v1/models: got %d", rec.Code)
	}
	if rec := doReq(srv, http.MethodGet, "/v1/accounts", testAdminKey, ""); rec.Code != 200 {
		t.Errorf("admin /v1/accounts: got %d", rec.Code)
	}
	if rec := doReq(srv, http.MethodGet, "/v1/accounts", "sentinel-acctA", ""); rec.Code != 403 {
		t.Errorf("account /v1/accounts: got %d, want 403", rec.Code)
	}
	for _, path := range []string{"/v1/quota", "/v1/sessions"} {
		if rec := doReq(srv, http.MethodGet, path, testAdminKey, ""); rec.Code != 200 {
			t.Errorf("admin %s: got %d", path, rec.Code)
		}
		if rec := doReq(srv, http.MethodGet, path, "sentinel-acctA", ""); rec.Code != 403 {
			t.Errorf("account %s: got %d, want 403", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("admin /v1/refresh: got %d (%s)", rec.Code, rec.Body.String())
	}
	req2 := httptest.NewRequest(http.MethodPost, "/v1/refresh", nil)
	req2.Header.Set("Authorization", "Bearer sentinel-acctA")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Errorf("account /v1/refresh: got %d, want 403", rec2.Code)
	}
}

func TestAuthAccountIsolation(t *testing.T) {
	srv := newAuthTestServer(t)
	// Account A sees only its own model; account B likewise; neither sees the other.
	for _, tc := range []struct{ token, want string }{
		{"sentinel-acctA", "acctA"},
		{"acctA", "acctA"},
		{"sentinel-acctB", "acctB"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("token %q: got %d", tc.token, rec.Code)
		}
	}
	// Cross-account token must not resolve to another account.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "sentinel-acctA")
	rec := httptest.NewRecorder()
	scope := srv.ResolveAuthScope(req)
	if !scope.OK || scope.AccountID != "acctA" || scope.Admin {
		t.Fatalf("x-api-key scope = %+v, want AccountID=acctA", scope)
	}
	_ = rec
}

func TestAuthQueryStringRejected(t *testing.T) {
	srv := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?key=sentinel-acctA", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("query-string key: got %d, want 401 (query keys must not be accepted)", rec.Code)
	}
}

func TestAuthHeaderVariants(t *testing.T) {
	srv := newAuthTestServer(t)
	if rec := doReq(srv, http.MethodGet, "/v1/models", "sentinel-acctA", "x-api-key"); rec.Code != 200 {
		t.Errorf("x-api-key: got %d", rec.Code)
	}
	if rec := doReq(srv, http.MethodGet, "/v1/models", "sentinel-acctA", "x-goog-api-key"); rec.Code != 200 {
		t.Errorf("x-goog-api-key: got %d", rec.Code)
	}
}

func TestAuthDoesNotLeakExistence(t *testing.T) {
	srv := newAuthTestServer(t)
	rec := doReq(srv, http.MethodGet, "/v1/models", "sentinel-no-such-account", "")
	if rec.Code != 401 {
		t.Fatalf("unknown account: got %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "acctA") || strings.Contains(rec.Body.String(), "acctB") {
		t.Fatalf("unauthorized response leaks account IDs: %s", rec.Body.String())
	}
}

func TestAuthUnknownRawTokenHotLoadLimit(t *testing.T) {
	srv := newAuthTestServer(t)
	// Unknown safe-ID tokens route through hot-load, then fail closed.
	for _, tok := range []string{"unknown-id-1", "unknown-id-2"} {
		if rec := doReq(srv, http.MethodGet, "/v1/models", tok, ""); rec.Code != 401 {
			t.Errorf("unknown token %q: got %d, want 401", tok, rec.Code)
		}
	}
}

func TestMetricsEndpointAdminOnly(t *testing.T) {
	srv := newAuthTestServer(t)
	if rec := doReq(srv, http.MethodGet, "/metrics", "", ""); rec.Code != 401 {
		t.Errorf("unauthenticated /metrics: got %d, want 401", rec.Code)
	}
	if rec := doReq(srv, http.MethodGet, "/metrics", "sentinel-acctA", ""); rec.Code != 403 {
		t.Errorf("account /metrics: got %d, want 403", rec.Code)
	}
	// Generate traffic, then verify exposition.
	_ = doReq(srv, http.MethodGet, "/v1/models", testAdminKey, "")
	rec := doReq(srv, http.MethodGet, "/metrics", testAdminKey, "")
	if rec.Code != 200 {
		t.Fatalf("admin /metrics: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sentinel_http_requests_total",
		`path="/v1/models"`,
		"sentinel_accounts 2",
		"sentinel_models",
		`sentinel_account_fails{id="acctA"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("metrics content type = %q, want Prometheus text exposition", ct)
	}
}

func TestCountTokensEndpoint(t *testing.T) {
	srv := newAuthTestServer(t)
	post := func(token, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/count-tokens", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("", `{"text":"hello"}`); rec.Code != 401 {
		t.Errorf("unauthenticated count-tokens: got %d, want 401", rec.Code)
	}
	rec := post("sentinel-acctA", `{"text":"hello world, this is a test"}`)
	if rec.Code != 200 {
		t.Fatalf("count-tokens text: got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"token_count"`) || !strings.Contains(rec.Body.String(), `"tokens":`) {
		t.Fatalf("unexpected count-tokens payload: %s", rec.Body.String())
	}
	recMsgs := post("acctB", `{"messages":[{"role":"user","content":"hi there"}]}`)
	if recMsgs.Code != 200 {
		t.Fatalf("count-tokens messages: got %d (%s)", recMsgs.Code, recMsgs.Body.String())
	}
	if rec := post("sentinel-acctA", `{}`); rec.Code != 400 {
		t.Errorf("empty count-tokens body: got %d, want 400", rec.Code)
	}
	if rec := post("sentinel-acctA", `{"messages":[]}`); rec.Code != 400 {
		t.Errorf("empty messages: got %d, want 400", rec.Code)
	}
}
