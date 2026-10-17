package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

var reSafeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// UpstreamClient abstracts an account's Gemini Web operations so unit tests can
// inject deterministic frame streams while production uses *gemini.AccountClient.
type UpstreamClient interface {
	Bootstrap(ctx context.Context) error
	AvailableModels(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error)
	GetQuota(ctx context.Context) []gemini.QuotaBucket
	UploadFile(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error)
	StreamGenerateLive(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error)
	RotateCookies(ctx context.Context) (*cookies.RotateResult, error)
	ReloadFromDiskIfNewer() bool
	InvalidateAuth()
}

// AccountState holds the server-side state and cached metadata for one Google account.
type AccountState struct {
	mu              sync.RWMutex
	ID              string
	Email           string
	FilePath        string
	CapturedAt      time.Time
	Client          UpstreamClient
	Models          []gemini.ModelInfo
	Capacity        int
	Quota           []gemini.QuotaBucket
	QuotaAt         time.Time
	quotaRefreshing bool
	ModelsAt        time.Time
	ModelsError     error
	Fails           int
	LastFail        time.Time
	LastError       string
	LastUsed        time.Time
}

// AuthScope represents the caller's resolved token scope.
type AuthScope struct {
	OK        bool
	AccountID string
	Admin     bool
}

type cachedUpload struct {
	File gemini.UploadedFile
	At   time.Time
}

// serverMetrics holds lightweight in-memory operational counters.
// Label values are fixed route paths and numeric statuses only — request
// bodies, tokens, and query strings are never recorded.
type serverMetrics struct {
	mu       sync.Mutex
	requests map[string]int64
}

// Server is the browserless sentinel HTTP daemon.
type Server struct {
	Cfg     config.Config
	Pool    *transport.Pool
	Rotator *cookies.Rotator

	mu             sync.RWMutex
	accounts       []*AccountState
	tokenToAcct    map[string]string
	sessionCache   map[string][]any
	sessionCacheAt map[string]time.Time
	sessionOrder   []string
	uploadCache    map[string]cachedUpload
	uploadOrder    []string
	lastHotLoadAt  time.Time
	clientFactory  func(a *AccountState) UpstreamClient
	metrics        *serverMetrics
}

// New constructs a new Server from cfg.
func New(cfg config.Config) *Server {
	maxSessions := cfg.MaxSessions
	if maxSessions <= 0 {
		maxSessions = 16
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			ProfileName:      cfg.ImpersonateProfile,
			ProxyURL:         cfg.ProxyURL,
			UserAgent:        cfg.UserAgent,
			SecCHUA:          cfg.SecCHUA,
			SecCHUAPlatform:  cfg.SecCHUAPlatform,
			Timeout:          cfg.StreamTimeout,
			FirstByteTimeout: cfg.FirstByteTimeout,
			StallTimeout:     cfg.StallTimeout,
		},
		MaxSessions: maxSessions,
		MaxQueue:    cfg.MaxQueueDepth,
		WaitTimeout: cfg.SessionWaitTimeout,
	})
	rotator := cookies.NewRotator()

	tokMap := make(map[string]string, len(cfg.AccountTokens))
	for k, v := range cfg.AccountTokens {
		tokMap[k] = v
	}

	s := &Server{
		Cfg:            cfg,
		Pool:           pool,
		Rotator:        rotator,
		tokenToAcct:    tokMap,
		sessionCache:   make(map[string][]any),
		sessionCacheAt: make(map[string]time.Time),
		uploadCache:    make(map[string]cachedUpload),
		metrics:        &serverMetrics{requests: make(map[string]int64)},
	}
	s.loadSessionCacheFromDisk()
	return s
}

