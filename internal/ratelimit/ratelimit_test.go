package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func fixedClock(w *Window, t *time.Time) { w.now = func() time.Time { return *t } }

func TestWindow_AllowsLimitThenRejectsUntilReset(t *testing.T) {
	w := NewWindow(3, time.Minute, 0)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fixedClock(w, &now)
	for i := 0; i < 3; i++ {
		if ok, _ := w.Allow("k"); !ok {
			t.Fatalf("event %d should be allowed", i)
		}
	}
	ok, retry := w.Allow("k")
	if ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("4th event must be rejected with a retry inside the window, got ok=%v retry=%v", ok, retry)
	}
	if over, _ := w.Peek("k"); !over {
		t.Fatal("Peek must report the key over the limit")
	}
	now = now.Add(time.Minute)
	if ok, _ := w.Allow("k"); !ok {
		t.Fatal("window reset must allow again")
	}
	if over, _ := w.Peek("other"); over {
		t.Fatal("unknown key is never over")
	}
}

func TestWindow_RecordReportsTableFullAndEvictsExpired(t *testing.T) {
	if ok, _ := NewWindow(0, time.Minute, 0).Allow("x"); !ok {
		t.Fatal("limit 0 disables the limiter")
	}
	w := NewWindow(10, time.Minute, 2)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fixedClock(w, &now)
	w.Record("a")
	w.Record("b")
	if !w.Saturated() || w.Tracked("c") {
		t.Fatal("table with two live entries must be saturated and c untracked")
	}
	if out, retry := w.Record("c"); out != TableFull || retry != time.Minute {
		t.Fatalf("full table must report TableFull without recording, got %v %v", out, retry)
	}
	if out, _ := w.Record("a"); out != Allowed {
		t.Fatalf("tracked keys keep counting while the table is full, got %v", out)
	}
	now = now.Add(time.Minute)
	if w.Saturated() {
		t.Fatal("expired entries free the table")
	}
	if out, _ := w.Record("c"); out != Allowed || !w.Tracked("c") {
		t.Fatal("after expiry a new key is tracked")
	}
}

func TestSemaphore_CapsAndReleases(t *testing.T) {
	s := NewSemaphore(2)
	r1, ok1 := s.TryAcquire()
	r2, ok2 := s.TryAcquire()
	_, ok3 := s.TryAcquire()
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("expected two slots: %v %v %v", ok1, ok2, ok3)
	}
	r1()
	if _, ok := s.TryAcquire(); !ok {
		t.Fatal("released slot must be reusable")
	}
	r2()
	if _, ok := NewSemaphore(0).TryAcquire(); !ok {
		t.Fatal("cap 0 disables the semaphore")
	}
}

// --------------------------------------------------------------- proxy policy

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestProxyPolicy_DirectModeIgnoresForwardedFor(t *testing.T) {
	var none ProxyPolicy
	if got := none.ClientIP(req("203.0.113.9:4444", "198.51.100.7")); got != "203.0.113.9" {
		t.Fatalf("no trusted proxies: peer expected, got %q", got)
	}
	p, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ClientIP(req("203.0.113.9:4444", "198.51.100.7")); got != "203.0.113.9" {
		t.Fatalf("untrusted peer cannot supply a client address, got %q", got)
	}
}

