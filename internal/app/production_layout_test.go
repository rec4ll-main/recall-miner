package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionSourceHasNoDebugComposeOverride(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "docker-compose.vllm-debug.yml")); !os.IsNotExist(err) {
		t.Fatalf("debug Compose override must not ship in production source: %v", err)
	}
	for _, path := range []string{"README.md", "SETUP.md", ".env.example", "docker-compose.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range []string{"docker-compose.vllm-debug.yml", "VLLM_PUBLISH_HOST_PORT"} {
			if strings.Contains(string(raw), retired) {
				t.Fatalf("%s still references retired %s", path, retired)
			}
		}
	}
}

func TestProductionEnvironmentExampleHasEmptyRequiredSecrets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			settings[key] = value
		}
	}
	for _, key := range []string{"MINER_HOTKEY_URI", "MINER_INTERNAL_API_KEY"} {
		if value, present := settings[key]; !present || value != "" {
			t.Fatalf("%s must be present and empty in the public template", key)
		}
	}
	for key, want := range map[string]string{
		"ENVIRONMENT":           "production",
		"RECALL_API_URL":        "https://rec4ll.com/api",
		"TLS_ENABLED":           "true",
		"GENERATION_ENABLED":    "false",
		"SIDECAR_MINER_API_URL": "https://miner:8001",
		"MINER_TLS_CA_FILE":     "/var/lib/recall-miner/certs/miner-cert.pem",
	} {
		if got := settings[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	// Mainnet public identities are preset, not left for the operator to request.
	if got, want := settings["API_SIGNING_HOTKEY_SS58"], "5ECDAred992gLh55nJ7PxQDtihHXy5qZZXuCkqLVJdmDsrYw"; got != want {
		t.Fatalf("API_SIGNING_HOTKEY_SS58 = %q, want mainnet API signing key %q", got, want)
	}
	if _, present := settings["VALIDATOR_HOTKEY_SS58"]; present {
		t.Fatal("VALIDATOR_HOTKEY_SS58 was removed and fails miner startup; it must not appear in .env.example")
	}
}
