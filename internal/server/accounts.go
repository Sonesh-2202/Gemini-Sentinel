package server

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
	"github.com/sonesh-2202/gemini-sentinel/internal/toolbridge"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

// SetAccounts replaces the server's loaded account list (used by tests).
func (s *Server) SetAccounts(accts []*AccountState) {
	s.mu.Lock()
	s.accounts = accts
	if len(s.Cfg.AccountTokens) == 0 {
		for _, a := range accts {
			if a != nil && a.ID != "" {
				s.registerAccountTokensLocked(a.ID, a.ID)
			}
		}
	}
	s.mu.Unlock()
}

func (s *Server) registerAccountTokensLocked(rawID, targetID string) {
	if rawID == "" || targetID == "" {
		return
	}
	for _, tok := range []string{config.TokenPrefix + rawID, rawID} {
		if _, explicit := s.Cfg.AccountTokens[tok]; !explicit {
			s.tokenToAcct[tok] = targetID
		}
	}
}

// Accounts returns a snapshot of the loaded accounts.
func (s *Server) Accounts() []*AccountState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AccountState, len(s.accounts))
	copy(out, s.accounts)
	return out
}

func (s *Server) scopedAccounts(scope AuthScope) []*AccountState {
	accts := s.Accounts()
	if scope.AccountID == "" {
		return accts
	}
	var out []*AccountState
	for _, a := range accts {
		if a.ID == scope.AccountID {
			out = append(out, a)
		}
	}
	return out
}

// LoadCredentials loads and deduplicates account JSON files from AccountsDir and CredFile,
// falling back to live Chrome SQLite decryption if no saved files exist.
func (s *Server) LoadCredentials() int {
	var rawList []*AccountState

	credFile := filepath.Join(s.Cfg.HomeDir, "web-credentials.json")
	if entries, err := os.ReadDir(s.Cfg.AccountsDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, fn := range names {
			id := strings.TrimSuffix(fn, ".json")
			if !reSafeID.MatchString(id) {
				continue
			}
			fullPath := filepath.Join(s.Cfg.AccountsDir, fn)
			if st, err := s.loadAccountStateFromFile(id, fullPath); err == nil {
				rawList = append(rawList, st)
			}
		}
	}

	if st, err := s.loadAccountStateFromFile("default", credFile); err == nil {
		rawList = append(rawList, st)
	}

	if len(rawList) == 0 && s.Cfg.CookieDB != "" {
		if dbPath := cookies.FindChromeCookieDB(s.Cfg.CookieDB); dbPath != "" {
			if hdr, err := cookies.ReadChromeCookies(dbPath); err == nil && cookies.ExtractSAPISID(hdr) != "" {
				ac := gemini.NewAccountClient("chrome-live", hdr, "", s.Pool, s.Rotator)
				rawList = append(rawList, &AccountState{
					ID:     "chrome-live",
					Email:  "chrome-profile",
					Client: ac,
				})
			}
		}
	}

	deduped := dedupeAccounts(rawList, credFile)

	s.mu.Lock()
	s.accounts = deduped
	// Auto-populate raw <id> so 9router and direct harnesses work out of the box.
	byEmail := make(map[string]string, len(deduped))
	for _, a := range deduped {
		s.registerAccountTokensLocked(a.ID, a.ID)
		if a.Email != "" {
			byEmail[strings.ToLower(strings.TrimSpace(a.Email))] = a.ID
		}
	}
	for _, raw := range rawList {
		if targetID, ok := byEmail[strings.ToLower(strings.TrimSpace(raw.Email))]; ok {
			s.registerAccountTokensLocked(raw.ID, targetID)
		}
	}
	s.mu.Unlock()
	return len(deduped)
}

func (s *Server) loadAccountStateFromFile(id, path string) (*AccountState, error) {
	ac, err := gemini.LoadAccountFromFile(path, s.Pool, s.Rotator)
	if err != nil {
		return nil, err
	}
	ac.ID = id
	rec, _, _ := cookies.LoadAccountFile(path)
	var capAt time.Time
	if rec != nil && rec.CapturedAt != "" {
		capAt, _ = time.Parse(time.RFC3339, rec.CapturedAt)
	}
	return &AccountState{
		ID:         id,
		Email:      ac.Email(),
		FilePath:   path,
		CapturedAt: capAt,
		Client:     ac,
	}, nil
}

