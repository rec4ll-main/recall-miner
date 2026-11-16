package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestComposeApplicationProberPassthrough(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	root := repoRoot(t)
	composeFile := filepath.Join(root, "docker-compose.yml")

	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "API_SIGNING_HOTKEY_SS58: ${API_SIGNING_HOTKEY_SS58:-}") {
		t.Fatal("compose must not inject API_SIGNING_HOTKEY_SS58 with an empty default")
	}
	if !strings.Contains(body, "- API_SIGNING_HOTKEY_SS58\n") && !strings.Contains(body, "- API_SIGNING_HOTKEY_SS58\r\n") {
		t.Fatal("compose must pass API_SIGNING_HOTKEY_SS58 through in list form when set")
	}

	baseEnv := []string{
		"MINER_INTERNAL_API_KEY=test-internal-key-for-compose-render",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DOCKER_HOST=" + os.Getenv("DOCKER_HOST"),
		"XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR"),
	}

	t.Run("absent_on_host", func(t *testing.T) {
		envMap := minerEnvFromComposeConfig(t, root, baseEnv)
		val, present := envMap["API_SIGNING_HOTKEY_SS58"]
		if present && val != nil {
			t.Fatalf("expected API_SIGNING_HOTKEY_SS58 absent or null when unset, got %#v", val)
		}
	})

	t.Run("set_on_host", func(t *testing.T) {
		env := append(append([]string{}, baseEnv...), "API_SIGNING_HOTKEY_SS58=5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
		envMap := minerEnvFromComposeConfig(t, root, env)
		val, present := envMap["API_SIGNING_HOTKEY_SS58"]
		if !present {
			t.Fatal("expected API_SIGNING_HOTKEY_SS58 present when set on host")
		}
		got, _ := val.(string)
		if got != "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY" {
			t.Fatalf("forwarded value = %#v", val)
		}
	})

	t.Run("empty_on_host", func(t *testing.T) {
		env := append(append([]string{}, baseEnv...), "API_SIGNING_HOTKEY_SS58=")
		envMap := minerEnvFromComposeConfig(t, root, env)
		val, present := envMap["API_SIGNING_HOTKEY_SS58"]
		if !present {
			t.Fatal("expected explicit empty API_SIGNING_HOTKEY_SS58 to be forwarded")
		}
		got, _ := val.(string)
		if got != "" {
			t.Fatalf("expected empty string, got %#v", val)
		}
	})

	t.Run("dotenv_file_absent_line", func(t *testing.T) {
		dir := t.TempDir()
		envFile := filepath.Join(dir, ".env")
		contents := "MINER_INTERNAL_API_KEY=test-internal-key-for-compose-render\n"
		if err := os.WriteFile(envFile, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("docker", "compose", "--env-file", envFile, "-f", composeFile, "config", "--format", "json")
		cmd.Dir = root
		cmd.Env = baseEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compose config with dotenv: %v\n%s", err, out)
		}
		envMap := minerEnvFromConfigJSON(t, out)
		val, present := envMap["API_SIGNING_HOTKEY_SS58"]
		if present && val != nil {
			t.Fatalf("dotenv without application key must preserve absence, got %#v", val)
		}
	})
}

func minerEnvFromComposeConfig(t *testing.T, root string, env []string) map[string]any {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", "docker-compose.yml", "config", "--format", "json")
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}
	return minerEnvFromConfigJSON(t, out)
}

func minerEnvFromConfigJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var cfg struct {
		Services map[string]struct {
			Environment map[string]any `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse compose json: %v\n%s", err, raw)
	}
	miner, ok := cfg.Services["miner"]
	if !ok {
		t.Fatal("miner service missing from compose config")
	}
	if len(miner.Environment) == 0 {
		t.Fatal("miner environment empty in compose config")
	}
	return miner.Environment
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

// VALIDATOR_HOTKEY_SS58 was removed and fails startup when present, so no
// compose file may forward it (even with an empty default).
func TestComposeDoesNotForwardRemovedValidatorHotkey(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"docker-compose.yml", "deploy/compose/cpu.yml", "deploy/compose/gpu.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, "VALIDATOR_HOTKEY_SS58") {
				t.Fatalf("%s forwards removed VALIDATOR_HOTKEY_SS58: %s", name, trimmed)
			}
		}
	}
}
