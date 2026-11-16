package app

import (
	"net/http"

	"recall-miner/internal/auth"
	"recall-miner/internal/ratelimit"
)

func (s *server) validatorAuth(next http.Handler) http.Handler {
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlers.IncProcessedRequests()
		next.ServeHTTP(w, r)
	})
	signer := auth.NewResponseSigner(s.keypair, s.cfg.ResponseSigningEnabled, s.cfg.MaxBodyBytes, s.log)
	// Per-signer request limit runs after signature, allowlist and replay checks and
	// inside the response signer, so a 503 refusal is signed like any other response.
	limited := ratelimit.KeyLimit(s.limits.perHotkey, verifiedHotkey, counting)
	return auth.Validator(s.cfg.MinerHotkey, s.cfg.ApplicationIdentity(), s.cfg.EpistulaEnvelopeRequired, s.cfg.MaxBodyBytes, s.replays, s.log, signer.Wrap(limited))
}

func (s *server) internalAuth(next http.Handler) http.Handler {
	return auth.Internal(s.cfg.InternalAPIKey, next)
}
