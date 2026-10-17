package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

const (
	Origin        = "https://gemini.google.com"
	DefaultBL     = "boq_assistant-bard-web-server_20260415.09_p0"
	modelsTTL     = 10 * time.Minute
	xsrfTTL       = 20 * time.Minute
	defaultTenant = "bard-storage"
	defaultPushID = "feeds/mcudyrk2a4khkz"

	quotaPayloadFlash    = "[[[1,11],[2,11],[6,11]]]"
	quotaPayloadAdvanced = "[[[1,4],[6,6],[1,15]]]"
)

var (
	quotaActionLabels = map[int]string{
		4:  "Gemini Pro",
		11: "Gemini Flash",
		15: "Gemini Flash Thinking",
	}

	snlm0eRe     = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	cfb2hRe      = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	fdrfjeRe     = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
	pushIDRe     = regexp.MustCompile(`"(?:qKIAYe|q2k)":"(feeds/[a-z0-9]+)"`)
	pctxRe       = regexp.MustCompile(`"Ylro7b":"([^"]{4,400})"`)
	emailRe      = regexp.MustCompile(`"oPEP7c":"([^"]+)"`)
	emailAltRe   = regexp.MustCompile(`"(?:email|userEmail)":"([^"]+)"`)
	tierRe       = regexp.MustCompile(`"uJ6Dbe":"([^"]+)"`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// AvailableModelsResult holds the live model catalog, capacity, and tier flags for an account.
type AvailableModelsResult struct {
	Status        int               `json:"status"`
	Headers       map[string]string `json:"-"`
	Models        []ModelInfo       `json:"models"`
	TierFlags     []int             `json:"tierFlags"`
	CapFlags      []int             `json:"capFlags"`
	Capacity      int               `json:"capacity"`
	CapacityField int               `json:"capacityField"`
}

// BatchEntry is a single parsed rpcid result from batchexecute.
type BatchEntry struct {
	RPCID   string
	Payload any
}

// AccountClient is an isolated, per-account Gemini Web client backed by a browserless
// Chrome uTLS/HTTP2 session and persistent dual-path cookie rotation.
type AccountClient struct {
	ID       string
	FilePath string
	Pool     *transport.Pool
	Rotator  *cookies.Rotator

	mu             sync.RWMutex
	bootMu         sync.Mutex
	cookieHeader   string
	email          string
	userTier       string
	at             string
	bl             string
	fSid           string
	pushID         string
	pctx           string
	bootstrappedAt time.Time
	fileMtimeMs    int64
	modelsCache    *AvailableModelsResult
	modelsCachedAt time.Time
	reqCounter     atomic.Int64
}

// NewAccountClient creates an AccountClient for a specific Google account.
func NewAccountClient(id, cookieHeader, filePath string, pool *transport.Pool, rotator *cookies.Rotator) *AccountClient {
	ac := &AccountClient{
		ID:           id,
		FilePath:     filePath,
		Pool:         pool,
		Rotator:      rotator,
		cookieHeader: cookieHeader,
		pushID:       defaultPushID,
	}
	ac.reqCounter.Store(100000 + time.Now().UnixNano()%800000)
	return ac
}

// LoadAccountFromFile loads an AccountClient from an account JSON file.
func LoadAccountFromFile(path string, pool *transport.Pool, rotator *cookies.Rotator) (*AccountClient, error) {
	rec, mtime, err := cookies.LoadAccountFile(path)
	if err != nil {
		return nil, err
	}
	id := rec.Name
	if id == "" {
		id = "default"
	}
	ac := NewAccountClient(id, rec.Cookies, path, pool, rotator)
	ac.email = rec.Email
	ac.at = rec.AT
	ac.bl = rec.BL
	ac.fSid = rec.FSid
	ac.fileMtimeMs = mtime
	return ac, nil
}

// ReloadFromDiskIfNewer checks if the account JSON file was updated externally
// (e.g. by web-login or scp) and reloads its cookies and tokens in place.
func (ac *AccountClient) ReloadFromDiskIfNewer() bool {
	ac.mu.RLock()
	path := ac.FilePath
	lastMtime := ac.fileMtimeMs
	ac.mu.RUnlock()
	if path == "" {
		return false
	}
	rec, mtime, err := cookies.LoadAccountFile(path)
	if err != nil || mtime <= lastMtime || rec.Cookies == "" {
		return false
	}
	ac.mu.Lock()
	changed := rec.Cookies != ac.cookieHeader
	ac.cookieHeader = rec.Cookies
	if rec.Email != "" {
		ac.email = rec.Email
	}
	if rec.AT != "" {
		ac.at = rec.AT
	}
	if rec.BL != "" {
		ac.bl = rec.BL
	}
	if rec.FSid != "" {
		ac.fSid = rec.FSid
	}
	ac.fileMtimeMs = mtime
	if changed {
		ac.bootstrappedAt = time.Time{}
		ac.modelsCache = nil
	}
	ac.mu.Unlock()

	if changed && ac.Pool != nil {
		if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
			s.QueueCookieUpdate(rec.Cookies)
		}
	}
	return changed
}

// Email returns the account's Google email address.
func (ac *AccountClient) Email() string {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.email
}

// SetTokens sets pre-existing SNlM0e (at), cfb2h (bl), and FdrFJe (fSid) tokens.
func (ac *AccountClient) SetTokens(at, bl, fSid string) {
	ac.mu.Lock()
	if at != "" {
		ac.at = at
		ac.bootstrappedAt = time.Now()
	}
	if bl != "" {
		ac.bl = bl
	}
	if fSid != "" {
		ac.fSid = fSid
	}
	ac.mu.Unlock()
}

// CookieHeader returns the current Cookie header string.
func (ac *AccountClient) CookieHeader() string {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.cookieHeader
}

// InvalidateAuth clears the cached SNlM0e token so the next call re-bootstraps.
func (ac *AccountClient) InvalidateAuth() {
	ac.mu.Lock()
	ac.bootstrappedAt = time.Now().Add(-2 * xsrfTTL)
	ac.mu.Unlock()
}

func (ac *AccountClient) sessionKey() string {
	if ac.ID != "" {
		return ac.ID
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	if ac.email != "" {
		return ac.email
	}
	return "default"
}

func (ac *AccountClient) absorbAndPersistCookies(setCookies []string) bool {
	if len(setCookies) == 0 {
		return false
	}
	ac.mu.Lock()
	merged, changed := cookies.AbsorbSetCookie(ac.cookieHeader, setCookies)
	if !changed {
		ac.mu.Unlock()
		return false
	}
	ac.cookieHeader = merged
	ac.mu.Unlock()

	if ac.Pool != nil {
		if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
			s.QueueCookieUpdate(merged)
		}
	}
	ac.persistState()
	return true
}

func (ac *AccountClient) persistState() {
	ac.mu.RLock()
	path := ac.FilePath
	if path == "" {
		ac.mu.RUnlock()
		return
	}
	rec := cookies.CredentialFile{
		Kind:       "gemini-web",
		Name:       ac.ID,
		Email:      ac.email,
		Cookies:    ac.cookieHeader,
		AT:         ac.at,
		BL:         ac.bl,
		FSid:       ac.fSid,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
	}
	ac.mu.RUnlock()
	if mtime, err := cookies.SaveCredentialFile(path, rec); err == nil {
		ac.mu.Lock()
		ac.fileMtimeMs = mtime
		ac.mu.Unlock()
	}
}

func (ac *AccountClient) acquireSession(ctx context.Context) (*transport.Session, error) {
	ac.ReloadFromDiskIfNewer()
	return ac.Pool.Acquire(ctx, ac.sessionKey(), ac.CookieHeader())
}

// Bootstrap loads https://gemini.google.com/app over the account's transport session
// and extracts SNlM0e (at), cfb2h (bl), FdrFJe (f.sid), pushId, pctx, and email.
func (ac *AccountClient) Bootstrap(ctx context.Context) error {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return err
	}
	ac.bootMu.Lock()
	defer ac.bootMu.Unlock()
	return ac.bootstrapOnClient(ctx, sess.Client)
}

