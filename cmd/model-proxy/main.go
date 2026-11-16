package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"recall-miner/internal/httpx"

	"github.com/denisbrodbeck/machineid"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

type config struct {
	ListenAddr           string
	UpstreamURL          string
	UpstreamHealthPath   string
	InternalAPIKey       string
	ServiceName          string
	ServicePublicURL     string
	MinerAPIURL          string
	MinerTLSCAFile       string
	Capabilities         []string
	Models               []string
	Metadata             map[string]any
	RegisterOnStart      bool
	RegistrationInterval time.Duration
	MachineID            string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}

	cfg := configFromEnv()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if cfg.UpstreamURL == "" {
		logger.Error("UPSTREAM_URL is required")
		os.Exit(1)
	}

	upstream, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		logger.Error("invalid UPSTREAM_URL", "error", err)
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Host = upstream.Host
		r.Header.Del("X-Internal-Api-Key")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("proxy request failed", "service", cfg.ServiceName, "path", r.URL.Path, "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.InternalAPIKey == "" {
			http.Error(w, "INTERNAL_API_KEY is not configured", http.StatusServiceUnavailable)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Api-Key")), []byte(cfg.InternalAPIKey)) != 1 {
			http.Error(w, "invalid internal API key", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.RegisterOnStart {
		go runRegistration(ctx, cfg, logger)
	}

	go func() {
		logger.Info("model proxy listening", "service", cfg.ServiceName, "addr", cfg.ListenAddr, "upstream", cfg.UpstreamURL)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
	}
}

func configFromEnv() config {
	serviceName := env("SERVICE_NAME", "model-proxy")
	machineID := getMachineID()
	metadata := map[string]any{}
	if raw := env("METADATA", ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &metadata)
	}
	metadata["host"] = hostMetadata()
	return config{
		ListenAddr:           env("LISTEN_ADDR", ":9100"),
		UpstreamURL:          strings.TrimRight(env("UPSTREAM_URL", ""), "/"),
		UpstreamHealthPath:   env("UPSTREAM_HEALTH_PATH", "/health"),
		InternalAPIKey:       env("INTERNAL_API_KEY", ""),
		ServiceName:          serviceName,
		ServicePublicURL:     strings.TrimRight(env("SERVICE_PUBLIC_URL", "http://"+serviceName+":9100"), "/"),
		MinerAPIURL:          strings.TrimRight(env("MINER_API_URL", "http://miner-api:8001"), "/"),
		MinerTLSCAFile:       env("MINER_TLS_CA_FILE", ""),
		Capabilities:         splitList(env("CAPABILITIES", "")),
		Models:               splitList(env("MODELS", "")),
		Metadata:             metadata,
		RegisterOnStart:      envBool("REGISTER_ON_START", true),
		RegistrationInterval: time.Duration(envInt("REGISTRATION_INTERVAL_SECONDS", 1)) * time.Second,
		MachineID:            machineID,
	}
}

func getMachineID() string {
	if id, err := machineid.ProtectedID("recall-miner"); err == nil && id != "" {
		return id
	}
	if hostname, err := os.Hostname(); err == nil {
		return "hostname:" + hostname
	}
	return "unknown"
}

