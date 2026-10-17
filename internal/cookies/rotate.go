package cookies

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

const (
	rotatePageURL = "https://accounts.google.com/RotateCookiesPage?og_pid=658&rot=3&origin=https%3A%2F%2Fgemini.google.com&exp_id=0"
	rotatePostURL = "https://accounts.google.com/RotateCookies"
	// Sentinel payload for renewing __Secure-1PSIDTS / __Secure-3PSIDTS.
	// Must be sent verbatim with leading zeros ([000,...]), never json.Marshal'd.
	rotate1PSIDTSSentinel = `[000,"-0000000000000000000"]`
	min1PSIDTSInterval    = 60 * time.Second
	defaultRotateInterval = 9 * time.Minute
)

var (
	psidtsCookies = EssentialNames
	rotateInitRe  = regexp.MustCompile(`init\('([^']{4,64})'\s*,\s*([0-9.]+)\s*,[^)]*?([0-9.]+)\s*\)`)
)

// Rotator performs dual-path Google session keepalive over the account's own
// isolated SentinelClient:
//  1. Sentinel POST /RotateCookies ([000,"-0000000000000000000"]) to renew
//     short-lived __Secure-1PSIDTS / __Secure-3PSIDTS (~20-30 min TTL).
//  2. Iframe GET /RotateCookiesPage?og_pid=658 -> POST /RotateCookies ([658,"<id>"])
//     to renew SIDCC / __Secure-1PSIDCC / __Secure-3PSIDCC.
type Rotator struct {
	mu           sync.Mutex
	lastPSIDTSAt map[string]time.Time
}

// NewRotator creates a new cookie Rotator with per-account rate-limit guards.
func NewRotator() *Rotator {
	return &Rotator{
		lastPSIDTSAt: make(map[string]time.Time),
	}
}

// RotateResult reports the outcome of a dual-path cookie rotation.
type RotateResult struct {
	Status       int
	Rotated      bool
	CookieHeader string
	NextInterval time.Duration
}

// RotateAccount executes both the 1PSIDTS sentinel rotation and the 658 iframe
// SIDCC rotation using the account's dedicated SentinelClient.
func (r *Rotator) RotateAccount(ctx context.Context, accountID string, currentCookie string, sc *transport.SentinelClient) (*RotateResult, error) {
	cookie := currentCookie
	rotatedAny := false
	lastStatus := 0
	nextInterval := defaultRotateInterval

	// Primary path: POST /RotateCookies with [000,"-0000000000000000000"] and full cookie jar.
	// Renews both __Secure-1PSIDTS / __Secure-3PSIDTS and SIDCC / __Secure-*PSIDCC in a single call.
	// Calling /RotateCookies twice in the same minute triggers HTTP 429 on the second call,
	// so the 658 iframe flow is used only as a fallback if the primary call does not succeed.
	if r.allow1PSIDTS(accountID) {
		subset := Subset(cookie, psidtsCookies)
		if subset == "" {
			subset = cookie
		}
		res, err := sc.Do(ctx, transport.RequestOptions{
			Method: "POST",
			URL:    rotatePostURL,
			Kind:   transport.ReqKindCORS,
			Headers: map[string]string{
				"content-type":   "application/json",
				"origin":         "https://accounts.google.com",
				"referer":        "https://accounts.google.com/",
				"sec-fetch-site": "same-origin",
				"cookie":         subset,
			},
			Body:    []byte(rotate1PSIDTSSentinel),
			Timeout: 20 * time.Second,
		})
		if err == nil {
			lastStatus = res.Status
			r.note1PSIDTS(accountID, res.Status)
			if merged, changed := AbsorbSetCookie(cookie, res.SetCookies); changed {
				cookie = merged
				rotatedAny = true
			}
			if res.Status == 200 && rotatedAny {
				return &RotateResult{
					Status:       res.Status,
					Rotated:      rotatedAny,
					CookieHeader: cookie,
					NextInterval: nextInterval,
				}, nil
			}
		}
	}

	// Fallback path: Browser iframe RotateCookiesPage (og_pid=658) -> SIDCC / *PSIDCC rotation.
	var pageErr error
	pageRes, pageErr := sc.Do(ctx, transport.RequestOptions{
		Method: "GET",
		URL:    rotatePageURL,
		Kind:   transport.ReqKindIframe,
		Headers: map[string]string{
			"cookie": cookie,
		},
		Timeout: 20 * time.Second,
	})
	if pageErr == nil {
		if lastStatus == 0 {
			lastStatus = pageRes.Status
		}
		if merged, changed := AbsorbSetCookie(cookie, pageRes.SetCookies); changed {
			cookie = merged
			rotatedAny = true
		}
		if pageRes.Status == 200 {
			if m := rotateInitRe.FindStringSubmatch(pageRes.Body); len(m) >= 4 {
				rotID := m[1]
				if sec, err := strconv.ParseFloat(m[3], 64); err == nil && sec >= 60 && sec <= 3600 {
					nextInterval = time.Duration(sec) * time.Second
				}
				body := fmt.Sprintf(`[658,%q]`, rotID)
				postRes, postErr := sc.Do(ctx, transport.RequestOptions{
					Method: "POST",
					URL:    rotatePostURL,
					Kind:   transport.ReqKindCORS,
					Headers: map[string]string{
						"content-type":   "application/json",
						"origin":         "https://accounts.google.com",
						"referer":        rotatePageURL,
						"sec-fetch-site": "same-origin",
						"sec-fetch-mode": "same-origin",
						"cookie":         cookie,
					},
					Body:    []byte(body),
					Timeout: 20 * time.Second,
				})
				if postErr == nil {
					lastStatus = postRes.Status
					if merged, changed := AbsorbSetCookie(cookie, postRes.SetCookies); changed {
						cookie = merged
						rotatedAny = true
					}
				}
			}
		}
	}
	if lastStatus == 0 && pageErr != nil {
		return nil, pageErr
	}
	return &RotateResult{
		Status:       lastStatus,
		Rotated:      rotatedAny,
		CookieHeader: cookie,
		NextInterval: nextInterval,
	}, nil
}

func (r *Rotator) allow1PSIDTS(accountID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.lastPSIDTSAt[accountID]
	if !ok {
		return true
	}
	return time.Since(last) >= min1PSIDTSInterval
}

func (r *Rotator) note1PSIDTS(accountID string, status int) {
	if status == 200 || status == 401 || status == 403 || status == 429 {
		r.mu.Lock()
		r.lastPSIDTSAt[accountID] = time.Now()
		r.mu.Unlock()
	}
}
