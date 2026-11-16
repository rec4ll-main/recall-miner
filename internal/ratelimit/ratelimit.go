// Package ratelimit holds the miner's in-process overload controls (SECURITY_BACKLOG,
// "Rate Limiting"):
//
//   - Window: a fixed-window counter per key with a bounded key table. Used for the
//     pre-auth per-IP authentication-failure limiter and the post-Epistula per-hotkey
//     request limiter.
//   - Semaphore: a per-route concurrency cap for expensive routes.
//   - ProxyPolicy: which peers may supply a client address through X-Forwarded-For.
//
// Everything is local to one process on purpose: a miner runs one API replica.
package ratelimit

import (
	"container/list"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Outcome of recording one event in a Window.
type Outcome int

const (
	Allowed     Outcome = iota // recorded, within the limit
	OverLimit                  // recorded key is above its limit
	TableFull                  // key not tracked and no room to track it; nothing recorded
	PendingFull                // pending work fills the budget before retained failures do
)

// Window is a fixed-window counter: at most Limit events per key per Window.
type Window struct {
	Limit   int
	Window  time.Duration
	MaxKeys int // bounded table; 0 = DefaultMaxKeys

	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
	expiry  list.List
}

type entry struct {
	count    int
	failures int // retained failed authentication, excluding pending reservations
	start    time.Time
	key      string
	node     *list.Element
}

const DefaultMaxKeys = 10_000

// NewWindow returns a limiter; a Limit <= 0 disables it (Allow always succeeds).
func NewWindow(limit int, window time.Duration, maxKeys int) *Window {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Window{Limit: limit, Window: window, MaxKeys: maxKeys, entries: map[string]*entry{}, now: time.Now}
}

// Enabled reports whether the limiter enforces anything.
func (w *Window) Enabled() bool { return w != nil && w.Limit > 0 }

// Record counts one event for key. TableFull means the key is not tracked and the
// table holds MaxKeys live entries, so the event could not be counted; callers must
// decide explicitly what that means for them.
func (w *Window) Record(key string) (Outcome, time.Duration) {
	if !w.Enabled() {
		return Allowed, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out, retry, _ := w.recordLocked(key, w.now(), false)
	return out, retry
}

func (w *Window) recordLocked(key string, now time.Time, pending bool) (Outcome, time.Duration, *entry) {
	e, found := w.entries[key]
	if found && now.Sub(e.start) >= w.Window {
		e.count, e.failures, e.start = 0, 0, now
		w.expiry.MoveToBack(e.node)
	}
	if !found {
		if !w.roomLocked(now) {
			return TableFull, w.Window, nil
		}
		e = &entry{start: now, key: key}
		e.node = w.expiry.PushBack(e)
		w.entries[key] = e
	}
	if e.count >= w.Limit {
		if pending && e.failures < w.Limit {
			return PendingFull, w.Window - now.Sub(e.start), nil
		}
		return OverLimit, w.Window - now.Sub(e.start), nil
	}
	e.count++
	if !pending {
		e.failures++
	}
	return Allowed, 0, e
}

// reserve atomically charges pending authentication before entering the handler.
// A successful completion refunds only its own window, never a replacement bucket.
func (w *Window) reserve(key string) (Outcome, time.Duration, func()) {
	out, retry, settle := w.reserveAuth(key)
	return out, retry, func() { settle(true) }
}

// reserveAuth settles each reservation once: success refunds, failure retains its
// charge and records a completed authentication failure in the same generation.
func (w *Window) reserveAuth(key string) (Outcome, time.Duration, func(bool)) {
	if !w.Enabled() {
		return Allowed, 0, func(bool) {}
	}
	w.mu.Lock()
	out, retry, e := w.recordLocked(key, w.now(), true)
	var start time.Time
	if e != nil {
		start = e.start
	}
	w.mu.Unlock()
	var once sync.Once
	return out, retry, func(success bool) {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if e != nil && w.entries[key] == e && e.start.Equal(start) && e.count > 0 {
				if !success {
					e.failures++
					return
				}
				e.count--
				// The last successful completion frees both the key and its
				// expiry node, regardless of which reservation created it.
				if e.count == 0 {
					delete(w.entries, key)
					w.expiry.Remove(e.node)
				}
			}
		})
	}
}