func dedupeAccounts(list []*AccountState, credFile string) []*AccountState {
	byEmail := make(map[string]*AccountState)
	var out []*AccountState
	for _, a := range list {
		key := strings.ToLower(strings.TrimSpace(a.Email))
		if key == "" {
			out = append(out, a)
			continue
		}
		prev, exists := byEmail[key]
		if !exists {
			byEmail[key] = a
			out = append(out, a)
			continue
		}
		aNamed := a.FilePath != credFile
		prevNamed := prev.FilePath != credFile
		newer := a.CapturedAt.After(prev.CapturedAt)
		tieNamed := a.CapturedAt.Equal(prev.CapturedAt) && aNamed && !prevNamed
		if newer || tieNamed {
			byEmail[key] = a
			for i := range out {
				if out[i] == prev {
					out[i] = a
					break
				}
			}
		}
	}
	return out
}

func (s *Server) clientFor(a *AccountState) UpstreamClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.clientFactory != nil {
		c := s.clientFactory(a)
		if c != nil {
			a.Client = c
			return c
		}
	}
	if a.Client != nil {
		a.Client.ReloadFromDiskIfNewer()
		return a.Client
	}
	if a.FilePath != "" {
		if ac, err := gemini.LoadAccountFromFile(a.FilePath, s.Pool, s.Rotator); err == nil {
			ac.ID = a.ID
			a.Client = ac
			return ac
		}
	}
	ac := gemini.NewAccountClient(a.ID, "", a.FilePath, s.Pool, s.Rotator)
	a.Client = ac
	return ac
}

// RefreshAccount refreshes the live model list and quota for a single account,
// preserving the last known good state on failure.
func (s *Server) RefreshAccount(ctx context.Context, a *AccountState, force bool) *AccountState {
	a.mu.RLock()
	hasModels := len(a.Models) > 0
	fresh := !a.ModelsAt.IsZero() && time.Since(a.ModelsAt) < 10*time.Minute
	a.mu.RUnlock()
	if !force && hasModels && fresh {
		return a
	}

	c := s.clientFor(a)
	am, err := c.AvailableModels(ctx, force)
	if err != nil || am == nil || am.Status != 200 || len(am.Models) == 0 {
		if err == nil {
			err = transport.NewUpstreamError("invalid upstream model registry", 502, "model_discovery_error", nil, nil)
		}
		a.mu.Lock()
		a.ModelsError = err
		a.mu.Unlock()
		return a
	}

	quota := c.GetQuota(ctx)

	a.mu.Lock()
	a.Models = am.Models
	a.Capacity = am.Capacity
	a.ModelsAt = time.Now()
	a.ModelsError = nil
	a.mu.Unlock()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	}
	return a
}

// EnsureModels refreshes all accounts concurrently.
func (s *Server) EnsureModels(ctx context.Context, force bool) []*AccountState {
	accts := s.Accounts()
	var wg sync.WaitGroup
	for _, a := range accts {
		wg.Add(1)
		go func(acct *AccountState) {
			defer wg.Done()
			s.RefreshAccount(ctx, acct, force)
		}(a)
	}
	wg.Wait()
	return accts
}

func (s *Server) allModels() []gemini.ModelInfo {
	accts := s.Accounts()
	seen := make(map[string]bool)
	var out []gemini.ModelInfo
	for _, a := range accts {
		a.mu.RLock()
		for _, m := range a.Models {
			if !seen[m.ModelName] {
				seen[m.ModelName] = true
				out = append(out, m)
			}
		}
		a.mu.RUnlock()
	}
	return out
}

func resolveModelOnAccount(a *AccountState, wanted string) *gemini.ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	w := strings.TrimSpace(wanted)
	if w == "" {
		if len(a.Models) > 0 {
			m := a.Models[0]
			return &m
		}
		return nil
	}
	for _, m := range a.Models {
		if gemini.MatchesModel(m, w) {
			found := m
			return &found
		}
	}
	return nil
}

