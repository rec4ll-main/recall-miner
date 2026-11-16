package config

import (
	"os"
	"strings"
	"testing"
)

const apiSigningKey = "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"

func setProberEnv(t *testing.T, vals map[string]string) {
	t.Helper()
	for _, k := range []string{"API_SIGNING_HOTKEY_SS58", "APPLICATION_PROBER_HOTKEY_SS58"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
		if v, ok := vals[k]; ok {
			t.Setenv(k, v)
		}
	}
}

func TestAPISigningHotkey(t *testing.T) {
	setProberEnv(t, map[string]string{"API_SIGNING_HOTKEY_SS58": apiSigningKey})
	c := FromEnv()
	if c.ApplicationIdentity() != apiSigningKey {
		t.Fatalf("identity %q", c.ApplicationIdentity())
	}
	if err := ValidateApplicationProberHotkey(c); err != nil {
		t.Fatal(err)
	}
}

func TestRenamedProberHotkeyFailsStartup(t *testing.T) {
	for _, vals := range []map[string]string{
		{"APPLICATION_PROBER_HOTKEY_SS58": apiSigningKey},
		{"APPLICATION_PROBER_HOTKEY_SS58": apiSigningKey, "API_SIGNING_HOTKEY_SS58": apiSigningKey},
	} {
		setProberEnv(t, vals)
		c := FromEnv()
		if c.ApplicationProberHotkey == apiSigningKey && len(vals) == 1 {
			t.Fatal("old name was read")
		}
		if err := ValidateApplicationProberHotkey(c); err == nil || !strings.Contains(err.Error(), "renamed to API_SIGNING_HOTKEY_SS58") {
			t.Fatalf("old name accepted: %v", err)
		}
	}
}

func TestEmptyAPISigningHotkeyFails(t *testing.T) {
	setProberEnv(t, map[string]string{"API_SIGNING_HOTKEY_SS58": ""})
	if err := ValidateApplicationProberHotkey(FromEnv()); err == nil || !strings.Contains(err.Error(), "API_SIGNING_HOTKEY_SS58 is set but empty") {
		t.Fatalf("err=%v", err)
	}
}