// Allow records one event for key and reports whether it is within the limit. A key
// that cannot be tracked because the table is full is refused: bounded memory and an
// explicit refusal beat silently admitting an unbounded set of new keys.
func (w *Window) Allow(key string) (ok bool, retryAfter time.Duration) {
	out, retry := w.Record(key)
	return out == Allowed, retry
}

// Peek reports whether key is currently over the limit without recording an event.
func (w *Window) Peek(key string) (over bool, retryAfter time.Duration) {
	if !w.Enabled() {
		return false, 0
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	e, found := w.entries[key]
	if !found || now.Sub(e.start) >= w.Window {
		return false, 0
	}
	if e.count >= w.Limit {
		return true, w.Window - now.Sub(e.start)
	}
	return false, 0
}

// Tracked reports whether key currently has a live entry.
func (w *Window) Tracked(key string) bool {
	if !w.Enabled() {
		return false
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	e, found := w.entries[key]
	return found && now.Sub(e.start) < w.Window
}

// Saturated reports whether a new key could not be tracked right now.
func (w *Window) Saturated() bool {
	if !w.Enabled() {
		return false
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.roomLocked(now)
}

// roomLocked evicts expired entries when needed and reports whether a new key fits.
func (w *Window) roomLocked(now time.Time) bool {
	if len(w.entries) < w.MaxKeys {
		return true
	}
	// Ordered expiry avoids scanning the entire live table on each rejection.
	// Bound reclamation too: an expiry storm cannot monopolize the admission lock.
	for n := 0; n < 32; n++ {
		front := w.expiry.Front()
		if front == nil {
			break
		}
		e := front.Value.(*entry)
		if now.Sub(e.start) < w.Window {
			break
		}
		delete(w.entries, e.key)
		w.expiry.Remove(front)
	}
	return len(w.entries) < w.MaxKeys
}

// Semaphore caps concurrent in-flight requests for one route group.
type Semaphore struct {
	slots chan struct{}
}

// NewSemaphore returns a cap of n; n <= 0 disables it.
func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		return &Semaphore{}
	}
	return &Semaphore{slots: make(chan struct{}, n)}
}

// TryAcquire takes a slot without waiting; release must be called when it returns true.
func (s *Semaphore) TryAcquire() (release func(), ok bool) {
	if s == nil || s.slots == nil {
		return func() {}, true
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, true
	default:
		return nil, false
	}
}

// ProxyPolicy decides which peers may supply the client address. The zero value
// trusts no peer: the TCP peer address is always the client.
type ProxyPolicy struct {
	trusted []*net.IPNet
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or single IPs. An empty
// list trusts no peer. Any invalid entry is an error so a typo cannot silently widen
// or narrow the policy.
func ParseTrustedProxies(csv string) (ProxyPolicy, error) {
	var p ProxyPolicy
	for _, raw := range strings.Split(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				return ProxyPolicy{}, fmt.Errorf("invalid trusted proxy %q", s)
			}
			if ip.To4() != nil {
				s += "/32"
			} else {
				s += "/128"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return ProxyPolicy{}, fmt.Errorf("invalid trusted proxy %q: %w", raw, err)
		}
		p.trusted = append(p.trusted, n)
	}
	return p, nil
}

// Enabled reports whether any proxy is trusted.
func (p ProxyPolicy) Enabled() bool { return len(p.trusted) > 0 }

func (p ProxyPolicy) isTrusted(ip net.IP) bool {
	for _, n := range p.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the address to account a request to. When the TCP peer is not a
// trusted proxy, the peer is the client and X-Forwarded-For is ignored. When it is,
// X-Forwarded-For is read right to left, skipping trusted proxies, and the first
// address that is not a trusted proxy is the client: entries further left were
// supplied by the client and cannot be trusted. A malformed hop stops the walk and
// the request is accounted to the peer, so spoofed values never create new buckets.
func (p ProxyPolicy) ClientIP(r *http.Request) string {
	peer := remoteHost(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if !p.Enabled() || peerIP == nil || !p.isTrusted(peerIP) {
		return peer
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return peer
		}
		if !p.isTrusted(ip) {
			return ip.String()
		}
	}
	return peer
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// RetrySeconds rounds a retry delay up to whole seconds, at least 1.
func RetrySeconds(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// statusRecorder observes the status code a downstream handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// FailureGuard bounds authentication work spent on clients that keep failing it.
//
//   - PerSource counts 401/403 responses per client address. A tracked address over
//     its allowance is refused with 429 before any signature work.
//   - Overflow is a single shared budget used only when PerSource cannot track a new
//     address because its table holds MaxKeys live entries. Failures from untracked
//     addresses reserve there before authentication; while exhausted every untracked address
//     is refused with 503 before signature work until the window resets.
//
// Pending requests reserve budget atomically; successful completions refund it.
// Concurrent authentication is therefore bounded even before failures return.
//
// Requests that name TrustedSigner (the Recall API signing key) in Epistula-Signed-By
// are never accounted or refused here: they always reach signature verification. The
// budgets are keyed by address, and anyone can spend an address's budget (or, by
// filling the table, the shared overflow), so refusing on them would let a third party
// make the miner refuse the prober's valid requests and fail its probes. A forged claim
// costs one signature verification on a body already bounded by the body limit, and
// still fails authentication.
//
// IPv6 sources are accounted per /64, so one host's address block counts as one source
// and cannot fill the table on its own.
type FailureGuard struct {
	PerSource     *Window
	Overflow      *Window
	Proxies       ProxyPolicy
	TrustedSigner string
}

// sourceKey groups an IPv6 client into its /64; IPv4 (and IPv4-mapped) addresses and
// anything unparsable are used as they are.
func sourceKey(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() != nil {
		return addr
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

const overflowKey = "*"

// Middleware wraps next with the guard; a disabled PerSource window disables the guard.
func (g FailureGuard) Middleware(next http.Handler) http.Handler {
	if !g.PerSource.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.TrustedSigner != "" && strings.TrimSpace(r.Header.Get("Epistula-Signed-By")) == g.TrustedSigner {
			next.ServeHTTP(w, r)
			return
		}
		ip := sourceKey(g.Proxies.ClientIP(r))
		out, retry, settle := g.PerSource.reserveAuth(ip)
		if out == OverLimit {
			reject(w, http.StatusTooManyRequests, retry, "too many failed authentication attempts")
			return
		}
		if out == PendingFull {
			reject(w, http.StatusServiceUnavailable, retry, "authentication work at capacity")
			return
		}
		if out == TableFull {
			if !g.Overflow.Enabled() {
				reject(w, http.StatusServiceUnavailable, retry, "authentication failure table full")
				return
			}
			out, retry, settle = g.Overflow.reserveAuth(overflowKey)
			if out != Allowed {
				reject(w, http.StatusServiceUnavailable, retry, "authentication failure budget exhausted")
				return
			}
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		settle(rec.status != http.StatusUnauthorized && rec.status != http.StatusForbidden)
	})
}

// KeyLimit applies a per-key request limit; keyFn returns "" to skip limiting.
func KeyLimit(limiter *Window, keyFn func(*http.Request) string, next http.Handler) http.Handler {
	if !limiter.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFn(r)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if ok, retry := limiter.Allow(key); !ok {
			reject(w, http.StatusServiceUnavailable, retry, "request rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Concurrency bounds in-flight requests through sem; a full route answers 503 with a
// one-second Retry-After rather than queueing, so the caller's timeout budget is kept.
func Concurrency(sem *Semaphore, next http.Handler) http.Handler {
	if sem == nil || sem.slots == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := sem.TryAcquire()
		if !ok {
			reject(w, http.StatusServiceUnavailable, time.Second, "route at capacity")
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// reject writes a JSON refusal with Retry-After. Capacity, rate and overflow refusals
// use 503: the Recall API records any 4xx from a miner as a content ("ops") failure
// that trips the tier gate, while a 5xx is an availability failure, which is what an
// honest miner under load is. The per-source failure guard uses 429 because a source
// that keeps failing authentication is not a scored miner response.
func reject(w http.ResponseWriter, status int, retry time.Duration, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", RetrySeconds(retry))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"detail":"` + detail + `"}`))
}