func TestProxyPolicy_TrustedProxyUsesRightmostUntrustedHop(t *testing.T) {
	p, err := ParseTrustedProxies("10.0.0.0/8, 192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		r      *http.Request
		expect string
	}{
		{"single hop", req("10.0.0.5:1", "198.51.100.7"), "198.51.100.7"},
		{"client-supplied spoof on the left is ignored", req("10.0.0.5:1", "1.2.3.4, 198.51.100.7"), "198.51.100.7"},
		{"chained trusted proxies are skipped", req("10.0.0.5:1", "198.51.100.7, 192.0.2.10, 10.1.1.1"), "198.51.100.7"},
		{"multiple header lines", req("10.0.0.5:1", "1.2.3.4", "198.51.100.7"), "198.51.100.7"},
		{"malformed rightmost hop falls back to the peer", req("10.0.0.5:1", "198.51.100.7, not-an-ip"), "10.0.0.5"},
		{"no header falls back to the peer", req("10.0.0.5:1"), "10.0.0.5"},
		{"only trusted hops fall back to the peer", req("10.0.0.5:1", "10.2.2.2"), "10.0.0.5"},
		{"ipv6 client", req("10.0.0.5:1", "2001:db8::1"), "2001:db8::1"},
	}
	for _, c := range cases {
		if got := p.ClientIP(c.r); got != c.expect {
			t.Errorf("%s: got %q want %q", c.name, got, c.expect)
		}
	}
}

