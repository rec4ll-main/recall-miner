package app

import (
	"net/http"
	"sync"
	"time"
)

// Calls to the Recall API (registration and heartbeats) use their own HTTP client.
//
// With the shared client (HTTP_TIMEOUT_SECONDS, 120 s by default, and Go's default
// connection reuse) a connection to the API's edge that died without being closed was
// reused for every later heartbeat: each one waited the full 120 s and failed, and the
// miner stayed silent until it was restarted.
const (
	// recallAPITimeout bounds one registration or heartbeat call, well under the 30 s
	// heartbeat interval so a failed call never swallows the next tick.
	recallAPITimeout = 15 * time.Second
	// recallAPIPingAfter / recallAPIPingTimeout: an HTTP/2 connection that receives nothing
	// for this long is pinged, and closed if the ping is not answered in time.
	recallAPIPingAfter   = 15 * time.Second
	recallAPIPingTimeout = 5 * time.Second
	recallAPIIdleTimeout = 60 * time.Second
)

// recallAPIClient sends registration and heartbeat requests. After any failed request it
// replaces its connection pool, so the next request dials a fresh connection: Go's HTTP/2
// client keeps using a connection whose cancelled stream is still unacknowledged, which a
// silently dead connection never acknowledges. The old pool's connections are closed by
// the HTTP/2 health ping.
type recallAPIClient struct {
	timeout time.Duration
	mu      sync.Mutex
	c       *http.Client
}

func newRecallAPIClient(timeout time.Duration) *recallAPIClient {
	return &recallAPIClient{timeout: timeout, c: newRecallAPIHTTPClient(timeout)}
}

func newRecallAPIHTTPClient(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.IdleConnTimeout = recallAPIIdleTimeout
	t.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: recallAPIPingAfter,
		PingTimeout:     recallAPIPingTimeout,
	}
	return &http.Client{Timeout: timeout, Transport: t}
}

func (r *recallAPIClient) client() *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c
}

// Do sends one request; on failure the pool is replaced (once per failed client).
func (r *recallAPIClient) Do(req *http.Request) (*http.Response, error) {
	c := r.client()
	resp, err := c.Do(req)
	if err != nil {
		r.mu.Lock()
		if r.c == c {
			r.c = newRecallAPIHTTPClient(r.timeout)
		}
		r.mu.Unlock()
		c.CloseIdleConnections()
	}
	return resp, err
}