func (s *Server) checkPreFlightQuota(a *AccountState, model gemini.ModelInfo, mid string) error {
	if !s.Cfg.RejectDowngrade {
		return nil
	}
	name := strings.ToLower(model.ModelName)
	if strings.Contains(name, "lite") {
		return nil
	}
	a.mu.RLock()
	quota := a.Quota
	a.mu.RUnlock()
	if len(quota) == 0 {
		return nil
	}
	now := time.Now()
	family := modelFamilyTokens(name)
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if q.Unlimited || q.Total <= 0 || q.Remaining > 0 || q.ResetAt == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
			spent = append(spent, q)
		}
	}
	if len(spent) == 0 {
		return nil
	}
	var related []gemini.QuotaBucket
	for _, q := range spent {
		lbl := strings.ToLower(q.Label)
		for _, tok := range family {
			if strings.Contains(lbl, tok) {
				related = append(related, q)
				break
			}
		}
	}
	if len(family) > 0 && len(related) == 0 {
		return nil
	}
	activeSpent := spent
	if len(related) > 0 {
		activeSpent = related
	}
	var earliest time.Time
	for _, q := range activeSpent {
		if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	}
	retrySec := int(math.Ceil(earliest.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return transport.NewUpstreamError(
		fmt.Sprintf("quota exceeded: %s is exhausted on account %s until %s", mid, a.ID, earliest.UTC().Format(time.RFC3339)),
		429,
		"rate_limit_error",
		map[string]string{
			"retry-after": strconv.Itoa(retrySec),
			"x-resets-at": earliest.UTC().Format(time.RFC3339),
		},
		nil,
	)
}

func (s *Server) classifyEmpty(ctx context.Context, a *AccountState, modelName string) error {
	c := s.clientFor(a)
	qCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	quota := c.GetQuota(qCtx)
	cancel()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	} else {
		a.mu.RLock()
		quota = a.Quota
		a.mu.RUnlock()
	}
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if !q.Unlimited && q.Total > 0 && q.Remaining <= 0 {
			spent = append(spent, q)
		}
	}
	if len(spent) > 0 {
		family := modelFamilyTokens(modelName)
		var related []gemini.QuotaBucket
		for _, q := range spent {
			lbl := strings.ToLower(q.Label)
			for _, tok := range family {
				if strings.Contains(lbl, tok) {
					related = append(related, q)
					break
				}
			}
		}
		if len(family) > 0 && len(related) == 0 {
			return transport.NewUpstreamError("empty upstream response", 502, "empty_upstream_response", nil, nil)
		}
		chosen := spent
		if len(related) > 0 {
			chosen = related
		}
		labels := make([]string, 0, len(chosen))
		var earliest time.Time
		now := time.Now()
		for _, q := range chosen {
			labels = append(labels, q.Label)
			if q.ResetAt != "" {
				if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
					if earliest.IsZero() || t.Before(earliest) {
						earliest = t
					}
				}
			}
		}
		var hdrs map[string]string
		if !earliest.IsZero() {
			retrySec := int(math.Ceil(earliest.Sub(now).Seconds()))
			if retrySec < 1 {
				retrySec = 1
			}
			hdrs = map[string]string{
				"retry-after": strconv.Itoa(retrySec),
				"x-resets-at": earliest.UTC().Format(time.RFC3339),
			}
		}
		ident := a.Email
		if ident == "" {
			ident = a.ID
		}
		return transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: %s exhausted on %s", strings.Join(labels, ", "), ident),
			429,
			"rate_limit_error",
			hdrs,
			nil,
		)
	}
	return transport.NewUpstreamError("empty upstream response", 502, "empty_upstream_response", nil, nil)
}

func modelFamilyTokens(modelName string) []string {
	name := strings.ToLower(modelName)
	var family []string
	if strings.Contains(name, "argon") {
		family = append(family, "argon", "pro")
	}
	for _, tok := range []string{"pro", "flash", "thinking"} {
		if strings.Contains(name, tok) {
			family = append(family, tok)
		}
	}
	return family
}

