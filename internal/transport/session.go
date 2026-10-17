package transport

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SessionStatus describes the live state of a per-account transport session.
type SessionStatus struct {
	Key     string `json:"key"`
	Ready   bool   `json:"ready"`
	Busy    bool   `json:"busy"`
	Queued  int    `json:"queued"`
	Active  int    `json:"active"`
	Stale   bool   `json:"stale"`
	IdleSec int64  `json:"idleSec"`
}

// Session owns an isolated per-account SentinelClient and serializes generations
// on that account with a bounded wait queue, mirroring BrowserSession without Chrome.
type Session struct {
	Name          string
	Client        *SentinelClient
	Generation    uint64
	MaxQueueDepth int
	WaitTimeout   time.Duration
	mu            sync.Mutex
	ready         bool
	active        int
	reservedUntil time.Time
	lastUsed      time.Time
	cookieHeader  string
	pendingCookie string
	execMu        sync.Mutex
}

// NewSession constructs a new isolated account session.
func NewSession(name string, client *SentinelClient, gen uint64, cookieHeader string, maxQueue int, waitTimeout ...time.Duration) *Session {
	if maxQueue <= 0 {
		maxQueue = 4
	}
	var wt time.Duration
	if len(waitTimeout) > 0 && waitTimeout[0] > 0 {
		wt = waitTimeout[0]
	}
	return &Session{
		Name:          name,
		Client:        client,
		Generation:    gen,
		MaxQueueDepth: maxQueue,
		WaitTimeout:   wt,
		ready:         true,
		lastUsed:      time.Now(),
		cookieHeader:  cookieHeader,
	}
}

// Busy reports whether the session is currently executing or queueing work.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active > 0
}

// Active returns the number of in-flight + queued requests on this session.
func (s *Session) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Queued returns the number of requests waiting behind the currently executing request.
func (s *Session) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active <= 1 {
		return 0
	}
	return s.active - 1
}

// QueueCookieUpdate schedules a fresh cookie header to be applied before the next request.
func (s *Session) QueueCookieUpdate(cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cookie != "" && cookie != s.cookieHeader {
		s.pendingCookie = cookie
	}
}

// CookieHeader returns the session's active cookie header.
func (s *Session) CookieHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingCookie != "" {
		return s.pendingCookie
	}
	return s.cookieHeader
}

// Lease runs fn exclusively under this account session's lease, enforcing
// MaxQueueDepth and context cancellation while queued.
func (s *Session) Lease(ctx context.Context, fn func(cookieHeader string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	waiting := 0
	if s.active > 0 {
		waiting = s.active - 1
	}
	if waiting >= s.MaxQueueDepth {
		s.mu.Unlock()
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: %d request(s) already queued", s.Name, waiting),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	}
	s.active++
	s.reservedUntil = time.Time{}
	s.lastUsed = time.Now()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.active--
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()

	// Acquire the execution mutex while respecting caller cancellation while waiting.
	acquired := make(chan struct{})
	go func() {
		s.execMu.Lock()
		close(acquired)
	}()

	var waitCh <-chan time.Time
	var timer *time.Timer
	if s.WaitTimeout > 0 {
		timer = time.NewTimer(s.WaitTimeout)
		defer timer.Stop()
		waitCh = timer.C
	}

	select {
	case <-acquired:
		defer s.execMu.Unlock()
	case <-waitCh:
		go func() {
			<-acquired
			s.execMu.Unlock()
		}()
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: timed out after %s waiting for active generation", s.Name, s.WaitTimeout),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	case <-ctx.Done():
		// Clean up the lock when the goroutine eventually acquires it.
		go func() {
			<-acquired
			s.execMu.Unlock()
		}()
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, ctx.Err())
	}

	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	s.lastUsed = time.Now()
	if s.pendingCookie != "" && s.pendingCookie != s.cookieHeader {
		s.cookieHeader = s.pendingCookie
		s.pendingCookie = ""
	}
	curCookie := s.cookieHeader
	s.mu.Unlock()

	return fn(curCookie)
}

// RunStream executes an HTTP/2 multiplexed stream on this session without holding
// the single-tab execMu lock, allowing up to MaxQueueDepth concurrent streams per account.
func (s *Session) RunStream(ctx context.Context, fn func(cookieHeader string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return NewUpstreamError("request cancelled", 499, "client_cancelled", nil, err)
	}

	s.mu.Lock()
	limit := s.MaxQueueDepth
	if limit <= 0 {
		limit = 4
	}
	if s.active >= limit {
		s.mu.Unlock()
		return NewUpstreamError(
			fmt.Sprintf("session %s is busy: %d concurrent stream(s) already active", s.Name, s.active),
			429,
			"session_busy",
			map[string]string{"retry-after": "2"},
			nil,
		)
	}
	s.active++
	s.reservedUntil = time.Time{}
	s.lastUsed = time.Now()
	if s.pendingCookie != "" && s.pendingCookie != s.cookieHeader {
		s.cookieHeader = s.pendingCookie
		s.pendingCookie = ""
	}
	curCookie := s.cookieHeader
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.active--
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()

	return fn(curCookie)
}

// Close releases idle connections held by the session.
func (s *Session) Close() {
	s.mu.Lock()
	s.ready = false
	s.pendingCookie = ""
	s.mu.Unlock()
	if s.Client != nil {
		s.Client.ResetConnections()
	}
}

// Pool manages isolated per-account transport sessions with bounded capacity,
// reservation grace windows, and idle reaping.
type Pool struct {
	mu          sync.Mutex
	clientCfg   ClientConfig
	maxSessions int
	maxQueue    int
	waitTimeout time.Duration
	reserveFor  time.Duration
	generation  uint64
	sessions    map[string]*Session
}

// PoolOptions configures a Session Pool.
type PoolOptions struct {
	ClientConfig ClientConfig
	MaxSessions  int
	MaxQueue     int
	WaitTimeout  time.Duration
	ReserveFor   time.Duration
}

// NewPool creates a new per-account transport pool.
func NewPool(opts PoolOptions) *Pool {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 16
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = 4
	}
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = 5 * time.Second
	}
	if opts.ReserveFor <= 0 {
		opts.ReserveFor = 5 * time.Second
	}
	return &Pool{
		clientCfg:   opts.ClientConfig,
		maxSessions: opts.MaxSessions,
		maxQueue:    opts.MaxQueue,
		waitTimeout: opts.WaitTimeout,
		reserveFor:  opts.ReserveFor,
		sessions:    make(map[string]*Session),
	}
}