func runRegistration(ctx context.Context, cfg config, logger *slog.Logger) {
	if cfg.InternalAPIKey == "" || len(cfg.Capabilities) == 0 {
		logger.Warn("registration disabled: missing INTERNAL_API_KEY or CAPABILITIES")
		return
	}
	client, err := httpx.ClientWithRootCA(cfg.MinerTLSCAFile, 10*time.Second)
	if err != nil {
		logger.Error("invalid MINER_TLS_CA_FILE", "error", err)
		os.Exit(1)
	}
	for {
		if err := register(ctx, client, cfg); err != nil {
			logger.Warn("registration failed", "error", err)
		} else {
			if err := heartbeat(ctx, client, cfg); err != nil {
				logger.Warn("initial heartbeat failed", "error", err)
			}
			break
		}
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}

	ticker := time.NewTicker(cfg.RegistrationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := unregister(shutdownCtx, client, cfg); err != nil {
				logger.Warn("unregister failed", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := heartbeat(ctx, client, cfg); err != nil {
				logger.Warn("heartbeat failed; re-registering", "error", err)
				if err := register(ctx, client, cfg); err != nil {
					logger.Warn("re-registration failed", "error", err)
				}
			}
		}
	}
}

func register(ctx context.Context, client *http.Client, cfg config) error {
	payload := map[string]any{
		"name":         cfg.ServiceName,
		"url":          cfg.ServicePublicURL,
		"capabilities": cfg.Capabilities,
		"models":       cfg.Models,
		"metadata":     cfg.Metadata,
		"machine_id":   cfg.MachineID,
	}
	return post(ctx, client, cfg, "/internal/services/register", payload)
}

func heartbeat(ctx context.Context, client *http.Client, cfg config) error {
	status, code, errText := upstreamHealth(client, cfg)
	payload := map[string]any{
		"name":       cfg.ServiceName,
		"machine_id": cfg.MachineID,
		"health": map[string]any{
			"status":               status,
			"service":              "model-proxy",
			"upstream_url":         cfg.UpstreamURL,
			"upstream_health_path": cfg.UpstreamHealthPath,
			"upstream_status_code": code,
			"error":                errText,
			"metrics":              systemMetrics(),
			"checked_at":           time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	return post(ctx, client, cfg, "/internal/services/heartbeat", payload)
}

func unregister(ctx context.Context, client *http.Client, cfg config) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.MinerAPIURL+"/internal/services/"+cfg.ServiceName, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Internal-Api-Key", cfg.InternalAPIKey)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("unregister returned status %d", response.StatusCode)
	}
	return nil
}

func post(ctx context.Context, client *http.Client, cfg config, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.MinerAPIURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Internal-Api-Key", cfg.InternalAPIKey)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s returned status %d", path, response.StatusCode)
	}
	return nil
}

func upstreamHealth(client *http.Client, cfg config) (string, int, string) {
	resp, err := client.Get(cfg.UpstreamURL + cfg.UpstreamHealthPath)
	if err != nil {
		return "degraded", 0, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "ok", resp.StatusCode, ""
	}
	return "degraded", resp.StatusCode, resp.Status
}

func hostMetadata() map[string]any {
	metadata := map[string]any{
		"goos":        runtime.GOOS,
		"goarch":      runtime.GOARCH,
		"gpu_present": gpuPresent(),
		"memory_type": memoryType(),
	}
	if info, err := host.Info(); err == nil {
		metadata["hostname"] = info.Hostname
		metadata["platform"] = info.Platform
		metadata["platform_version"] = info.PlatformVersion
		metadata["kernel_version"] = info.KernelVersion
	}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		metadata["cpu_model"] = infos[0].ModelName
		metadata["cpu_vendor"] = infos[0].VendorID
		metadata["cpu_mhz"] = infos[0].Mhz
		metadata["cpu_physical_cores"] = infos[0].Cores
	}
	if count, err := cpu.Counts(true); err == nil {
		metadata["cpu_logical_cores"] = count
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		metadata["memory_total_bytes"] = vm.Total
	}
	return metadata
}

func systemMetrics() map[string]any {
	metrics := map[string]any{}
	if values, err := cpu.Percent(0, false); err == nil && len(values) > 0 {
		metrics["cpu_percent"] = values[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		metrics["memory"] = map[string]any{
			"total_bytes":     vm.Total,
			"available_bytes": vm.Available,
			"used_bytes":      vm.Used,
			"used_percent":    vm.UsedPercent,
		}
	}
	if usage, err := disk.Usage("/"); err == nil {
		metrics["disk"] = map[string]any{
			"path":         usage.Path,
			"total_bytes":  usage.Total,
			"free_bytes":   usage.Free,
			"used_bytes":   usage.Used,
			"used_percent": usage.UsedPercent,
		}
	}
	return metrics
}

func gpuPresent() bool {
	entries, err := os.ReadDir("/proc/driver/nvidia/gpus")
	return err == nil && len(entries) > 0
}

func memoryType() string {
	matches, err := filepath.Glob("/sys/devices/system/edac/mc/mc*/dimm*/dimm_mem_type")
	if err != nil || len(matches) == 0 {
		return "unknown"
	}
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if value != "" {
				return strings.ToUpper(value)
			}
		}
	}
	return "unknown"
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func healthcheck() error {
	url := env("HEALTHCHECK_URL", "http://127.0.0.1:9100/health")
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