func (ac *AccountClient) bootstrapOnClient(ctx context.Context, sc *transport.SentinelClient) error {
	doFetchApp := func() (*transport.Response, error) {
		res, err := sc.Do(ctx, transport.RequestOptions{
			Method:  "GET",
			URL:     Origin + "/app",
			Kind:    transport.ReqKindNavigate,
			Headers: map[string]string{"cookie": ac.CookieHeader()},
			Timeout: 30 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		ac.absorbAndPersistCookies(res.SetCookies)
		return res, nil
	}

	res, err := doFetchApp()
	if err != nil {
		return err
	}

	at := ""
	if res.Status == 200 {
		at = matchFirst(snlm0eRe, res.Body)
	}
	// Self-heal expired __Secure-1PSIDTS / SIDCC before declaring session dead:
	// if /app failed or returned no SNlM0e token, run RotateCookies once and retry.
	if res.Status != 200 || at == "" {
		if rotRes, rotErr := ac.rotateOnClient(ctx, sc); rotErr == nil && rotRes != nil && rotRes.Rotated {
			if retryRes, retryErr := doFetchApp(); retryErr == nil {
				res = retryRes
				if res.Status == 200 {
					at = matchFirst(snlm0eRe, res.Body)
				}
			}
		}
	}

	if res.Status != 200 {
		code := "upstream_http_error"
		status := res.Status
		if status == 401 || status == 403 || status == 302 {
			code = "upstream_auth_error"
			if status == 302 {
				status = 401
			}
		}
		return transport.NewUpstreamError(fmt.Sprintf("bootstrap HTTP %d", res.Status), status, code, res.Headers, nil)
	}

	bl := matchFirst(cfb2hRe, res.Body)
	fSid := matchFirst(fdrfjeRe, res.Body)
	pushID := matchFirst(pushIDRe, res.Body)
	pctx := matchFirst(pctxRe, res.Body)
	email := matchFirst(emailRe, res.Body)
	if email == "" {
		email = matchFirst(emailAltRe, res.Body)
	}
	tier := matchFirst(tierRe, res.Body)

	if at == "" {
		return transport.NewUpstreamError(
			"could not extract SNlM0e (at token); session may be expired",
			401,
			"upstream_auth_error",
			res.Headers,
			nil,
		)
	}
	ac.mu.Lock()
	ac.at = at
	if bl != "" {
		ac.bl = bl
	} else if ac.bl == "" {
		ac.bl = DefaultBL
	}
	if fSid != "" {
		ac.fSid = fSid
	}
	if pushID != "" {
		ac.pushID = pushID
	}
	if pctx != "" {
		ac.pctx = pctx
	}
	if email != "" {
		ac.email = email
	}
	if tier != "" {
		ac.userTier = tier
	}
	ac.bootstrappedAt = time.Now()
	ac.mu.Unlock()

	ac.persistState()
	return nil
}

func (ac *AccountClient) ensureBootstrapped(ctx context.Context, sc *transport.SentinelClient) error {
	ac.mu.RLock()
	at := ac.at
	age := time.Since(ac.bootstrappedAt)
	ac.mu.RUnlock()
	if at != "" && (ac.bootstrappedAt.IsZero() || age < xsrfTTL) {
		return nil
	}
	ac.bootMu.Lock()
	defer ac.bootMu.Unlock()
	ac.mu.RLock()
	at = ac.at
	age = time.Since(ac.bootstrappedAt)
	ac.mu.RUnlock()
	if at != "" && (ac.bootstrappedAt.IsZero() || age < xsrfTTL) {
		return nil
	}
	return ac.bootstrapOnClient(ctx, sc)
}

// RotateCookies runs the dual-path cookie rotation for this account and persists
// any rotated cookies to disk and to the active session without blocking active generation leases.
func (ac *AccountClient) RotateCookies(ctx context.Context) (*cookies.RotateResult, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, err
	}
	return ac.rotateOnClient(ctx, sess.Client)
}

func (ac *AccountClient) rotateOnClient(ctx context.Context, sc *transport.SentinelClient) (*cookies.RotateResult, error) {
	if ac.Rotator == nil {
		ac.Rotator = cookies.NewRotator()
	}
	res, err := ac.Rotator.RotateAccount(ctx, ac.sessionKey(), ac.CookieHeader(), sc)
	if err != nil {
		return nil, err
	}
	if res.Rotated && res.CookieHeader != "" {
		ac.mu.Lock()
		ac.cookieHeader = res.CookieHeader
		ac.mu.Unlock()
		if ac.Pool != nil {
			if s := ac.Pool.Existing(ac.sessionKey()); s != nil {
				s.QueueCookieUpdate(res.CookieHeader)
			}
		}
		ac.persistState()
	}
	return res, nil
}

// BatchExec calls /_/BardChatUi/data/batchexecute for the given RPC calls.
func (ac *AccountClient) BatchExec(ctx context.Context, rpcID, payloadJSON string) (*transport.Response, []BatchEntry, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, nil, err
	}
	if bErr := ac.ensureBootstrapped(ctx, sess.Client); bErr != nil {
		return nil, nil, bErr
	}
	res, err := ac.doBatchExec(ctx, sess.Client, rpcID, payloadJSON)
	if err != nil {
		return nil, nil, err
	}
	return res, ParseBatch(res.Body), nil
}

