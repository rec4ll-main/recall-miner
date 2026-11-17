package app

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freezeProxy forwards TCP to target. freeze() makes every connection open at that moment
// a black hole (bytes are swallowed, nothing is closed), like a NAT or edge connection that
// died silently; connections opened later work normally.
type freezeProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*atomic.Bool
	dials  atomic.Int32
}

func newFreezeProxy(t *testing.T, target string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go p.serve()
	return p
}

func (p *freezeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.dials.Add(1)
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		frozen := &atomic.Bool{}
		p.mu.Lock()
		p.conns = append(p.conns, frozen)
		p.mu.Unlock()
		pipe := func(dst, src net.Conn) {
			buf := make([]byte, 32*1024)
			for {
				n, err := src.Read(buf)
				if err != nil {
					dst.Close()
					return
				}
				if !frozen.Load() {
					if _, err := dst.Write(buf[:n]); err != nil {
						return
					}
				}
			}
		}
		go pipe(up, c)
		go pipe(c, up)
	}
}

func (p *freezeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.conns {
		f.Store(true)
	}
}

func h2TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// testRecallAPIClient trusts the test server's certificate in every pool the client builds.
type recallAPIClientForTest struct {
	*recallAPIClient
	tlsCfg *tls.Config
}

func (c *recallAPIClientForTest) trustNewPool() {
	tr := c.client().Transport.(*http.Transport)
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		tr.TLSClientConfig = c.tlsCfg.Clone()
	}
}

func testRecallAPIClient(srv *httptest.Server, timeout time.Duration) *recallAPIClientForTest {
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig
	r := newRecallAPIClient(timeout)
	r.c.Transport.(*http.Transport).TLSClientConfig = tlsCfg.Clone()
	return &recallAPIClientForTest{recallAPIClient: r, tlsCfg: tlsCfg}
}

func post(t *testing.T, c *recallAPIClientForTest, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	c.trustNewPool()
	return resp, err
}

func TestRecallAPIClientSettings(t *testing.T) {
	c := newRecallAPIClient(recallAPITimeout).client()
	if c.Timeout != 15*time.Second || recallAPITimeout >= 30*time.Second {
		t.Fatalf("timeout %v must stay under the 30 s heartbeat interval", c.Timeout)
	}
	tr := c.Transport.(*http.Transport)
	if tr == http.DefaultTransport || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatal("the Recall API client needs its own transport with HTTP/2 health pings")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must stay enabled on the cloned transport")
	}
}

// A connection that dies silently costs one failed call; the next call dials a new
// connection and succeeds (before the fix every later call reused the dead connection).
func TestRecallAPIRecoversFromSilentlyDeadConnection(t *testing.T) {
	srv := h2TestServer(t)
	proxy := newFreezeProxy(t, srv.Listener.Addr().String())
	url := "https://" + proxy.ln.Addr().String() + "/api/miner/heartbeat"
	c := testRecallAPIClient(srv, 2*time.Second)

	resp, err := post(t, c, url)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("test needs HTTP/2, got %s", resp.Proto)
	}
	resp.Body.Close()

	proxy.freeze()
	if _, err := post(t, c, url); err == nil {
		t.Fatal("a call on the frozen connection must fail")
	}
	resp, err = post(t, c, url)
	if err != nil {
		t.Fatalf("call after a failure must use a fresh connection: %v", err)
	}
	resp.Body.Close()
	if d := proxy.dials.Load(); d != 2 {
		t.Fatalf("dials=%d, want 2 (original + one fresh after the failure)", d)
	}
}
