package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type stubDoer struct {
	mu      sync.Mutex
	fn      func(req *fhttp.Request) (*fhttp.Response, error)
	closed  int
	lastReq *fhttp.Request
}

func (d *stubDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	d.mu.Lock()
	d.lastReq = req
	fn := d.fn
	d.mu.Unlock()
	return fn(req)
}

func (d *stubDoer) CloseIdleConnections() {
	d.mu.Lock()
	d.closed++
	d.mu.Unlock()
}

func TestSentinelClientOrderedHeaders(t *testing.T) {
	var captured *fhttp.Request
	doer := &stubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			captured = req
			return &fhttp.Response{
				StatusCode: 200,
				Header: fhttp.Header{
					"Content-Type": []string{"application/json"},
					"Set-Cookie":   []string{"__Secure-1PSIDTS=new_ts; Path=/; Secure; HttpOnly"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}

	sc, err := NewSentinelClient(ClientConfig{
		ProfileName: "chrome_150_psk",
		CustomDoer:  doer,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}

	res, err := sc.Do(context.Background(), RequestOptions{
		Method: "POST",
		URL:    "https://gemini.google.com/_/BardChatUi/data/batchexecute",
		Kind:   ReqKindCORS,
		Headers: map[string]string{
			"cookie":                    "SAPISID=abc",
			"x-goog-ext-525001261-jspb": "[1]",
		},
		Body: []byte("f.req=%5B%5D"),
	})
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	if res.Status != 200 || res.Body != "ok" {
		t.Fatalf("unexpected response: %+v", res)
	}
	if len(res.SetCookies) != 1 {
		t.Fatalf("expected 1 Set-Cookie, got %v", res.SetCookies)
	}
	pOrder := captured.Header[fhttp.PHeaderOrderKey]
	if len(pOrder) != 4 || pOrder[0] != ":method" || pOrder[1] != ":authority" {
		t.Fatalf("unexpected pseudo-header order: %v", pOrder)
	}
	hOrder := captured.Header[fhttp.HeaderOrderKey]
	if len(hOrder) == 0 || hOrder[0] != "sec-ch-ua" {
		t.Fatalf("expected sec-ch-ua first in header order, got %v", hOrder)
	}
}

func TestSessionLeaseQueueAndDeferredCookie(t *testing.T) {
	sc, _ := NewSentinelClient(ClientConfig{
		CustomDoer: &stubDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader("ok")),
				}, nil
			},
		},
	})
	sess := NewSession("acct1", sc, 1, "SAPISID=initial", 2)

	var seenCookies []string
	var mu sync.Mutex
	releaseFirst := make(chan struct{})
	firstEntered := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := sess.Lease(context.Background(), func(cookieHeader string) error {
			mu.Lock()
			seenCookies = append(seenCookies, cookieHeader)
			mu.Unlock()
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("first lease error: %v", err)
		}
	}()

	<-firstEntered
	// Queue a rotated cookie while the first request is in flight.
	sess.QueueCookieUpdate("SAPISID=rotated")

	go func() {
		defer wg.Done()
		err := sess.Lease(context.Background(), func(cookieHeader string) error {
			mu.Lock()
			seenCookies = append(seenCookies, cookieHeader)
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Errorf("second lease error: %v", err)
		}
	}()

	// Wait briefly so second request is queued, then check queue depth enforcement.
	time.Sleep(30 * time.Millisecond)
	close(releaseFirst)
	wg.Wait()

	if len(seenCookies) != 2 || seenCookies[0] != "SAPISID=initial" || seenCookies[1] != "SAPISID=rotated" {
		t.Fatalf("expected [SAPISID=initial, SAPISID=rotated], got %v", seenCookies)
	}
}

