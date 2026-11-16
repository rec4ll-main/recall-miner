package app

import (
	"os"
	"os/exec"
	"testing"

	"recall-miner/internal/config"
)

func TestComposeRecallAPIURL(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	baseEnv := []string{
		"MINER_INTERNAL_API_KEY=test-internal-key-for-compose-render",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DOCKER_HOST=" + os.Getenv("DOCKER_HOST"),
		"XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR"),
	}
	tests := []struct {
		name, value, want string
	}{
		{"default", "", "https://rec4ll.com/api"},
		{"set", "https://api.example.com/api", "https://api.example.com/api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := append(append([]string{}, baseEnv...), "RECALL_API_URL="+tt.value)
			envMap := minerEnvFromComposeConfig(t, repoRoot(t), env)
			value, ok := envMap["RECALL_API_URL"].(string)
			if !ok {
				t.Fatalf("compose omitted RECALL_API_URL: %#v", envMap["RECALL_API_URL"])
			}
			t.Setenv("RECALL_API_URL", value)
			if got := config.FromEnv().RecallAPIURL; got != tt.want {
				t.Fatalf("effective Recall API base = %q, want %q", got, tt.want)
			}
		})
	}
}