func (s *Server) recordModelExhausted(a *AccountState, modelName string) {
	if a == nil || modelName == "" {
		return
	}
	subID := "sub:" + strings.ToLower(strings.TrimSpace(modelName))
	resetAt := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	bucket := gemini.QuotaBucket{
		ID:        subID,
		Label:     modelName,
		Total:     1,
		Remaining: 0,
		ResetAt:   resetAt,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, q := range a.Quota {
		if q.ID == subID {
			a.Quota[i] = bucket
			return
		}
	}
	a.Quota = append(a.Quota, bucket)
}

func (s *Server) mergeAccountQuota(a *AccountState, fresh []gemini.QuotaBucket) []gemini.QuotaBucket {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	merged := make([]gemini.QuotaBucket, 0, len(fresh)+2)
	merged = append(merged, fresh...)
	for _, old := range a.Quota {
		if strings.HasPrefix(old.ID, "sub:") && old.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, old.ResetAt); err == nil && t.After(now) {
				merged = append(merged, old)
			}
		}
	}
	a.Quota = merged
	a.QuotaAt = now
	return merged
}

func (s *Server) isAccountBusy(acctID string) bool {
	if s.Pool == nil || acctID == "" {
		return false
	}
	if sess := s.Pool.Existing(acctID); sess != nil {
		return sess.Busy()
	}
	return false
}

func (s *Server) hasSessionCacheHit(a *AccountState, modelName string, hasher *prompt.HistoryHasher) bool {
	if !s.Cfg.SessionCache || a == nil || modelName == "" || hasher == nil || hasher.Len() < 3 {
		return false
	}
	for split := hasher.Len() - 1; split >= 2; split-- {
		if len(s.getSessionCache(hasher.HashPrefix(a.ID, modelName, split))) > 0 {
			return true
		}
	}
	return false
}

func (s *Server) pickFallbackAccount(accts []*AccountState, tried map[string]bool, requested string, hasher *prompt.HistoryHasher) (*AccountState, *gemini.ModelInfo) {
	var idleCand *AccountState
	var idleModel *gemini.ModelInfo
	var busyCand *AccountState
	var busyModel *gemini.ModelInfo
	for _, cand := range accts {
		if tried[cand.ID] {
			continue
		}
		m := resolveModelOnAccount(cand, requested)
		if m == nil {
			continue
		}
		if err := s.checkPreFlightQuota(cand, *m, m.ModelName); err != nil {
			continue
		}
		if !s.isAccountBusy(cand.ID) {
			if s.hasSessionCacheHit(cand, m.ModelName, hasher) {
				return cand, m
			}
			cand.mu.RLock()
			cFails := cand.Fails
			cand.mu.RUnlock()
			if idleCand == nil {
				idleCand = cand
				idleModel = m
			} else {
				idleCand.mu.RLock()
				iFails := idleCand.Fails
				idleCand.mu.RUnlock()
				if cFails < iFails {
					idleCand = cand
					idleModel = m
				}
			}
			continue
		}
		if busyCand == nil {
			busyCand = cand
			busyModel = m
		}
	}
	if idleCand != nil {
		return idleCand, idleModel
	}
	return busyCand, busyModel
}