// extractRequestToken extracts the caller's API key from Authorization Bearer,
// x-api-key, or x-goog-api-key headers. Query-string keys (?key=) are
// intentionally not accepted: URLs are logged by proxies and retained in
// shell history, so accepting secrets there would leak credentials.
func extractRequestToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	for _, h := range []string{"x-api-key", "x-goog-api-key"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

func tokenEquals(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ResolveAuthScope resolves the caller's API key into an AuthScope using
// constant-time comparisons. Empty, "public", and unknown tokens fail closed.
func (s *Server) ResolveAuthScope(r *http.Request) AuthScope {
	token := extractRequestToken(r)
	if token == "" || strings.EqualFold(strings.TrimSpace(token), "public") {
		return AuthScope{OK: false}
	}

	s.mu.RLock()
	for stored, acctID := range s.tokenToAcct {
		if tokenEquals(token, stored) {
			s.mu.RUnlock()
			return AuthScope{OK: true, AccountID: acctID}
		}
	}
	explicitTokens := len(s.Cfg.AccountTokens) > 0
	adminKey := s.Cfg.APIKey
	s.mu.RUnlock()

	// Admin check before hot-load: admin keys match reSafeID and must never
	// trigger a disk scan (which would also wipe in-memory state on empty dirs).
	if adminKey != "" && tokenEquals(token, adminKey) {
		return AuthScope{OK: true, Admin: true}
	}

	// Hot-load newly added account files on the fly when not locked by explicit SENTINEL_ACCOUNT_TOKENS,
	// rate-limited to avoid disk/SQLite scans on repeated invalid tokens.
	if !explicitTokens && (strings.HasPrefix(token, config.TokenPrefix) || reSafeID.MatchString(token)) {
		s.mu.RLock()
		canReload := time.Since(s.lastHotLoadAt) >= 2*time.Second
		s.mu.RUnlock()
		if canReload {
			s.mu.Lock()
			s.lastHotLoadAt = time.Now()
			s.mu.Unlock()
			before := s.Accounts()
			s.LoadCredentials()
			// Auto-load must never wipe in-memory accounts: a missing or
			// unreadable directory during a background request must not drop
			// live sessions. Explicit /v1/refresh may still replace the list.
			if len(s.Accounts()) == 0 && len(before) > 0 {
				s.SetAccounts(before)
			}
			s.mu.RLock()
			for stored, acctID := range s.tokenToAcct {
				if tokenEquals(token, stored) {
					foundAcct := acctID
					s.mu.RUnlock()
					return AuthScope{OK: true, AccountID: foundAcct}
				}
			}
			s.mu.RUnlock()
			// Fall through to the admin-key check below: admin tokens match
			// reSafeID and must not fail closed here.
		}
	}
	if s.Cfg.APIKey != "" && tokenEquals(token, s.Cfg.APIKey) {
		return AuthScope{OK: true, Admin: true}
	}
	return AuthScope{OK: false}
}

// requireAdmin enforces admin authorization for privileged endpoints.
// Account-scoped tokens receive 403 without revealing account metadata;
// missing or invalid tokens receive 401.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (AuthScope, bool) {
	scope := s.ResolveAuthScope(r)
	if !scope.OK {
		sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
		return scope, false
	}
	if !scope.Admin {
		sendJSON(w, 403, map[string]any{"error": map[string]any{"message": "admin authorization required", "type": "authentication_error"}}, nil)
		return scope, false
	}
	return scope, true
}

// adminDeniedStatus re-resolves the scope to report the right log status
// (401 unauthenticated vs 403 authenticated-but-not-admin) without
// re-sending a response.
func (s *Server) adminDeniedStatus(r *http.Request) int {
	if scope := s.ResolveAuthScope(r); scope.OK {
		return 403
	}
	return 401
}

// recordRequest counts one completed HTTP exchange by method, route, status.
func (s *Server) recordRequest(method, path string, status int) {
	if s == nil || s.metrics == nil {
		return
	}
	key := method + " " + path + " " + strconv.Itoa(status)
	s.metrics.mu.Lock()
	s.metrics.requests[key]++
	s.metrics.mu.Unlock()
}

func promLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// serveMetrics renders Prometheus text exposition for operational counters
// plus account/model gauges. Admin authorization only.
func (s *Server) serveMetrics(w http.ResponseWriter) {
	var b strings.Builder
	b.WriteString("# HELP sentinel_http_requests_total Total HTTP requests by method, path and status.\n")
	b.WriteString("# TYPE sentinel_http_requests_total counter\n")
	s.metrics.mu.Lock()
	keys := make([]string, 0, len(s.metrics.requests))
	for k := range s.metrics.requests {
		keys = append(keys, k)
	}
	s.metrics.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, " ", 3)
		if len(parts) != 3 {
			continue
		}
		s.metrics.mu.Lock()
		n := s.metrics.requests[k]
		s.metrics.mu.Unlock()
		fmt.Fprintf(&b, "sentinel_http_requests_total{method=%q,path=%q,status=%q} %d\n",
			promLabel(parts[0]), promLabel(parts[1]), promLabel(parts[2]), n)
	}
	accts := s.Accounts()
	fmt.Fprintf(&b, "# HELP sentinel_accounts Loaded accounts.\n# TYPE sentinel_accounts gauge\nsentinel_accounts %d\n", len(accts))
	fmt.Fprintf(&b, "# HELP sentinel_models Known models across accounts.\n# TYPE sentinel_models gauge\nsentinel_models %d\n", len(s.allModels()))
	b.WriteString("# HELP sentinel_account_fails Consecutive failures per account.\n# TYPE sentinel_account_fails gauge\n")
	for _, a := range accts {
		a.mu.RLock()
		id, fails := a.ID, a.Fails
		a.mu.RUnlock()
		fmt.Fprintf(&b, "sentinel_account_fails{id=%q} %d\n", promLabel(id), fails)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(200)
	_, _ = io.WriteString(w, b.String())
}

