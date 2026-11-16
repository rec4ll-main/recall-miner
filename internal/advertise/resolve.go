package advertise

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"recall-miner/internal/config"
)

const resolveTimeout = 3 * time.Second

func Resolve(cfg config.Config) (advertiseURL string, source string, err error) {
	if strings.TrimSpace(cfg.MinerAPIURL) != "" {
		if _, err := url.ParseRequestURI(cfg.MinerAPIURL); err != nil {
			return "", "", fmt.Errorf("MINER_API_URL is not a valid URL: %w", err)
		}
		return cfg.MinerAPIURL, "url", nil
	}
	host := strings.TrimSpace(cfg.ExternalIP)
	if host == "" {
		host = strings.TrimSpace(cfg.ExternalHost)
	}
	if host != "" {
		if err := validateHost(host, cfg, false); err != nil {
			return "", "", err
		}
		return buildURL(cfg, host), "explicit", nil
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.AdvertiseMode))
	if mode == "" || mode == "off" {
		if cfg.Environment == "dev" || cfg.Environment == "test" {
			return buildURL(cfg, "127.0.0.1"), "dev-default", nil
		}
		return "", "", fmt.Errorf("advertise host unset: set MINER_API_URL or MINER_EXTERNAL_IP (or MINER_EXTERNAL_HOST), or MINER_ADVERTISE_MODE=auto")
	}
	if mode != "auto" {
		return "", "", fmt.Errorf("invalid MINER_ADVERTISE_MODE %q (want off|auto)", cfg.AdvertiseMode)
	}
	ip, src, err := autoDiscover(cfg)
	if err != nil {
		return "", "", fmt.Errorf("advertise auto-derive failed (fail-closed): %w", err)
	}
	if err := validateHost(ip, cfg, true); err != nil {
		return "", "", fmt.Errorf("advertise auto-derive failed (fail-closed): %w", err)
	}
	return buildURL(cfg, ip), src, nil
}

func buildURL(cfg config.Config, host string) string {
	scheme := "http"
	if cfg.TLSEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, advertisePort(cfg))
}

func advertisePort(cfg config.Config) int {
	if cfg.ExternalPort > 0 {
		return cfg.ExternalPort
	}
	if cfg.InternalPort > 0 {
		return cfg.InternalPort
	}
	if p := portFromListenAddr(cfg.ListenAddr); p > 0 {
		return p
	}
	return 8001
}

func portFromListenAddr(addr string) int {
	if strings.HasPrefix(addr, ":") {
		p, err := strconv.Atoi(strings.TrimPrefix(addr, ":"))
		if err == nil {
			return p
		}
		return 0
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}

func validateHost(host string, cfg config.Config, fromAuto bool) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("advertise host is empty")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if fromAuto {
			return fmt.Errorf("auto-derive returned non-IP %q", host)
		}
		if strings.ContainsAny(host, "/ ") {
			return fmt.Errorf("invalid advertise host %q", host)
		}
		return nil
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		if cfg.AllowPrivateAdvertise && ip.IsLoopback() && !fromAuto {
			return nil
		}
		return fmt.Errorf("advertise host %q is loopback, unspecified, or link-local", host)
	}
	requirePublic := cfg.AdvertiseRequirePublic
	if fromAuto && !cfg.AdvertiseRequirePublicSet {
		requirePublic = true
	}
	if requirePublic && (ip.IsPrivate() || ip.IsMulticast()) {
		if cfg.AllowPrivateAdvertise && !fromAuto {
			return nil
		}
		return fmt.Errorf("advertise host %q is not a public address", host)
	}
	return nil
}

func autoDiscover(cfg config.Config) (ip string, source string, err error) {
	src := strings.ToUpper(strings.TrimSpace(cfg.AdvertiseSource))
	if src == "" {
		src = SourceE1
	}
	if src == SourceS1 {
		return "", "", fmt.Errorf("STUN source S1 not implemented in v1; use E1 or E2 or set MINER_EXTERNAL_IP")
	}
	endpoint, ok := HTTPSEchoAllowlist[src]
	if !ok {
		return "", "", fmt.Errorf("advertise source %q is not in the HTTPS allowlist", src)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	client := &http.Client{
		Timeout: resolveTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("redirect not allowed for advertise auto-derive")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("echo %s returned status %d", src, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", "", err
	}
	ip = strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", "", fmt.Errorf("echo %s returned non-IP %q", src, ip)
	}
	return ip, "auto:" + src, nil
}