func (ac *AccountClient) doBatchExec(ctx context.Context, sc *transport.SentinelClient, rpcID, payloadJSON string) (*transport.Response, error) {
	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	cookie := ac.cookieHeader
	ac.mu.RUnlock()

	reqID := ac.nextReqID()
	q := url.Values{}
	q.Set("rpcids", rpcID)
	q.Set("source-path", "/app")
	q.Set("bl", bl)
	q.Set("f.sid", fSid)
	q.Set("hl", "en")
	q.Set("_reqid", strconv.FormatInt(reqID, 10))
	q.Set("rt", "c")

	freq, _ := json.Marshal([][][]any{{{rpcID, payloadJSON, nil, "generic"}}})
	form := url.Values{}
	form.Set("at", at)
	form.Set("f.req", string(freq))

	modelHdr, _ := json.Marshal([]any{
		1, nil, nil, nil, nil, nil, nil, nil, []int{4, 5, 6, 8},
		nil, nil, nil, nil, nil, nil, nil, fSid,
	})

	headers := map[string]string{
		"content-type":              "application/x-www-form-urlencoded;charset=utf-8",
		"x-same-domain":             "1",
		"x-goog-ext-525001261-jspb": string(modelHdr),
		"x-goog-ext-73010989-jspb":  "[0]",
		"cookie":                    cookie,
	}
	if auth := cookies.SAPISIDHash(cookie, Origin); auth != "" {
		headers["authorization"] = auth
	}

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method:  "POST",
		URL:     Origin + "/_/BardChatUi/data/batchexecute?" + q.Encode(),
		Kind:    transport.ReqKindCORS,
		Headers: headers,
		Body:    []byte(form.Encode()),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	ac.absorbAndPersistCookies(res.SetCookies)
	return res, nil
}