// handleCountTokens estimates token usage for a prompt without calling the
// upstream. Accepts {"text": "..."}, {"input": "..."}, or OpenAI-style
// {"messages": [...]} and reports the same ~4-bytes-per-token estimate used
// in chat usage objects, so agents can budget before sending.
func (s *Server) handleCountTokens(w http.ResponseWriter, body map[string]any) {
	var msgs []map[string]any
	if raw, ok := body["messages"].([]any); ok && len(raw) > 0 {
		for _, mAny := range raw {
			m, ok := mAny.(map[string]any)
			if !ok || m == nil {
				sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "messages must be message objects", "type": "invalid_request_error"}}, nil)
				return
			}
			msgs = append(msgs, m)
		}
	} else if t, ok := body["text"].(string); ok && strings.TrimSpace(t) != "" {
		msgs = []map[string]any{{"role": "user", "content": t}}
	} else if in, ok := body["input"].(string); ok && strings.TrimSpace(in) != "" {
		msgs = []map[string]any{{"role": "user", "content": in}}
	} else {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "provide text, input, or a nonempty messages array", "type": "invalid_request_error"}}, nil)
		return
	}
	if len(msgs) > 500 {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "too many messages: max 500", "type": "invalid_request_error"}}, nil)
		return
	}
	flat, _, err := prompt.FlattenMessages(msgs, nil, nil, prompt.FlattenOptions{})
	if err != nil {
		sendChatError(w, false, err)
		return
	}
	sendJSON(w, 200, map[string]any{
		"object":     "token_count",
		"tokens":     prompt.EstimateTokens(flat),
		"characters": len(flat),
	}, nil)
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(b[:])
}

