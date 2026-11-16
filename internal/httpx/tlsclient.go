package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ClientWithRootCA returns an HTTP client that trusts caFile in addition to
// the system roots when caFile is non-empty. An empty caFile yields a plain
// client (system roots only). The certificate file is the miner TLS cert when
// sidecars call a native-TLS Compose miner over https://miner:8001; this is
// not a verification bypass.
func ClientWithRootCA(caFile string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if caFile == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read MINER_TLS_CA_FILE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("MINER_TLS_CA_FILE %q contains no certificates", caFile)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}
