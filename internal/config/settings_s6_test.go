package config

import (
	"strings"
	"testing"
)

// S6: UPLINK_LOG_LEVEL parsing (bad value → info with a warning that does
// not carry the value).
func TestS6LogLevelEnv(t *testing.T) {
	for v, want := range map[string]string{"": "info", "debug": "debug", "WARNING": "warn", "error": "error", "loud": "info"} {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "admin:secret")
		t.Setenv("UPLINK_LOG_LEVEL", v)
		c, logs := fromEnvQuiet(t)
		if c.LogLevel != want {
			t.Errorf("%q: got %s want %s", v, c.LogLevel, want)
		}
		if v == "loud" && (!strings.Contains(logs, "UPLINK_LOG_LEVEL") || strings.Contains(logs, "loud")) {
			t.Errorf("warning: %q", logs)
		}
		if c.EnvPinned["UPLINK_LOG_LEVEL"] != (v != "" && v != "loud") {
			t.Errorf("%q: pinned %v", v, c.EnvPinned["UPLINK_LOG_LEVEL"])
		}
	}
}

// S6: an env var pins its setting only when it holds a valid value.
func TestS6EnvPinned(t *testing.T) {
	cases := []struct {
		env, val string
		pinned   bool
	}{
		{"SESSION_DURATION_SECONDS", "900", true},
		{"SESSION_DURATION_SECONDS", "x", false},
		{"UPLINK_SESSION_DURATION_MAX", "3600", true},
		{"UPLINK_SESSION_DURATION_MAX", "99", false},
		{"UPLINK_SESSION_IDLE_GRACE_SECONDS", "120", true},
		{"UPLINK_SESSION_IDLE_GRACE_SECONDS", "5", false},
		{"UPLINK_FORWARD_IDLE_SECONDS", "60", true},
		{"UPLINK_FORWARD_IDLE_SECONDS", "601", false},
		{"UPLINK_FORWARD_IDLE_SECONDS", "", false},
	}
	for _, c := range cases {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "admin:secret")
		t.Setenv(c.env, c.val)
		cfg, _ := fromEnvQuiet(t)
		if cfg.EnvPinned[c.env] != c.pinned {
			t.Errorf("%s=%q: pinned %v", c.env, c.val, cfg.EnvPinned[c.env])
		}
	}
}