// ServeHTTP implements http.Handler for all server endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	ctx := r.Context()
	reqID := newRequestID()
	w.Header().Set("X-Request-ID", reqID)
	start := time.Now()
	status := 0
	quiet := s.Cfg.LogLevel == "error" || s.Cfg.LogLevel == "silent" || s.Cfg.LogLevel == "off"
	defer func() {
		if !quiet {
			// Safe operational log: endpoint, method, status, duration, request
			// ID. Never logs tokens, cookies, prompts, or query strings.
			log.Printf("[http] id=%s method=%s path=%s status=%d duration_ms=%d",
				reqID, r.Method, p, status, time.Since(start).Milliseconds())
		}
		s.recordRequest(r.Method, p, status)
	}()
	respond := func(code int, obj any, extra map[string]string) {
		status = code
		sendJSON(w, code, obj, extra)
	}

	switch {
	case r.Method == http.MethodGet && (p == "/health" || p == "/"):
		accts := s.Accounts()
		models := len(s.allModels())
		respond(200, map[string]any{
			"status":    "ok",
			"service":   "sentinel",
			"transport": "chrome-utls-h2",
			"profile":   s.Cfg.ImpersonateProfile,
			"accounts":  len(accts),
			"models":    models,
			"upstream":  "gemini.google.com",
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/sessions" || p == "/sessions"):
		if _, ok := s.requireAdmin(w, r); !ok {
			status = s.adminDeniedStatus(r)
			return
		}
		status = 200
		sendJSON(w, 200, map[string]any{
			"transport": "chrome-utls-h2",
			"profile":   s.Cfg.ImpersonateProfile,
			"sessions":  s.Pool.Status(),
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/accounts" || p == "/accounts"):
		if _, ok := s.requireAdmin(w, r); !ok {
			status = s.adminDeniedStatus(r)
			return
		}
		s.EnsureModels(ctx, false)
		pool := s.Accounts()
		list := make([]map[string]any, 0, len(pool))
		for _, a := range pool {
			a.mu.RLock()
			mNames := make([]string, 0, len(a.Models))
			for _, m := range a.Models {
				mNames = append(mNames, m.ModelName)
			}
			list = append(list, map[string]any{
				"id":       a.ID,
				"email":    a.Email,
				"capacity": a.Capacity,
				"fails":    a.Fails,
				"models":   mNames,
				"quota":    a.Quota,
			})
			a.mu.RUnlock()
		}
		status = 200
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/quota" || p == "/quota"):
		if _, ok := s.requireAdmin(w, r); !ok {
			status = s.adminDeniedStatus(r)
			return
		}
		s.EnsureModels(ctx, false)
		pool := s.Accounts()
		var wg sync.WaitGroup
		for _, a := range pool {
			wg.Add(1)
			go func(acct *AccountState) {
				defer wg.Done()
				q := s.clientFor(acct).GetQuota(ctx)
				if len(q) > 0 {
					s.mergeAccountQuota(acct, q)
				}
			}(a)
		}
		wg.Wait()
		list := make([]map[string]any, 0, len(pool))
		for _, a := range pool {
			a.mu.RLock()
			list = append(list, map[string]any{
				"id":    a.ID,
				"email": a.Email,
				"quota": a.Quota,
			})
			a.mu.RUnlock()
		}
		status = 200
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/models" || p == "/models"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			status = 401
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		accts := s.EnsureModels(ctx, false)
		if len(accts) == 0 {
			status = 503
			sendJSON(w, 503, map[string]any{"error": map[string]any{"message": "no accounts configured", "type": "auth_unavailable"}}, nil)
			return
		}
		pool := s.scopedAccounts(scope)
		allEmpty := len(pool) > 0
		var firstErr error
		for _, a := range pool {
			a.mu.RLock()
			if len(a.Models) > 0 {
				allEmpty = false
			}
			if firstErr == nil && a.ModelsError != nil {
				firstErr = a.ModelsError
			}
			a.mu.RUnlock()
		}
		if allEmpty && firstErr != nil {
			status = 502
			sendChatError(w, false, firstErr)
			return
		}

		seen := make(map[string]bool)
		var models []gemini.ModelInfo
		for _, a := range pool {
			a.mu.RLock()
			for _, m := range a.Models {
				if !seen[m.ModelName] {
					seen[m.ModelName] = true
					models = append(models, m)
				}
			}
			a.mu.RUnlock()
		}
		nowSec := time.Now().Unix()
		data := make([]map[string]any, 0, len(models))
		for _, x := range models {
			isReasoning := gemini.IsReasoningModel(x.ModelName)
			defThinkStr := "low"
			if isReasoning {
				defThinkStr = "high"
			}
			entry := map[string]any{
				"id":                    x.ModelName,
				"object":                "model",
				"created":               nowSec,
				"owned_by":              "google",
				"context_length":        1048576,
				"context_window":        1048576,
				"max_completion_tokens": 65536,
				"reasoning":             isReasoning,
				"thinking": map[string]any{
					"mode":           "effort",
					"default":        defThinkStr,
					"defaultLevel":   defThinkStr,
					"efforts":        gemini.SupportedThinkingEfforts,
					"requiresEffort": true,
				},
				"compat": map[string]any{
					"supportsReasoningEffort": isReasoning,
				},
				"capabilities": map[string]any{
					"vision":                  true,
					"pdf":                     false,
					"audioInput":              false,
					"videoInput":              false,
					"imageOutput":             false,
					"audioOutput":             false,
					"search":                  true,
					"tools":                   true,
					"reasoning":               isReasoning,
					"thinkingFormat":          "gemini-level",
					"thinkingCanDisable":      false,
					"thinkingEffortSupported": isReasoning,
					"thinkingLevels":          gemini.SupportedThinkingEfforts,
					"defaultThinkingLevel":    defThinkStr,
					"contextWindow":           1048576,
					"maxOutput":               65536,
				},
			}
			data = append(data, entry)
		}
		sendJSON(w, 200, map[string]any{"object": "list", "data": data}, nil)
		status = 200
		return

	case r.Method == http.MethodGet && p == "/metrics":
		if _, ok := s.requireAdmin(w, r); !ok {
			status = s.adminDeniedStatus(r)
			return
		}
		status = 200
		s.serveMetrics(w)
		return

	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/chat/completions"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			status = 401
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		status = 200
		body, ok := readJSONBody(w, r)
		if !ok {
			status = 400
			return
		}
		s.HandleChat(ctx, w, body, scope)
		return

	case r.Method == http.MethodPost && (p == "/v1/responses" || p == "/responses"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			status = 401
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		status = 200
		body, ok := readJSONBody(w, r)
		if !ok {
			status = 400
			return
		}
		s.HandleResponses(ctx, w, body, scope)
		return
	case r.Method == http.MethodPost && p == "/v1/count-tokens":
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			status = 401
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		status = 200
		body, ok := readJSONBody(w, r)
		if !ok {
			status = 400
			return
		}
		s.handleCountTokens(w, body)
		return
	case r.Method == http.MethodPost && (p == "/v1/refresh" || p == "/refresh"):
		if r.Header.Get("Origin") != "" {
			status = 403
			sendJSON(w, 403, map[string]any{"error": map[string]any{"message": "origin not allowed", "type": "authentication_error"}}, nil)
			return
		}
		if _, ok := s.requireAdmin(w, r); !ok {
			status = s.adminDeniedStatus(r)
			return
		}
		s.LoadCredentials()
		accts := s.EnsureModels(ctx, true)
		status = 200
		sendJSON(w, 200, map[string]any{
			"status":   "ok",
			"accounts": len(accts),
			"models":   len(s.allModels()),
		}, nil)
		return

	default:
		status = 404
		sendJSON(w, 404, map[string]any{
			"error": map[string]any{
				"message": "not found: " + p,
				"type":    "invalid_request_error",
			},
		}, nil)
	}
}

func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	const maxRequestBodyBytes = 32 * 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid request body", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	if len(raw) > maxRequestBodyBytes {
		sendJSON(w, 413, map[string]any{"error": map[string]any{"message": "request body exceeds 32MB limit", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	var body map[string]any
	if len(raw) == 0 {
		body = map[string]any{}
	} else if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	return body, true
}

// StartBackgroundWorkers starts periodic model warming, dual-path cookie rotation,
// and idle session reaping until ctx is cancelled.
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	// Warm model registry periodically
	refreshEvery := s.Cfg.ModelRefreshEvery
	if refreshEvery <= 0 {
		refreshEvery = 10 * time.Minute
	}
	go func() {
		wCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.EnsureModels(wCtx, false)
		cancel()
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rCtx, rCancel := context.WithTimeout(ctx, 45*time.Second)
				s.EnsureModels(rCtx, true)
				rCancel()
			}
		}
	}()

	// Reap idle transport sessions every 60 seconds
	if s.Cfg.IdleSessionTimeout > 0 {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.Pool.ReapIdle(s.Cfg.IdleSessionTimeout)
				}
			}
		}()
	}

	// Proactive dual-path cookie rotation (__Secure-1PSIDTS + SIDCC)
	if s.Cfg.RotateCookiesEvery > 0 {
		go func() {
			ticker := time.NewTicker(s.Cfg.RotateCookiesEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					for _, a := range s.Accounts() {
						if s.Pool != nil {
							if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
								continue
							}
						}
						c := s.clientFor(a)
						rCtx, rCancel := context.WithTimeout(ctx, 15*time.Second)
						res, err := c.RotateCookies(rCtx)
						rCancel()
						if err != nil {
							log.Printf("[cookies] %s rotate error: %v", a.ID, err)
						} else if res != nil && res.Rotated {
							log.Printf("[cookies] %s rotated session cookies (HTTP %d)", a.ID, res.Status)
						}
					}
				}
			}
		}()
	}
}

