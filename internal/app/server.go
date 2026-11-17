package app

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/auth"
	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/handlers"
	"recall-miner/internal/heartbeat"
	"recall-miner/internal/httpx"
	"recall-miner/internal/ratelimit"
	"recall-miner/internal/registry"
	"recall-miner/internal/tlscert"
)

type server struct {
	cfg        config.Config
	log        *slog.Logger
	httpClient *http.Client
	recallAPI  *recallAPIClient // registration and heartbeats only (recall_api_client.go)
	heartbeats *heartbeat.Store
	keypair    *signature.KeyringPair
	handlers   *handlers.Handlers
	replays    *auth.ReplayCache
	tlsConfig  *tls.Config // non-nil only when TLS_ENABLED
	spkiPin    string
	limits     overloadControls
}

// overloadControls are the in-process limiters (SECURITY_BACKLOG "Rate Limiting").
type overloadControls struct {
	quiet        *quietGate             // outermost: plain 404 for public requests not meant for this miner
	failureGuard ratelimit.FailureGuard // pre-auth: 401/403 per client address, bounded overflow
	perHotkey    *ratelimit.Window      // post-Epistula: requests per signer hotkey
	dbCreate     *ratelimit.Window      // POST /db/create per signer hotkey
	embed        *ratelimit.Semaphore   // /embed_text, /embed_text/batch
	rerank       *ratelimit.Semaphore
	query        *ratelimit.Semaphore // /db/{id}/query
	store        *ratelimit.Semaphore // /db/{id}/store, /db/{id}/restore
	generate     *ratelimit.Semaphore
}

func newOverloadControls(cfg config.Config) overloadControls {
	// The policy is validated at startup (validateOverloadConfig); an invalid list
	// here therefore only occurs in tests and degrades to trusting no proxy.
	proxies, _ := ratelimit.ParseTrustedProxies(cfg.RateLimitTrustedProxies)
	return overloadControls{
		quiet: newQuietGate(cfg.ApplicationIdentity(), proxies, nil),
		failureGuard: ratelimit.FailureGuard{
			PerSource: ratelimit.NewWindow(cfg.RateLimitAuthFailuresPerMin, time.Minute, cfg.RateLimitFailureTableSize),
			Overflow:  ratelimit.NewWindow(cfg.RateLimitFailureOverflow, time.Minute, 1),
			Proxies:   proxies,
			// The prober is never refused by address: see FailureGuard.
			TrustedSigner: cfg.ApplicationIdentity(),
		},
		perHotkey: ratelimit.NewWindow(cfg.RateLimitValidatorPerMin, time.Minute, 0),
		dbCreate:  ratelimit.NewWindow(cfg.RateLimitDBCreatePerMin, time.Minute, 0),
		embed:     ratelimit.NewSemaphore(cfg.EmbedMaxInflight),
		rerank:    ratelimit.NewSemaphore(cfg.RerankMaxInflight),
		query:     ratelimit.NewSemaphore(cfg.QueryMaxInflight),
		store:     ratelimit.NewSemaphore(cfg.StoreMaxInflight),
		generate:  ratelimit.NewSemaphore(cfg.GenerateMaxInflight),
	}
}

// verifiedHotkey is the per-signer limiter key, set by auth.Validator after the
// Epistula signature, allowlist and replay checks pass.
func verifiedHotkey(r *http.Request) string { return r.Header.Get("X-Verified-Hotkey") }

func (s *server) capped(sem *ratelimit.Semaphore, h http.HandlerFunc) http.Handler {
	return ratelimit.Concurrency(sem, h)
}