func (s *Server) lookupQuotaResetHeaders(_ context.Context, a *AccountState, modelName string) map[string]string {
	a.mu.RLock()
	quota := a.Quota
	a.mu.RUnlock()
	if len(quota) == 0 {
		return nil
	}

	family := modelFamilyTokens(modelName)
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if !q.Unlimited && q.Total > 0 && q.Remaining <= 0 {
			spent = append(spent, q)
		}
	}
	var related []gemini.QuotaBucket
	for _, q := range spent {
		lbl := strings.ToLower(q.Label)
		for _, tok := range family {
			if strings.Contains(lbl, tok) {
				related = append(related, q)
				break
			}
		}
	}
	chosen := spent
	if len(related) > 0 {
		chosen = related
	}
	now := time.Now()
	var earliest time.Time
	for _, q := range chosen {
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
			}
		}
	}
	if earliest.IsZero() {
		return nil
	}
	retrySec := int(math.Ceil(earliest.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return map[string]string{
		"retry-after": strconv.Itoa(retrySec),
		"x-resets-at": earliest.UTC().Format(time.RFC3339),
	}
}

func (s *Server) markAccountFailure(a *AccountState, msg string) {
	a.mu.Lock()
	a.Fails++
	a.LastFail = time.Now()
	a.LastError = msg
	a.mu.Unlock()
}

func (s *Server) resolveChatAccount(accts []*AccountState, scope AuthScope, requested string, hasher *prompt.HistoryHasher) *AccountState {
	if scope.AccountID != "" {
		for _, a := range accts {
			if a.ID == scope.AccountID {
				return a
			}
		}
		return nil
	}
	if !scope.Admin {
		s.mu.RLock()
		explicitTokens := len(s.Cfg.AccountTokens) > 0
		s.mu.RUnlock()
		if explicitTokens {
			return nil
		}
	}
	var best *AccountState
	bestBusy := false
	bestHasCache := false
	for _, a := range accts {
		aBusy := s.isAccountBusy(a.ID)
		aHasCache := false
		if m := resolveModelOnAccount(a, requested); m != nil {
			if s.checkPreFlightQuota(a, *m, m.ModelName) == nil {
				aHasCache = s.hasSessionCacheHit(a, m.ModelName, hasher)
			}
		}
		if best == nil {
			best = a
			bestBusy = aBusy
			bestHasCache = aHasCache
			continue
		}
		if bestBusy && !aBusy {
			best = a
			bestBusy = false
			bestHasCache = aHasCache
			continue
		}
		if !bestBusy && aBusy {
			continue
		}
		if aHasCache && !bestHasCache {
			best = a
			bestBusy = aBusy
			bestHasCache = true
			continue
		}
		if bestHasCache && !aHasCache {
			continue
		}
		a.mu.RLock()
		aFails, aUsed := a.Fails, a.LastUsed
		a.mu.RUnlock()
		best.mu.RLock()
		bFails, bUsed := best.Fails, best.LastUsed
		best.mu.RUnlock()
		if aFails < bFails || (aFails == bFails && aUsed.Before(bUsed)) {
			best = a
			bestBusy = aBusy
			bestHasCache = aHasCache
		}
	}
	return best
}

const quotaRefreshCooldown = 45 * time.Second

func (s *Server) refreshQuotaAsync(a *AccountState, c UpstreamClient) {
	if a == nil || c == nil {
		return
	}
	a.mu.Lock()
	if a.quotaRefreshing {
		a.mu.Unlock()
		return
	}
	lowQuota := a.Fails > 0
	if !lowQuota {
		for _, q := range a.Quota {
			if !q.Unlimited && q.Total > 0 && q.Remaining <= 2 {
				lowQuota = true
				break
			}
		}
	}
	if !lowQuota && !a.QuotaAt.IsZero() && time.Since(a.QuotaAt) < quotaRefreshCooldown {
		a.mu.Unlock()
		return
	}
	a.quotaRefreshing = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.quotaRefreshing = false
			a.mu.Unlock()
		}()
		if s.Pool != nil {
			if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
				return
			}
		}
		qCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		q := c.GetQuota(qCtx)
		if len(q) > 0 {
			s.mergeAccountQuota(a, q)
		}
	}()
}

type preparedGeneration struct {
	accts               []*AccountState
	account             *AccountState
	model               *gemini.ModelInfo
	mid                 string
	requested           string
	contract            *toolbridge.Contract
	hasTools            bool
	stream              bool
	buffered            bool
	opts                gemini.StreamOptions
	fullPrompt          string
	curPrompt           string
	overflowParts       []prompt.AttachmentPart
	imageParts          []prompt.AttachmentPart
	fullAttachmentParts []prompt.AttachmentPart
	attachmentParts     []prompt.AttachmentPart
	hasher              *prompt.HistoryHasher
}

