package handlers

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is the miner binary identity string emitted as miner_version.
// Override at link time with -X recall-miner/internal/handlers.Version=...
// or at runtime with MINER_VERSION / MINER_PUBLIC_VERSION.
var Version = "dev"

// processStartedAt is process uptime baseline (OBSERVED wall clock at init).
var processStartedAt = time.Now()

// IncProcessedRequests bumps the monotonic processed_requests counter.
// Called from validator-authenticated request paths (not heartbeats).
func (h *Handlers) IncProcessedRequests() {
	if h == nil {
		return
	}
	h.processedRequests.Add(1)
}

func (h *Handlers) processedRequestsCount() int64 {
	if h == nil {
		return 0
	}
	return int64(h.processedRequests.Load())
}

// buildInventoryStats returns allowlisted public inventory keys for heartbeat
// resource_stats. Safe strings only (no URL/IP/hostname). storage_gb is
// capacity-only and never derived from storage_used_gb. GPU is omitted when
// unknown — never fabricated.
func (h *Handlers) buildInventoryStats() map[string]any {
	stats := map[string]any{
		"os":                 runtime.GOOS,
		"uptime_seconds":     int64(time.Since(processStartedAt).Seconds()),
		"processed_requests": h.processedRequestsCount(),
		"miner_version":      inventoryMinerVersion(),
		"version":            inventoryMinerVersion(),
	}

	if region := inventoryRegion(); region != "" {
		stats["region"] = region
	}
	if cpu := inventoryCPUModel(); cpu != "" {
		stats["cpu"] = cpu
		stats["cpu_model"] = cpu
	}
	if ram := inventoryRAMGB(); ram > 0 {
		stats["ram_gb"] = ram
	}
	if capGB := inventoryStorageCapacityGB(); capGB > 0 {
		stats["storage_gb"] = capGB
	}
	if gpu := h.inventoryGPUModel(); gpu != "" {
		stats["gpu"] = gpu
		stats["gpu_model"] = gpu
	}
	return stats
}

func inventoryRegion() string {
	for _, key := range []string{"MINER_REGION", "MINER_PUBLIC_REGION"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if inventoryStringSafe(v) {
				return v
			}
			return ""
		}
	}
	// Safe default for test miners.
	return "us-east"
}

func inventoryMinerVersion() string {
	for _, key := range []string{"MINER_VERSION", "MINER_PUBLIC_VERSION"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if inventoryStringSafe(v) {
				return v
			}
			return "recall-miner/dev"
		}
	}
	v := strings.TrimSpace(Version)
	if v == "" {
		v = "dev"
	}
	if strings.Contains(v, "/") {
		if inventoryStringSafe(v) {
			return v
		}
		return "recall-miner/dev"
	}
	out := "recall-miner/" + v
	if inventoryStringSafe(out) {
		return out
	}
	return "recall-miner/dev"
}

func inventoryCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			model := strings.Join(strings.Fields(strings.TrimSpace(parts[1])), " ")
			if inventoryStringSafe(model) {
				return model
			}
			return ""
		}
	}
	return ""
}

func inventoryRAMGB() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || kb <= 0 {
			return 0
		}
		// Match API extract: bytes/1e9 style → report GB as float.
		return (kb * 1024) / 1e9
	}
	return 0
}

func inventoryStorageCapacityGB() float64 {
	for _, key := range []string{"MINER_STORAGE_CAPACITY_GB", "MINER_PUBLIC_STORAGE_GB"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if gb, err := strconv.ParseFloat(v, 64); err == nil && gb > 0 {
				return gb
			}
		}
	}
	if total, _, ok := inventoryDiskBytes("/"); ok && total > 0 {
		return float64(total) / 1e9
	}
	// Same capacity advertised on register (OBSERVED register body storage_gb=500).
	return 500
}

func inventoryDiskUsedGB() (float64, bool) {
	total, free, ok := inventoryDiskBytes("/")
	if !ok || total == 0 {
		return 0, false
	}
	used := total - free
	if used < 0 {
		used = 0
	}
	return float64(used) / 1e9, true
}

func inventoryDiskBytes(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return bs * st.Blocks, bs * st.Bavail, true
}

func (h *Handlers) inventoryGPUModel() string {
	if h != nil && h.registry != nil {
		for _, svc := range h.registry.List() {
			for _, key := range []string{"gpu_model", "gpu", "model_name", "device_name"} {
				if s, ok := svc.Metadata[key].(string); ok {
					s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
					if inventoryStringSafe(s) {
						return s
					}
				}
			}
		}
	}
	return inventoryNvidiaGPUModel()
}

func inventoryNvidiaGPUModel() string {
	matches, err := filepath.Glob("/proc/driver/nvidia/gpus/*/information")
	if err != nil || len(matches) == 0 {
		return ""
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, "model:") {
			continue
		}
		model := strings.TrimSpace(line[len("model:"):])
		// preserve original casing from after first colon
		if idx := strings.Index(line, ":"); idx >= 0 {
			model = strings.TrimSpace(line[idx+1:])
		}
		model = strings.Join(strings.Fields(model), " ")
		if inventoryStringSafe(model) {
			return model
		}
		return ""
	}
	return ""
}

// inventoryStringSafe mirrors recall-web isSafePublicInventoryString enough to
// avoid emitting values the API sanitize would null (URL/IP/hostname).
func inventoryStringSafe(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if strings.Contains(value, "://") {
		return false
	}
	for _, scheme := range []string{"http:", "https:", "ws:", "wss:", "ftp:", "file:"} {
		if strings.HasPrefix(lower, scheme) {
			return false
		}
	}
	// crude IPv4
	if looksLikeIPv4(value) {
		return false
	}
	if strings.Contains(value, ":") && looksLikeHostPort(value) {
		return false
	}
	for _, suf := range []string{".local", ".internal", ".lan", ".corp", ".home", ".localhost"} {
		if strings.HasSuffix(lower, suf) {
			return false
		}
	}
	// DNS-like with alphabetic TLD (reject); allow semver 1.2.3
	if inventoryDNSLike(value) {
		return false
	}
	return true
}

func looksLikeIPv4(value string) bool {
	for _, tok := range splitNonAlnum(value) {
		if isDottedQuad(tok) {
			return true
		}
	}
	return isDottedQuad(value)
}

func splitNonAlnum(value string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func isDottedQuad(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func looksLikeHostPort(value string) bool {
	// host:port with numeric port
	idx := strings.LastIndex(value, ":")
	if idx <= 0 || idx == len(value)-1 {
		return false
	}
	port := value[idx+1:]
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

func inventoryDNSLike(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(lower, ".")
	if len(parts) < 3 {
		return false
	}
	tld := parts[len(parts)-1]
	if len(tld) < 2 {
		return false
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			return false // digit TLD → treat as version-like, allow
		}
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

