package config

import "testing"

func TestFromEnvRecallAPIURL(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"default", "", "https://rec4ll.com/api"},
		{"set", "https://api.example.com/api", "https://api.example.com/api"},
		{"trailing_slashes", "https://api.example.com/api///", "https://api.example.com/api"},
		{"base_without_api", "https://api.example.com", "https://api.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RECALL_API_URL", tt.value)
			if got := FromEnv().RecallAPIURL; got != tt.want {
				t.Fatalf("Recall API base = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFromEnvEnvironmentIsALabel(t *testing.T) {
	for _, value := range []string{"", "dev", "test", "production", "prod", "mainnet", "staging", "custom-label"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ENVIRONMENT", value)
			if got := FromEnv().Environment; got != value {
				t.Fatalf("Environment = %q, want %q", got, value)
			}
		})
	}
	t.Setenv("ENVIRONMENT", "PRODUCTION")
	if got := FromEnv().Environment; got != "production" {
		t.Fatalf("Environment = %q, want production", got)
	}
}
