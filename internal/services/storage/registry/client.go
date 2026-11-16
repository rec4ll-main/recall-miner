package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"

	"recall-miner/internal/httpx"
	"recall-miner/internal/services/storage/config"
)

type Client struct {
	cfg  config.Config
	log  *slog.Logger
	http *http.Client
}

type registerPayload struct {
	Name         string         `json:"name"`
	URL          string         `json:"url"`
	Capabilities []string       `json:"capabilities"`
	Models       []string       `json:"models,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	MachineID    string         `json:"machine_id,omitempty"`
}

func NewClient(cfg config.Config, logger *slog.Logger) *Client {
	client, err := httpx.ClientWithRootCA(cfg.MinerTLSCAFile, 10*time.Second)
	if err != nil {
		logger.Error("invalid MINER_TLS_CA_FILE", "error", err)
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		cfg:  cfg,
		log:  logger,
		http: client,
	}
}

func (c *Client) Run(ctx context.Context) {
	if c.cfg.InternalAPIKey == "" {
		c.log.Warn("registration disabled: INTERNAL_API_KEY is empty")
		return
	}

	for {
		if err := c.Register(ctx); err != nil {
			c.log.Warn("registration failed", "error", err)
		} else {
			if err := c.Heartbeat(ctx); err != nil {
				c.log.Warn("initial heartbeat failed", "error", err)
			}
			break
		}

		if !sleep(ctx, 5*time.Second) {
			return
		}
	}

	ticker := time.NewTicker(c.cfg.RegistrationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Heartbeat(ctx); err != nil {
				c.log.Warn("heartbeat failed; re-registering", "error", err)
				if err := c.Register(ctx); err != nil {
					c.log.Warn("re-registration failed", "error", err)
				}
			}
		}
	}
}

func (c *Client) Register(ctx context.Context) error {
	machineID := c.machineID()
	payload := registerPayload{
		Name:         c.cfg.ServiceName,
		URL:          c.cfg.PublicURL,
		Capabilities: []string{"db"},
		MachineID:    machineID,
		Metadata: map[string]any{
			"engine":      c.cfg.DBEngine,
			"api":         "recall-db-v1",
			"wrapper":     "go",
			"backend_url": c.cfg.BackendEndpoint(),
			"implemented": true,
			"host":        hostMetadata(),
		},
	}
	if err := c.post(ctx, "/internal/services/register", payload); err != nil {
		return err
	}
	c.log.Info("registered db service", "name", c.cfg.ServiceName, "url", c.cfg.PublicURL, "engine", c.cfg.DBEngine)
	return nil
}

func (c *Client) Heartbeat(ctx context.Context) error {
	payload := map[string]any{
		"name":       c.cfg.ServiceName,
		"machine_id": c.machineID(),
		"health": map[string]any{
			"status":      "ok",
			"service":     "db-service",
			"engine":      c.cfg.DBEngine,
			"backend_url": c.cfg.BackendEndpoint(),
			"metrics":     systemMetrics(),
			"checked_at":  time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	return c.post(ctx, "/internal/services/heartbeat", payload)
}

func (c *Client) machineID() string {
	id, err := machineid.ProtectedID("recall-miner")
	if err == nil && id != "" {
		return id
	}
	c.log.Warn("machine id unavailable", "error", err)
	if hostname, hostErr := os.Hostname(); hostErr == nil {
		return "hostname:" + hostname
	}
	return "unknown"
}

func (c *Client) Unregister(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.cfg.MinerAPIURL+"/internal/services/"+c.cfg.ServiceName, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Internal-Api-Key", c.cfg.InternalAPIKey)

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		return fmt.Errorf("unregister returned status %d", response.StatusCode)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MinerAPIURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Internal-Api-Key", c.cfg.InternalAPIKey)

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		return fmt.Errorf("%s returned status %d", path, response.StatusCode)
	}
	return nil
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