// Existing returns a ready session for key if one is currently open.
func (p *Pool) Existing(key string) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[key]
	if !ok || s.Generation != p.generation {
		return nil
	}
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready {
		return s
	}
	return nil
}

// Acquire returns or creates the isolated Session bound to key.
func (p *Pool) Acquire(ctx context.Context, key, cookieHeader string) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(p.waitTimeout)

	for {
		p.mu.Lock()
		if existing, ok := p.sessions[key]; ok {
			existing.mu.Lock()
			isReady := existing.Generation == p.generation && existing.ready
			if isReady {
				if cookieHeader != "" && cookieHeader != existing.cookieHeader {
					existing.pendingCookie = cookieHeader
				}
				existing.reservedUntil = time.Now().Add(p.reserveFor)
			}
			existing.mu.Unlock()
			if isReady {
				p.mu.Unlock()
				return existing, nil
			}
			delete(p.sessions, key)
		}

		if len(p.sessions) < p.maxSessions || p.evictLRUIdleLocked() {
			sc, err := NewSentinelClient(p.clientCfg)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			s := NewSession(key, sc, p.generation, cookieHeader, p.maxQueue, p.waitTimeout)
			s.reservedUntil = time.Now().Add(p.reserveFor)
			p.sessions[key] = s
			p.mu.Unlock()
			return s, nil
		}
		p.mu.Unlock()

		if time.Now().After(deadline) {
			return nil, NewUpstreamError(
				fmt.Sprintf("all %d transport sessions are busy", p.maxSessions),
				429,
				"rate_limit_error",
				nil,
				nil,
			)
		}
		select {
		case <-ctx.Done():
			return nil, NewUpstreamError("request cancelled", 499, "client_cancelled", nil, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *Pool) evictLRUIdleLocked() bool {
	now := time.Now()
	var bestKey string
	var bestSession *Session
	var bestLastUsed time.Time
	for k, s := range p.sessions {
		s.mu.Lock()
		idle := s.active == 0 && s.ready && !s.reservedUntil.After(now)
		lu := s.lastUsed
		s.mu.Unlock()
		if !idle {
			continue
		}
		if bestSession == nil || lu.Before(bestLastUsed) {
			bestKey = k
			bestSession = s
			bestLastUsed = lu
		}
	}
	if bestSession == nil {
		return false
	}
	delete(p.sessions, bestKey)
	go bestSession.Close()
	return true
}

// ReapIdle closes sessions that have been idle longer than maxIdle.
func (p *Pool) ReapIdle(maxIdle time.Duration) int {
	now := time.Now()
	p.mu.Lock()
	var toClose []*Session
	for k, s := range p.sessions {
		s.mu.Lock()
		canReap := s.active == 0 && s.ready && !s.reservedUntil.After(now) && now.Sub(s.lastUsed) >= maxIdle
		s.mu.Unlock()
		if canReap {
			delete(p.sessions, k)
			toClose = append(toClose, s)
		}
	}
	p.mu.Unlock()
	for _, s := range toClose {
		s.Close()
	}
	return len(toClose)
}

// Status returns a diagnostic snapshot of all open sessions in the pool.
func (p *Pool) Status() []SessionStatus {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SessionStatus, 0, len(p.sessions))
	for k, s := range p.sessions {
		s.mu.Lock()
		queued := 0
		if s.active > 1 {
			queued = s.active - 1
		}
		st := SessionStatus{
			Key:     k,
			Ready:   s.ready,
			Busy:    s.active > 0,
			Queued:  queued,
			Active:  s.active,
			Stale:   s.Generation != p.generation,
			IdleSec: int64(now.Sub(s.lastUsed).Seconds()),
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Reset invalidates the current generation and closes all sessions.
func (p *Pool) Reset() {
	p.mu.Lock()
	p.generation++
	old := p.sessions
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()
	for _, s := range old {
		s.Close()
	}
}