func TestSessionQueueOverflowAndPoolReset(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{
			CustomDoer: &stubDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader("ok")),
					}, nil
				},
			},
		},
		MaxSessions: 2,
		MaxQueue:    1,
		WaitTimeout: 80 * time.Millisecond,
	})

	sess, err := pool.Acquire(context.Background(), "acct", "SAPISID=1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	var doneWg sync.WaitGroup
	doneWg.Add(2)

	// Holder #1 (active)
	go func() {
		defer doneWg.Done()
		_ = sess.Lease(context.Background(), func(_ string) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	// Holder #2 (fills the 1-slot wait queue)
	var queuedFlag atomic.Bool
	go func() {
		defer doneWg.Done()
		queuedFlag.Store(true)
		_ = sess.Lease(context.Background(), func(_ string) error {
			<-hold
			return nil
		})
	}()
	for !queuedFlag.Load() || sess.Queued() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Holder #3 should be rejected with 429 rate_limit_error because MaxQueue == 1
	err3 := sess.Lease(context.Background(), func(_ string) error { return nil })
	var ue *UpstreamError
	if !errors.As(err3, &ue) || ue.StatusCode != 429 {
		t.Fatalf("expected 429 queue overflow error, got %v", err3)
	}

	close(hold)
	doneWg.Wait()

	pool.Reset()
	if len(pool.Status()) != 0 {
		t.Fatalf("expected 0 sessions after Reset, got %v", pool.Status())
	}
}

func TestSessionLeaseWaitTimeoutReturns429SessionBusy(t *testing.T) {
	pool := NewPool(PoolOptions{
		ClientConfig: ClientConfig{CustomDoer: &stubDoer{}},
		MaxSessions:  4,
		MaxQueue:     4,
		WaitTimeout:  40 * time.Millisecond,
	})
	sess, err := pool.Acquire(context.Background(), "acct-wait", "SID=1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Lease(context.Background(), func(_ string) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	errWait := sess.Lease(context.Background(), func(_ string) error { return nil })
	var ue *UpstreamError
	if !errors.As(errWait, &ue) || ue.StatusCode != 429 || ue.Code != "session_busy" || ue.Headers["retry-after"] != "2" {
		t.Fatalf("expected 429 session_busy with retry-after=2 after WaitTimeout, got %+v (%v)", ue, errWait)
	}

	close(hold)
	wg.Wait()

	// Subsequent lease after holder releases must succeed cleanly
	if errAfter := sess.Lease(context.Background(), func(_ string) error { return nil }); errAfter != nil {
		t.Fatalf("expected lease after release to succeed, got %v", errAfter)
	}
}

func TestNavigateVsCORSHeaderOrdering(t *testing.T) {
	var captured []*fhttp.Request
	doer := &stubDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			captured = append(captured, req)
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}
	sc, err := NewSentinelClient(ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	_, _ = sc.Do(context.Background(), RequestOptions{
		Method:  "GET",
		URL:     "https://gemini.google.com/app",
		Kind:    ReqKindNavigate,
		Headers: map[string]string{"cookie": "SID=1"},
	})
	_, _ = sc.Do(context.Background(), RequestOptions{
		Method:  "POST",
		URL:     "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
		Kind:    ReqKindCORS,
		Headers: map[string]string{"content-type": "application/x-www-form-urlencoded;charset=utf-8", "cookie": "SID=1"},
	})
	if len(captured) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(captured))
	}
	navOrder := captured[0].Header[fhttp.HeaderOrderKey]
	idxUpgrade, idxUA := -1, -1
	for i, k := range navOrder {
		if k == "upgrade-insecure-requests" {
			idxUpgrade = i
		}
		if k == "user-agent" {
			idxUA = i
		}
	}
	if idxUpgrade < 0 || idxUA < 0 || idxUpgrade >= idxUA {
		t.Fatalf("expected upgrade-insecure-requests before user-agent in Navigate order: %v", navOrder)
	}
}

func TestConcurrentPoolAcquireAndLeaseRaceFree(t *testing.T) {
	pool := NewPool(PoolOptions{
		MaxSessions: 4,
		MaxQueue:    8,
	})
	defer pool.Reset()
	ctx := context.Background()

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 50 {
				s, err := pool.Acquire(ctx, "shared-account", "SID=worker")
				if err != nil {
					t.Errorf("Acquire failed: %v", err)
					return
				}
				_ = s.Lease(ctx, func(_ string) error {
					if i%2 == 0 {
						s.QueueCookieUpdate("SID=updated")
					}
					return nil
				})
				_ = s.CookieHeader()
			}
		}(worker)
	}
	wg.Wait()
}
