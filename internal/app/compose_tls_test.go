package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMinerTLSHostsMergesExtraSANs(t *testing.T) {
	got := minerTLSHosts("https://miner.example.com:7101", []string{"miner", "miner.example.com:7101", " miner "})
	if len(got) != 2 {
		t.Fatalf("hosts = %#v", got)
	}
	if got[0] != "miner.example.com:7101" || got[1] != "miner" {
		t.Fatalf("hosts = %#v", got)
	}
}

func TestComposeForwardsNativeTLSSettings(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	required := []string{
		"TLS_ENABLED=",
		"TLS_CERT_FILE=",
		"TLS_KEY_FILE=",
		"TLS_AUTO_GENERATE=",
		"TLS_SAN_HOSTS=",
		"MINER_TLS_CA_FILE:",
		"SIDECAR_MINER_API_URL",
		"miner-certs:",
		"NETUID=",
		"MINER_HOTKEY_URI",
		"HOTKEY_URI",
	}
	for _, needle := range required {
		if !strings.Contains(body, needle) {
			t.Fatalf("docker-compose.yml missing %q", needle)
		}
	}
	setup, err := os.ReadFile(filepath.Join(root, "SETUP.md"))
	if err != nil {
		t.Fatal(err)
	}
	setupBody := string(setup)
	if !strings.Contains(setupBody, "MINER_HOTKEY_URI=<miner-hotkey-secret>") {
		t.Fatal("SETUP mainnet example must document MINER_HOTKEY_URI")
	}
	if strings.Contains(setupBody, "\nHOTKEY_URI=<miner-hotkey-secret>") {
		t.Fatal("SETUP mainnet example still assigns HOTKEY_URI alone")
	}
	if !strings.Contains(setupBody, "Native TLS with Compose") {
		t.Fatal("SETUP missing native TLS compose section")
	}
}
