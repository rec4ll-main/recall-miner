package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runtime image must trust public CAs for outbound HTTPS and must own the
// certificate mountpoint so a fresh miner-certs volume is writable by UID 1000.
func TestRuntimeImageHasCABundleAndOwnedCertMountpoint(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	stages := strings.Split(string(raw), "\nFROM ")
	runtime := stages[len(stages)-1]
	for _, want := range []string{
		"apt-get install -y --no-install-recommends ca-certificates",
		"--uid 1000",
		"mkdir -p /var/lib/recall-miner/certs",
		"chown -R miner:miner /var/lib/recall-miner",
		"USER miner",
	} {
		if !strings.Contains(runtime, want) {
			t.Fatalf("runtime stage is missing %q", want)
		}
	}
	if strings.Index(runtime, "USER miner") < strings.Index(runtime, "chown -R miner:miner") {
		t.Fatal("ownership must be set before switching to the runtime user")
	}
}