func TestParseTrustedProxies_RejectsInvalidEntries(t *testing.T) {
	for _, bad := range []string{"10.0.0.0/33", "not-a-cidr", "10.0.0.0/8, nope"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if p, err := ParseTrustedProxies(" , "); err != nil || p.Enabled() {
		t.Fatalf("blank list trusts nothing: %v %v", p, err)
	}
}

// --------------------------------------------------------------- failure guard

func authHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("X-Good") == "1" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func send(h http.Handler, ip string, good bool) int {
	r := httptest.NewRequest(http.MethodPost, "/rerank", nil)
	r.RemoteAddr = ip + ":1"
	if good {
		r.Header.Set("X-Good", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestFailureGuard_CutsOffRepeatedFailuresOnly(t *testing.T) {
	calls := 0
	g := FailureGuard{PerSource: NewWindow(2, time.Minute, 0), Overflow: NewWindow(100, time.Minute, 1)}
	h := g.Middleware(authHandler(&calls))
	if send(h, "203.0.113.9", true) != 200 || send(h, "203.0.113.9", true) != 200 {
		t.Fatal("good requests pass and are not counted")
	}
	if send(h, "203.0.113.9", false) != 401 || send(h, "203.0.113.9", false) != 401 {
		t.Fatal("first failures reach the handler")
	}
	before := calls
	if code := send(h, "203.0.113.9", false); code != http.StatusTooManyRequests || calls != before {
		t.Fatalf("third attempt must be refused before the handler, got %d (handler ran %d)", code, calls-before)
	}
	if send(h, "198.51.100.5", false) != http.StatusUnauthorized {
		t.Fatal("another source still reaches authentication")
	}
}

// Adversarial saturation: fill the per-source table with distinct failing sources,
// then show that unseen sources can no longer reach authentication without bound.
func TestFailureGuard_TableSaturationUsesBoundedOverflow(t *testing.T) {
	const tableSize, overflow = 50, 5
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	per := NewWindow(3, time.Minute, tableSize)
	ov := NewWindow(overflow, time.Minute, 1)
	fixedClock(per, &now)
	fixedClock(ov, &now)
	calls := 0
	h := FailureGuard{PerSource: per, Overflow: ov}.Middleware(authHandler(&calls))

	for i := 0; i < tableSize; i++ {
		if code := send(h, fmt.Sprintf("10.1.%d.%d", i/256, i%256), false); code != 401 {
			t.Fatalf("filling source %d: got %d", i, code)
		}
	}
	if !per.Saturated() {
		t.Fatal("table must be saturated")
	}
	// Unseen sources: exactly `overflow` failures are admitted to authentication.
	admitted, refused := 0, 0
	for i := 0; i < 1000; i++ {
		switch send(h, fmt.Sprintf("198.18.%d.%d", i/256, i%256), false) {
		case 401:
			admitted++
		case http.StatusServiceUnavailable:
			refused++
		default:
			t.Fatal("unexpected status for an unseen source")
		}
	}
	if admitted != overflow || refused != 1000-overflow {
		t.Fatalf("overflow must bound unseen sources: admitted=%d refused=%d", admitted, refused)
	}
	// Tracked sources keep their own accounting while the table is full.
	if send(h, "10.1.0.0", false) != 401 {
		t.Fatal("a tracked source under its allowance still reaches authentication")
	}
	// The window resets: the table frees up and unseen sources are tracked again.
	now = now.Add(time.Minute)
	if code := send(h, "198.18.9.9", false); code != 401 {
		t.Fatalf("after the window an unseen source is admitted again, got %d", code)
	}
	if !per.Tracked("198.18.9.9") {
		t.Fatal("after the window a failing source is tracked per source")
	}
}

func TestFailureGuard_UsesProxyPolicyForAccounting(t *testing.T) {
	p, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := FailureGuard{PerSource: NewWindow(1, time.Minute, 0), Overflow: NewWindow(10, time.Minute, 1), Proxies: p}.Middleware(authHandler(&calls))
	do := func(xff string) int {
		r := httptest.NewRequest(http.MethodPost, "/rerank", nil)
		r.RemoteAddr = "10.0.0.5:1"
		r.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if do("198.51.100.7") != 401 || do("198.51.100.7") != http.StatusTooManyRequests {
		t.Fatal("the forwarded client is accounted individually behind a trusted proxy")
	}
	if do("198.51.100.8") != 401 {
		t.Fatal("another client behind the same proxy is not banned with it")
	}
	if do("6.6.6.6, 198.51.100.7") != http.StatusTooManyRequests {
		t.Fatal("a spoofed left-most hop cannot escape the client's bucket")
	}
}

// --------------------------------------------------------------- key limit and concurrency

func TestKeyLimit_PerKeyAndSkipOnEmptyKey(t *testing.T) {
	lim := NewWindow(1, time.Minute, 0)
	h := KeyLimit(lim, func(r *http.Request) string { return r.Header.Get("X-Key") }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(key string) int {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		if key != "" {
			r.Header.Set("X-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if do("a") != 200 || do("a") != 503 || do("b") != 200 || do("") != 200 || do("") != 200 {
		t.Fatal("per-key limiting or empty-key skip broken")
	}
}

func TestConcurrency_RejectsWhenFull(t *testing.T) {
	sem := NewSemaphore(1)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h := Concurrency(sem, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/embed_text", nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never entered the handler")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/embed_text", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("second in-flight request must get 503 with Retry-After 1, got %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	close(release)
	wg.Wait()
	// The slot is free again; release is closed, so the handler returns immediately.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/embed_text", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("slot must be free again, got %d", rec.Code)
	}
}

func TestFailureGuard_ReservesConcurrentAdmissions(t *testing.T) {
	for _, saturated := range []bool{false, true} {
		t.Run(fmt.Sprint(saturated), func(t *testing.T) {
			per := NewWindow(3, time.Minute, 1)
			ov := NewWindow(2, time.Minute, 1)
			if saturated {
				per.Record("existing")
			}
			entered := make(chan struct{}, 100)
			release := make(chan struct{})
			done := make(chan struct{}, 100)
			h := FailureGuard{PerSource: per, Overflow: ov}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release; w.WriteHeader(401) }))
			for i := 0; i < 50; i++ {
				go func() { send(h, "203.0.113.1", false); done <- struct{}{} }()
			}
			// Wait until rejected requests finish, or all requests have reached auth in the broken implementation.
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			completed := 0
		wait:
			for completed < 48 {
				select {
				case <-done:
					completed++
				case <-timer.C:
					break wait
				}
			}
			admitted := len(entered)
			close(release)
			for completed < 50 {
				<-done
				completed++
			}
			max := 3
			if saturated {
				max = 2
			}
			if admitted > max {
				t.Fatalf("admitted %d simultaneous requests, budget %d", admitted, max)
			}
		})
	}
}

func TestReservationRefundDoesNotCreditReplacementWindow(t *testing.T) {
	now := time.Unix(100, 0)
	w := NewWindow(1, time.Minute, 1)
	fixedClock(w, &now)
	out, _, refund := w.reserve("client")
	if out != Allowed {
		t.Fatal(out)
	}
	now = now.Add(time.Minute)
	if ok, _ := w.Allow("client"); !ok {
		t.Fatal("new window blocked")
	}
	refund()
	refund()
	if ok, _ := w.Allow("client"); ok {
		t.Fatal("late refund credited a new window")
	}
}
func TestWindow_ExpiryMetadataIsBounded(t *testing.T) {
	now := time.Unix(100, 0)
	w := NewWindow(1, time.Minute, 2)
	fixedClock(w, &now)
	for i := 0; i < 1000; i++ {
		now = now.Add(time.Minute)
		w.Allow("a")
		w.Allow("b")
		w.Allow(fmt.Sprint(i))
		if len(w.entries) > 2 || w.expiry.Len() > 2 {
			t.Fatal("unbounded accounting")
		}
	}
}
func TestWindow_SaturationDoesNotScanLiveTable(t *testing.T) {
	now := time.Unix(100, 0)
	w := NewWindow(1, time.Minute, 10000)
	fixedClock(w, &now)
	for i := 0; i < w.MaxKeys; i++ {
		w.Allow(fmt.Sprint(i))
	}
	// The expiry queue's first live entry ends cleanup; repeated rejection must not
	// change the queue or create metadata for rejected identities.
	first := w.expiry.Front()
	for i := 0; i < 10000; i++ {
		if out, _ := w.Record("untracked"); out != TableFull {
			t.Fatal(out)
		}
	}
	if w.expiry.Front() != first || w.expiry.Len() != w.MaxKeys {
		t.Fatal("live queue changed")
	}
}

// --------------------------------------------------------------- prober never refused by address

const testProber = "5ProberSigningKeyForTestsOnly"

func sendAs(h http.Handler, addr, signedBy string, good bool) int {
	r := httptest.NewRequest(http.MethodPost, "/rerank", nil)
	r.RemoteAddr = addr
	if signedBy != "" {
		r.Header.Set("Epistula-Signed-By", signedBy)
	}
	if good {
		r.Header.Set("X-Good", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// External review: unsigned requests from the address the prober shares (a proxy or NAT
// in front of the miner) used to put that address over its allowance, and the prober's
// valid requests were then refused with 429 before signature verification.
func TestFailureGuard_SharedAddressFloodDoesNotRefuseProber(t *testing.T) {
	calls := 0
	g := FailureGuard{PerSource: NewWindow(60, time.Minute, 10000), Overflow: NewWindow(600, time.Minute, 1), TrustedSigner: testProber}
	h := g.Middleware(authHandler(&calls))
	for i := 0; i < 200; i++ {
		sendAs(h, "203.0.113.1:1", "", false)
	}
	if code := sendAs(h, "203.0.113.1:1", "", false); code != http.StatusTooManyRequests {
		t.Fatalf("the flooding address itself is still cut off, got %d", code)
	}
	if code := sendAs(h, "203.0.113.1:1", testProber, true); code != http.StatusOK {
		t.Fatalf("the prober's valid request from the same address must reach verification, got %d", code)
	}
}

// External review: filling the address table from one IPv6 /64 and then spending the
// shared overflow made every unseen address, the prober's included, get 503.
func TestFailureGuard_TableAndOverflowExhaustionDoesNotRefuseProber(t *testing.T) {
	const tableSize, overflow = 50, 5
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	per := NewWindow(3, time.Minute, tableSize)
	ov := NewWindow(overflow, time.Minute, 1)
	fixedClock(per, &now)
	fixedClock(ov, &now)
	calls := 0
	h := FailureGuard{PerSource: per, Overflow: ov, TrustedSigner: testProber}.Middleware(authHandler(&calls))
	for i := 0; i < tableSize; i++ {
		sendAs(h, fmt.Sprintf("10.2.%d.%d:1", i/256, i%256), "", false)
	}
	for i := 0; i < 2*overflow; i++ {
		sendAs(h, fmt.Sprintf("198.18.%d.%d:1", i/256, i%256), "", false)
	}
	if code := sendAs(h, "192.0.2.77:1", "", false); code != http.StatusServiceUnavailable {
		t.Fatalf("an unseen unsigned source is still refused while the budget is spent, got %d", code)
	}
	if code := sendAs(h, "192.0.2.77:1", testProber, true); code != http.StatusOK {
		t.Fatalf("the prober from an unseen address must reach verification, got %d", code)
	}
}

// A forged prober claim is not refused by the guard, but it still fails authentication
// and does not spend the address's budget for anyone else.
func TestFailureGuard_ForgedProberClaimStillFailsAuthentication(t *testing.T) {
	calls := 0
	g := FailureGuard{PerSource: NewWindow(1, time.Minute, 0), Overflow: NewWindow(10, time.Minute, 1), TrustedSigner: testProber}
	h := g.Middleware(authHandler(&calls))
	for i := 0; i < 20; i++ {
		if code := sendAs(h, "198.51.100.9:1", testProber, false); code != http.StatusUnauthorized {
			t.Fatalf("a forged claim reaches verification and fails it, got %d", code)
		}
	}
	if calls != 20 {
		t.Fatalf("every forged claim was verified, handler ran %d times", calls)
	}
	if code := sendAs(h, "198.51.100.9:1", "", false); code != http.StatusUnauthorized {
		t.Fatalf("forged claims did not spend the address's own allowance, got %d", code)
	}
	if code := sendAs(h, "198.51.100.9:1", "5SomeoneElse", false); code != http.StatusTooManyRequests {
		t.Fatalf("other signers from the address are accounted as before, got %d", code)
	}
}

// With no trusted signer configured the guard behaves as before for every request.
func TestFailureGuard_NoTrustedSignerConfigured(t *testing.T) {
	calls := 0
	h := FailureGuard{PerSource: NewWindow(1, time.Minute, 0), Overflow: NewWindow(10, time.Minute, 1)}.Middleware(authHandler(&calls))
	sendAs(h, "198.51.100.10:1", testProber, false)
	if code := sendAs(h, "198.51.100.10:1", testProber, false); code != http.StatusTooManyRequests {
		t.Fatalf("without a trusted signer a claim earns no exemption, got %d", code)
	}
}

func TestFailureGuard_IPv6CountedPerSlash64(t *testing.T) {
	calls := 0
	per := NewWindow(3, time.Minute, 100)
	h := FailureGuard{PerSource: per, Overflow: NewWindow(10, time.Minute, 1)}.Middleware(authHandler(&calls))
	for i := 1; i <= 3; i++ {
		if code := sendAs(h, fmt.Sprintf("[2001:db8:1:2::%x]:1", i), "", false); code != http.StatusUnauthorized {
			t.Fatalf("failure %d reaches authentication, got %d", i, code)
		}
	}
	if code := sendAs(h, "[2001:db8:1:2:ffff::1]:1", "", false); code != http.StatusTooManyRequests {
		t.Fatalf("another address in the same /64 shares the allowance, got %d", code)
	}
	if code := sendAs(h, "[2001:db8:1:3::1]:1", "", false); code != http.StatusUnauthorized {
		t.Fatalf("the next /64 is a separate source, got %d", code)
	}
	if !per.Tracked("2001:db8:1:2::/64") {
		t.Fatal("the /64 is the tracked key")
	}
}

func TestSourceKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.4":          "203.0.113.4",
		"::ffff:203.0.113.4":   "::ffff:203.0.113.4",
		"2001:db8:a:b:c:d:e:f": "2001:db8:a:b::/64",
		"not-an-address":       "not-an-address",
	} {
		if got := sourceKey(in); got != want {
			t.Errorf("sourceKey(%q) = %q, want %q", in, got, want)
		}
	}
}