func newServer(cfg config.Config, logger *slog.Logger) *server {
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	apiClient := client.New(httpClient, cfg.InternalAPIKey)
	registry := registry.New()
	heartbeats := heartbeat.NewStore(cfg, logger)
	s := &server{
		cfg:        cfg,
		log:        logger,
		httpClient: httpClient,
		recallAPI:  newRecallAPIClient(recallAPITimeout),
		heartbeats: heartbeats,
		handlers:   handlers.New(cfg, logger, httpClient, apiClient, registry, heartbeats),
		replays:    auth.NewReplayCache(15*time.Second, cfg.ReplayCacheMaxEntries),
		limits:     newOverloadControls(cfg),
	}
	s.limits.quiet.log = logger
	return s
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// /health answers the miner's own machine and private network (Docker health checks)
	// unsigned; from anywhere else it needs the Recall API's signature like every route.
	signedHealth := s.validatorAuth(http.HandlerFunc(s.handlers.Health))
	mux.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limits.quiet.local(r) {
			s.handlers.Health(w, r)
			return
		}
		signedHealth.ServeHTTP(w, r)
	}))
	mux.Handle("GET /services", s.validatorAuth(http.HandlerFunc(s.handlers.Services)))
	// Per-route overload controls sit inside validatorAuth, so they only ever act on
	// authenticated traffic and answer 503 + Retry-After (availability, not content).
	mux.Handle("POST /db/create", s.validatorAuth(ratelimit.KeyLimit(s.limits.dbCreate, verifiedHotkey, http.HandlerFunc(s.handlers.DBCreate))))
	mux.Handle("DELETE /db/{db_id}", s.validatorAuth(http.HandlerFunc(s.handlers.DBDelete)))
	mux.Handle("GET /db/{db_id}", s.validatorAuth(http.HandlerFunc(s.handlers.DBInfo)))
	mux.Handle("POST /db/{db_id}/restore", s.validatorAuth(s.capped(s.limits.store, s.handlers.DBRestore)))
	mux.Handle("POST /db/{db_id}/store", s.validatorAuth(s.capped(s.limits.store, s.handlers.DBStore)))
	mux.Handle("DELETE /db/{db_id}/embed/{embed_id}", s.validatorAuth(http.HandlerFunc(s.handlers.DBDeleteEmbed)))
	mux.Handle("POST /db/{db_id}/retention-proof", s.validatorAuth(s.capped(s.limits.query, s.handlers.DBRetentionProof)))
	mux.Handle("POST /db/{db_id}/query", s.validatorAuth(s.capped(s.limits.query, s.handlers.DBQuery)))
	mux.Handle("POST /embed_text", s.validatorAuth(s.capped(s.limits.embed, s.handlers.EmbedText)))
	mux.Handle("POST /embed_text/batch", s.validatorAuth(s.capped(s.limits.embed, s.handlers.EmbedBatch)))
	mux.Handle("POST /rerank", s.validatorAuth(s.capped(s.limits.rerank, s.handlers.Rerank)))
	mux.Handle("POST /generate", s.validatorAuth(s.capped(s.limits.generate, s.handlers.Generate)))

	mux.Handle("POST /internal/services/register", s.internalAuth(http.HandlerFunc(s.handlers.RegisterService)))
	mux.Handle("POST /internal/services/heartbeat", s.internalAuth(http.HandlerFunc(s.handlers.ServiceHeartbeat)))
	mux.Handle("GET /internal/services/heartbeats", s.internalAuth(http.HandlerFunc(s.handlers.ListServiceHeartbeats)))
	mux.Handle("GET /internal/services", s.internalAuth(http.HandlerFunc(s.handlers.ListRegisteredServices)))
	mux.Handle("DELETE /internal/services/{name}", s.internalAuth(http.HandlerFunc(s.handlers.UnregisterService)))
	mux.Handle("GET /internal/services/{name}/config", s.internalAuth(http.HandlerFunc(s.handlers.GetSidecarConfig)))
	mux.Handle("POST /internal/services/{name}/model", s.internalAuth(http.HandlerFunc(s.handlers.SetSidecarModel)))
	// Outermost: requests from public addresses that are not for this miner get a plain
	// 404 (quietGate). Then bound the authentication work spent on sources that keep
	// failing it (signature-verification DoS); pending work reserves budget and success
	// refunds it.
	return s.limits.quiet.Middleware(s.limits.failureGuard.Middleware(httpx.BodyLimitMiddleware(s.cfg.MaxBodyBytes)(mux)))
}

func (s *server) TLSConfig() *tls.Config { return s.tlsConfig }
func (s *server) SPKIPin() string        { return s.spkiPin }

// minerTLSHosts returns the public endpoint host[:port] used as a certificate
// SAN, derived from MINER_API_URL when it carries a host.
func minerTLSHosts(minerAPIURL string, extra []string) []string {
	hosts := make([]string, 0, 1+len(extra))
	seen := map[string]struct{}{}
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	if minerAPIURL != "" {
		if u, err := url.Parse(minerAPIURL); err == nil && u.Host != "" {
			add(u.Host)
		}
	}
	for _, host := range extra {
		add(host)
	}
	return hosts
}

// initTLS loads or auto-generates the miner TLS certificate and computes the
// SPKI pin when TLS is enabled. The certificate CN is the miner hotkey SS58.
// It must run after MinerHotkey is resolved; errors fail startup.
func (s *server) initTLS(hotkey string) error {
	if !s.cfg.TLSEnabled {
		return nil
	}
	if hotkey == "" {
		return fmt.Errorf("TLS_ENABLED requires a miner hotkey for the certificate CN")
	}
	cert, err := tlscert.LoadOrGenerate(s.cfg.TLSCertFile, s.cfg.TLSKeyFile, hotkey, minerTLSHosts(s.cfg.MinerAPIURL, s.cfg.TLSExtraSANHosts), s.cfg.TLSAutoGenerate)
	if err != nil {
		return err
	}
	pin, err := tlscert.SPKIPin(cert)
	if err != nil {
		return err
	}
	s.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.spkiPin = pin
	return nil
}
