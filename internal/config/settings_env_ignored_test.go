package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// fromEnvLogs runs FromEnv and returns the config and what it logged.
func fromEnvLogs(t *testing.T) (*Config, string) {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c, buf.String()
}

// Distinct non-default values for every console setting's former variable.
var ignoredEnvValues = map[string]string{
	"sessionDurationDefault": "4321", "sessionDurationMax": "54321", "sessionIdleGrace": "1234",
	"forwardIdle": "432", "logLevel": "debug", "sessionFailover": "false", "providerCooldown": "4444",
	"providerExclude": "m1=0x" + strings.Repeat("c", 40), "sessionRelease": "false",
	"auditRetentionDays": "3210", "housekeeping": "false",
}

// Console settings are not read from the environment: every former
// variable is ignored (the default applies), one warning names them all,
// and no value is logged.
func TestConsoleSettingEnvIgnoredWithOneWarning(t *testing.T) {
	clearSettingEnv(t)
	var names []string
	for _, d := range Settings {
		if !d.Settable() {
			continue
		}
		v, ok := ignoredEnvValues[d.Name]
		if !ok {
			t.Fatalf("no test value for %s", d.Name)
		}
		t.Setenv(d.Env, v)
		names = append(names, d.Env)
	}
	c, logs := fromEnvLogs(t)
	for _, d := range Settings {
		if d.Settable() && d.Effective(c) != d.Default {
			t.Errorf("%s: env applied (%q, default %q)", d.Name, d.Effective(c), d.Default)
		}
	}
	if n := strings.Count(logs, "now managed in the console Settings"); n != 1 {
		t.Fatalf("want one warning, got %d: %q", n, logs)
	}
	for _, e := range names {
		if !strings.Contains(logs, e) {
			t.Errorf("warning does not name %s: %q", e, logs)
		}
	}
	for _, v := range []string{"4321", "54321", "1234", "4444", "3210", strings.Repeat("c", 40)} {
		if strings.Contains(logs, v) {
			t.Errorf("warning carries a value (%s): %q", v, logs)
		}
	}
	if strings.Join(c.IgnoredEnv, ",") != strings.Join(names, ",") {
		t.Errorf("IgnoredEnv %v, want %v", c.IgnoredEnv, names)
	}
}

// One variable: one warning naming it. None set (or blank): no warning.
func TestConsoleSettingEnvWarningOnlyWhenSet(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("SESSION_FAILOVER", "false")
	c, logs := fromEnvLogs(t)
	if !c.SessionFailover || strings.Count(logs, "now managed in the console Settings") != 1 || !strings.Contains(logs, "SESSION_FAILOVER") {
		t.Fatalf("failover %v, logs %q", c.SessionFailover, logs)
	}
	clearSettingEnv(t)
	t.Setenv("HOUSEKEEPING", "  ")
	c, logs = fromEnvLogs(t)
	if strings.Contains(logs, "console Settings") || len(c.IgnoredEnv) != 0 {
		t.Fatalf("warned with nothing set: %q", logs)
	}
}

// Saved > default: a saved restart value applies even when its former
// variable is set to something else; without a saved value the default
// applies.
func TestConsoleSettingSavedOverDefault(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("AUDIT_RETENTION_DAYS", "30")
	t.Setenv("SESSION_FAILOVER", "false")
	c, _ := fromEnvLogs(t)
	ApplyPersisted(c, map[string]string{"auditRetentionDays": "120"})
	if c.AuditRetentionDays != 120 {
		t.Errorf("saved value not applied: %d", c.AuditRetentionDays)
	}
	if !c.SessionFailover {
		t.Error("env applied where nothing was saved (want the default true)")
	}
}

// Env-only settings are still read from the environment.
func TestEnvOnlySettingsStillFromEnv(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("ACTIVE_MODELS_URL", "https://feed.example/m.json")
	c, logs := fromEnvLogs(t)
	if c.ActiveModelsURL != "https://feed.example/m.json" || !c.EnvOnlySet["ACTIVE_MODELS_URL"] {
		t.Fatalf("env-only not read: %q %v", c.ActiveModelsURL, c.EnvOnlySet)
	}
	if strings.Contains(logs, "console Settings") {
		t.Fatalf("env-only variable warned: %q", logs)
	}
}