func retryHeaders(hdrs map[string]string) map[string]string {
	if len(hdrs) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, k := range []string{"retry-after", "x-resets-at"} {
		val := hdrs[k]
		if val == "" {
			if k == "retry-after" {
				val = hdrs["Retry-After"]
			} else {
				val = hdrs["X-Resets-At"]
			}
		}
		if val != "" && !strings.ContainsAny(val, "\r\n") {
			out[k] = val
		}
	}
	return out
}

func sendJSON(w http.ResponseWriter, status int, obj any, extraHeaders map[string]string) {
	b, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func sendChatError(w http.ResponseWriter, headersSent bool, err error) {
	status := 502
	code := ""
	var hdrs map[string]string
	var ue *transport.UpstreamError
	if errors.As(err, &ue) && ue != nil {
		if ue.StatusCode >= 400 && ue.StatusCode <= 599 {
			status = ue.StatusCode
		}
		code = ue.Code
		hdrs = ue.Headers
	}
	errType := "upstream_error"
	switch status {
	case 429:
		errType = "rate_limit_error"
	case 401, 403:
		errType = "authentication_error"
	case 400:
		errType = "invalid_request_error"
	}
	if code == "" {
		code = errType
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    errType,
			"code":    code,
		},
	}
	if headersSent {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		sendJSON(w, status, payload, retryHeaders(hdrs))
	}
}
