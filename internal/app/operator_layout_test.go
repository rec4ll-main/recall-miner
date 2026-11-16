package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The supported Compose overrides keep advertised capabilities aligned with
// the backends each profile runs.
func TestSupportedComposeOverrides(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	cpu := read("deploy/compose/cpu.yml")
	for _, want := range []string{
		"vllm:\n    profiles:",
		"model-proxy:\n    profiles:",
		"CAPABILITIES: embedding,rerank\n",
	} {
		if !strings.Contains(cpu, want) {
			t.Fatalf("cpu.yml missing %q", want)
		}
	}
	gpu := read("deploy/compose/gpu.yml")
	for _, want := range []string{
		"image: ${VLLM_IMAGE:?",
		"CAPABILITIES: rerank\n",
	} {
		if !strings.Contains(gpu, want) {
			t.Fatalf("gpu.yml missing %q", want)
		}
	}
	for name, body := range map[string]string{"cpu.yml": cpu, "gpu.yml": gpu} {
		for _, forbidden := range []string{"generate", "ports:", "GENERATION_ENABLED", "ENVIRONMENT", "VLLM_PUBLISH_HOST_PORT"} {
			if regexp.MustCompile(`(?m)^[^#]*` + regexp.QuoteMeta(forbidden)).MatchString(body) {
				t.Fatalf("%s must not set %q outside comments", name, forbidden)
			}
		}
	}
}

// The operator guides reference files and anchors that exist.
func TestOperatorGuideLinksResolve(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"README.md", "SETUP.md", "docs/ENVIRONMENT.md", "docs/FRESH_START.md", "docs/GENERATION_CHUTES.md", "setup-agents.md"} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`\]\(([^)\s]+)\)`).FindAllStringSubmatch(string(raw), -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			dest := filepath.Join(root, rel)
			if file != "" {
				dest = filepath.Join(filepath.Dir(filepath.Join(root, rel)), file)
			}
			body, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("%s: link target %s missing", rel, target)
			}
			if anchor != "" && !hasAnchor(string(body), anchor) {
				t.Fatalf("%s: anchor %s missing", rel, target)
			}
		}
	}
}

func hasAnchor(markdown, anchor string) bool {
	clean := regexp.MustCompile(`[^\w\- ]`)
	for _, m := range regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(markdown, -1) {
		slug := strings.ReplaceAll(clean.ReplaceAllString(strings.ToLower(strings.TrimSpace(m[1])), ""), " ", "-")
		if slug == anchor {
			return true
		}
	}
	return false
}