// ParseBatch parses a batchexecute response body into BatchEntry items.
func ParseBatch(text string) []BatchEntry {
	body := strings.TrimLeft(text, ")]}' \t\r\n")
	var out []BatchEntry
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || isDigits(t) {
			continue
		}
		var arr []any
		if err := json.Unmarshal([]byte(t), &arr); err != nil {
			continue
		}
		for _, entryAny := range arr {
			entry, ok := entryAny.([]any)
			if !ok || len(entry) < 3 {
				continue
			}
			tag, _ := entry[0].(string)
			if tag != "wrb.fr" {
				continue
			}
			rpcID, _ := entry[1].(string)
			payload := entry[2]
			if s, ok := payload.(string); ok {
				var parsed any
				if err := json.Unmarshal([]byte(s), &parsed); err == nil {
					payload = parsed
				}
			}
			out = append(out, BatchEntry{RPCID: rpcID, Payload: payload})
		}
	}
	return out
}

// AvailableModels queries the live otAQ7b RPC (`GET_USER_STATUS`), re-bootstrapping
// on code 1016 if needed, and computes the account's live model catalog and capacity.
func (ac *AccountClient) AvailableModels(ctx context.Context, force bool) (*AvailableModelsResult, error) {
	if !force {
		ac.mu.RLock()
		cached := ac.modelsCache
		cachedAt := ac.modelsCachedAt
		ac.mu.RUnlock()
		if cached != nil && len(cached.Models) > 0 && time.Since(cachedAt) < modelsTTL {
			return cached, nil
		}
	}

	res, entries, err := ac.BatchExec(ctx, "otAQ7b", "[]")
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		code := "model_discovery_error"
		if res.Status == 401 {
			code = "upstream_auth_error"
		}
		return nil, transport.NewUpstreamError(fmt.Sprintf("model discovery HTTP %d", res.Status), res.Status, code, res.Headers, nil)
	}

	p := findBatchPayload(entries, "otAQ7b")
	if isStatus1016(p) {
		ac.InvalidateAuth()
		if bErr := ac.Bootstrap(ctx); bErr != nil {
			return nil, bErr
		}
		res, entries, err = ac.BatchExec(ctx, "otAQ7b", "[]")
		if err != nil {
			return nil, err
		}
		p = findBatchPayload(entries, "otAQ7b")
	}

	if isStatus1016(p) {
		return nil, transport.NewUpstreamError("session is unauthenticated or cookies have expired (code 1016)", 401, "upstream_auth_error", res.Headers, nil)
	}

	pArr, ok := p.([]any)
	if !ok || len(pArr) <= 15 {
		return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
	}
	rawModels, ok := pArr[15].([]any)
	if !ok || len(rawModels) == 0 {
		return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
	}
	if len(pArr) > 16 && pArr[16] != nil {
		if _, ok := pArr[16].([]any); !ok {
			return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
		}
	}
	if len(pArr) > 17 && pArr[17] != nil {
		if _, ok := pArr[17].([]any); !ok {
			return nil, transport.NewUpstreamError("malformed upstream model registry", 502, "model_discovery_error", res.Headers, nil)
		}
	}

	var tierFlags, capFlags []int
	if len(pArr) > 16 {
		tierFlags = toIntSlice(pArr[16])
	}
	if len(pArr) > 17 {
		capFlags = toIntSlice(pArr[17])
	}
	capacity, capacityField := ComputeCapacity(tierFlags, capFlags)

	models := make([]ModelInfo, 0, len(rawModels))
	for _, mdAny := range rawModels {
		md, ok := mdAny.([]any)
		if !ok || len(md) == 0 {
			return nil, transport.NewUpstreamError("malformed upstream model entry", 502, "model_discovery_error", res.Headers, nil)
		}
		modelID, ok := md[0].(string)
		if !ok || strings.TrimSpace(modelID) == "" {
			return nil, transport.NewUpstreamError("malformed upstream model entry", 502, "model_discovery_error", res.Headers, nil)
		}
		category := stringAt(md, 1)
		if category == "" {
			category = stringAt(md, 10)
		}
		displayName := stringAt(md, 11)
		if displayName == "" {
			displayName = stringAt(md, 19)
		}
		if displayName == "" {
			displayName = stringAt(md, 1)
		}
		description := stringAt(md, 12)
		if description == "" {
			description = stringAt(md, 2)
		}
		modelNumber := 1
		if n, ok := intAt(md, 17); ok {
			modelNumber = n
		} else if n, ok := intAt(md, 9); ok {
			modelNumber = n
		}

		catSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(category)), "-")
		modelName := "gemini-" + modelID
		if catSlug != "" {
			modelName = "gemini-" + catSlug
		}
		dispSlug := whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(displayName)), "-")
		var aliases []string
		if dispSlug != "" && "gemini-"+dispSlug != modelName {
			aliases = append(aliases, "gemini-"+dispSlug)
		}
		disp := displayName
		if disp == "" {
			disp = category
		}
		if disp == "" {
			disp = modelID
		}

		models = append(models, ModelInfo{
			ModelID:       modelID,
			ModelName:     modelName,
			DisplayName:   disp,
			Aliases:       aliases,
			Description:   description,
			Capacity:      capacity,
			CapacityField: capacityField,
			ModelNumber:   modelNumber,
		})
	}

	result := &AvailableModelsResult{
		Status:        res.Status,
		Headers:       res.Headers,
		Models:        models,
		TierFlags:     tierFlags,
		CapFlags:      capFlags,
		Capacity:      capacity,
		CapacityField: capacityField,
	}

	ac.mu.Lock()
	ac.modelsCache = result
	ac.modelsCachedAt = time.Now()
	ac.mu.Unlock()

	return result, nil
}

