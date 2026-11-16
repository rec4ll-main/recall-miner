package app

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"recall-miner/internal/ratelimit"
)

// notFoundBody is what every refused request receives: the same plain 404 any web
// server gives for an unknown path, so a scan cannot tell this is a Recall miner.
const notFoundBody = "404 page not found\n"

// quietGate answers requests from outside the miner's own machine and private network
// with a plain 404 unless they are meant for this miner:
//
//   - requests that name the Recall API signing key in Epistula-Signed-By (they are then
//     verified as before; a failed verification is also answered 404), and
//   - /internal/ requests that carry X-Internal-Api-Key (sidecars on another machine;
//     a wrong key is also answered 404).
//
// Everything else from a public address (a scan, an unsigned request, a request signed
// by any other key) gets the 404 before any other work. Loopback and private addresses
// (Docker health checks, sidecars on the compose network) are served exactly as before.
// The client address is resolved with the same trusted-proxy policy as the failure guard.
// With no Recall API signing key configured (development only: startup refuses that
// unless VALIDATOR_ALLOWLIST_DEV_EMPTY=true) the gate is off and nothing is hidden.
type quietGate struct {
	trustedSigner string
	proxies       ratelimit.ProxyPolicy
	log           *slog.Logger
	lastLog       atomic.Int64
	refused       atomic.Int64
}

func newQuietGate(trustedSigner string, proxies ratelimit.ProxyPolicy, log *slog.Logger) *quietGate {
	return &quietGate{trustedSigner: strings.TrimSpace(trustedSigner), proxies: proxies, log: log}
}

// localAddress reports whether addr is loopback, private (RFC 1918, fc00::/7) or
// link-local, i.e. the miner's own machine or private network. Unparsable is not local.
func localAddress(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (g *quietGate) local(r *http.Request) bool {
	return g.trustedSigner == "" || localAddress(g.proxies.ClientIP(r))
}

// signedForThisMiner reports whether the request names the Recall API signing key. It is
// a claim only; the route's own authentication verifies it.
func (g *quietGate) signedForThisMiner(r *http.Request) bool {
	return g.trustedSigner != "" && strings.TrimSpace(r.Header.Get("Epistula-Signed-By")) == g.trustedSigner
}

func (g *quietGate) Middleware(next http.Handler) http.Handler {
	if g.trustedSigner == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.local(r) {
			next.ServeHTTP(w, r)
			return
		}
		internalKeyed := strings.HasPrefix(r.URL.Path, "/internal/") && r.Header.Get("X-Internal-Api-Key") != ""
		if !internalKeyed && !g.signedForThisMiner(r) {
			g.refuse(w, r)
			return
		}
		hw := &hideAuthFailures{ResponseWriter: w}
		next.ServeHTTP(hw, r)
		if hw.hidden {
			g.note(r)
		}
	})
}

func (g *quietGate) refuse(w http.ResponseWriter, r *http.Request) {
	writeNotFound(w)
	g.note(r)
}

// note logs refused requests at most once a minute, with how many were refused, so an
// operator can see why a client gets 404 without a scan being able to flood the log.
func (g *quietGate) note(r *http.Request) {
	n := g.refused.Add(1)
	if g.log == nil {
		return
	}
	now := time.Now().Unix()
	prev := g.lastLog.Load()
	if now-prev < 60 || !g.lastLog.CompareAndSwap(prev, now) {
		return
	}
	g.refused.Add(-n)
	g.log.Info("answered requests not signed by the Recall API with 404",
		"count_since_last", n, "last_path", r.URL.Path, "last_client", g.proxies.ClientIP(r))
}

func writeNotFound(w http.ResponseWriter) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundBody))
}

// hideAuthFailures turns a 401 or 403 from the miner's own authentication into the same
// plain 404, so a forged or wrong-key request learns nothing. Other statuses pass through.
type hideAuthFailures struct {
	http.ResponseWriter
	wrote  bool
	hidden bool
}

func (h *hideAuthFailures) WriteHeader(code int) {
	if h.wrote {
		return
	}
	h.wrote = true
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		h.hidden = true
		writeNotFound(h.ResponseWriter)
		return
	}
	h.ResponseWriter.WriteHeader(code)
}

func (h *hideAuthFailures) Write(b []byte) (int, error) {
	if !h.wrote {
		h.WriteHeader(http.StatusOK)
	}
	if h.hidden {
		return len(b), nil
	}
	return h.ResponseWriter.Write(b)
}