func (s *Server) prepareGeneration(
	ctx context.Context,
	body map[string]any,
	scope AuthScope,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
) (*preparedGeneration, error) {
	const maxMessages = 500
	if len(messages) == 0 {
		return nil, transport.NewUpstreamError("messages must be a nonempty array", 400, "invalid_request_error", nil, nil)
	}
	if len(messages) > maxMessages {
		return nil, transport.NewUpstreamError(
			fmt.Sprintf("too many messages: got %d, max %d", len(messages), maxMessages),
			400, "invalid_request_error", nil, nil)
	}
	rawModel, _ := body["model"].(string)
	requested, inlineThink := gemini.ParseModelRequest(rawModel)

	accts := s.Accounts()
	if len(accts) == 0 {
		return nil, transport.NewUpstreamError("no accounts configured", 503, "auth_unavailable", nil, nil)
	}

	var hasher *prompt.HistoryHasher
	if s.Cfg.SessionCache && len(messages) >= 3 {
		hasher = prompt.NewHistoryHasher(messages, toolsRaw, toolChoiceRaw)
	}

	account := s.resolveChatAccount(accts, scope, requested, hasher)
	if account == nil {
		msg := "this endpoint requires a per-account token; the admin key cannot serve chat"
		if scope.AccountID != "" {
			msg = fmt.Sprintf("token is bound to account %q, which is not loaded", scope.AccountID)
		}
		return nil, transport.NewUpstreamError(msg, 401, "authentication_error", nil, nil)
	}

	contract, err := toolbridge.ValidateToolRequest(toolsRaw, toolChoiceRaw)
	if err != nil {
		return nil, err
	}

	account.mu.RLock()
	hasModels := len(account.Models) > 0
	account.mu.RUnlock()
	if !hasModels {
		s.RefreshAccount(ctx, account, false)
	}

	account.mu.RLock()
	hasModels = len(account.Models) > 0
	mErr := account.ModelsError
	account.mu.RUnlock()
	if !hasModels && mErr != nil {
		return nil, mErr
	}

	model := resolveModelOnAccount(account, requested)
	if model == nil {
		reqLabel := requested
		if reqLabel == "" {
			reqLabel = "(unspecified)"
		}
		return nil, transport.NewUpstreamError(fmt.Sprintf("account %s cannot serve model %s", account.ID, reqLabel), 404, "model_not_found", nil, nil)
	}
	mid := model.ModelName

	if err := s.checkPreFlightQuota(account, *model, mid); err != nil {
		return nil, err
	}

	fullPrompt, overflowParts, err := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
		StartIndex:      0,
		MaxPromptChars:  s.Cfg.MaxPromptChars,
		MaxOldToolChars: s.Cfg.MaxOldToolChars,
		AllowOverflow:   true,
	})
	if err != nil {
		return nil, err
	}

	imageParts := prompt.ExtractImageParts(messages)
	fullAttachmentParts := make([]prompt.AttachmentPart, 0, len(overflowParts)+len(imageParts))
	fullAttachmentParts = append(fullAttachmentParts, overflowParts...)
	fullAttachmentParts = append(fullAttachmentParts, imageParts...)
	attachmentParts := fullAttachmentParts

	hasTools := len(contract.Tools) > 0 && contract.ChoiceMode != "none"
	stream, _ := body["stream"].(bool)
	buffered := s.Cfg.RejectDowngrade || hasTools

	tempReq, _ := body["temporary"].(bool)
	effortVal := extractEffortFromBody(body)
	if (effortVal == nil || effortVal == "") && inlineThink != "" {
		effortVal = inlineThink
	}
	opts := gemini.StreamOptions{
		Model:         model,
		ThinkingLevel: gemini.ThinkingLevelFor(effortVal, mid, strconv.Itoa(s.Cfg.DefaultThinking)),
		Temporary:     tempReq || s.Cfg.Temporary,
		DisableTools:  true,
	}

	curPrompt := fullPrompt
	if s.Cfg.SessionCache && !opts.Temporary && hasher != nil {
		for split := len(messages) - 1; split >= 2; split-- {
			prefixKey := hasher.HashPrefix(account.ID, mid, split)
			if cachedMeta := s.getSessionCache(prefixKey); len(cachedMeta) > 0 {
				opts.Metadata = cachedMeta
				deltaPrompt, _, dErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
					StartIndex:      split,
					MaxPromptChars:  s.Cfg.MaxPromptChars,
					MaxOldToolChars: s.Cfg.MaxOldToolChars,
					AllowOverflow:   false,
				})
				if dErr == nil {
					curPrompt = deltaPrompt
					attachmentParts = prompt.ExtractImageParts(messages[split:])
				}
				break
			}
		}
	}

	return &preparedGeneration{
		accts:               accts,
		account:             account,
		model:               model,
		mid:                 mid,
		requested:           requested,
		contract:            contract,
		hasTools:            hasTools,
		stream:              stream,
		buffered:            buffered,
		opts:                opts,
		fullPrompt:          fullPrompt,
		curPrompt:           curPrompt,
		overflowParts:       overflowParts,
		imageParts:          imageParts,
		fullAttachmentParts: fullAttachmentParts,
		attachmentParts:     attachmentParts,
		hasher:              hasher,
	}, nil
}