// GetQuota queries the qpEbW RPC (`CHECK_GEMINI_QUOTA`) for both Flash and Pro/Advanced buckets.
func (ac *AccountClient) GetQuota(ctx context.Context) []QuotaBucket {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil
	}
	if bErr := ac.ensureBootstrapped(ctx, sess.Client); bErr != nil {
		return nil
	}

	payloads := []string{quotaPayloadFlash, quotaPayloadAdvanced}
	batchResults := make([][]BatchEntry, len(payloads))
	var wg sync.WaitGroup
	for i, payload := range payloads {
		wg.Add(1)
		go func(idx int, pl string) {
			defer wg.Done()
			res, rErr := ac.doBatchExec(ctx, sess.Client, "qpEbW", pl)
			if rErr == nil && res != nil {
				batchResults[idx] = ParseBatch(res.Body)
			}
		}(i, payload)
	}
	wg.Wait()

	var out []QuotaBucket
	for _, entries := range batchResults {
		if len(entries) == 0 {
			continue
		}
		p := findBatchPayload(entries, "qpEbW")
		pArr, ok := p.([]any)
		if !ok || len(pArr) == 0 {
			continue
		}
		items, ok := pArr[0].([]any)
		if !ok {
			continue
		}
		for _, itemAny := range items {
			item, ok := itemAny.([]any)
			if !ok || len(item) < 6 {
				continue
			}
			idList, _ := item[0].([]any)
			if len(idList) < 2 {
				continue
			}
			actionID, ok := intAt(idList, 1)
			if !ok {
				continue
			}
			label, exists := quotaActionLabels[actionID]
			if !exists {
				continue
			}
			var idParts []string
			for _, v := range idList {
				if f, ok := v.(float64); ok {
					idParts = append(idParts, strconv.Itoa(int(f)))
				}
			}
			var usagePct *float64
			if f, ok := item[2].(float64); ok {
				val := f
				usagePct = &val
			}
			var resetAt string
			if rArr, ok := item[3].([]any); ok && len(rArr) > 0 {
				if ts, ok := rArr[0].(float64); ok && ts > 0 {
					resetAt = time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
				}
			}
			total, _ := intAt(item, 4)
			remaining, _ := intAt(item, 5)
			out = append(out, QuotaBucket{
				ID:              strings.Join(idParts, "-"),
				Label:           label,
				UsagePercentage: usagePct,
				Total:           total,
				Remaining:       remaining,
				ResetAt:         resetAt,
				Unlimited:       total == 0 && remaining == 0,
			})
		}
	}
	return out
}

func (ac *AccountClient) nextReqID() int64 {
	return ac.reqCounter.Add(100000)
}

func findBatchPayload(entries []BatchEntry, rpcID string) any {
	for _, e := range entries {
		if e.RPCID == rpcID {
			return e.Payload
		}
	}
	return nil
}

func isStatus1016(p any) bool {
	arr, ok := p.([]any)
	if !ok || len(arr) <= 14 {
		return false
	}
	if f, ok := arr[14].(float64); ok && int(f) == 1016 {
		return true
	}
	return false
}

func toIntSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		if f, ok := item.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func stringAt(arr []any, idx int) string {
	if idx >= 0 && idx < len(arr) {
		if s, ok := arr[idx].(string); ok {
			return s
		}
	}
	return ""
}

func intAt(arr []any, idx int) (int, bool) {
	if idx >= 0 && idx < len(arr) {
		if f, ok := arr[idx].(float64); ok && float64(int(f)) == f {
			return int(f), true
		}
	}
	return 0, false
}

func matchFirst(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
