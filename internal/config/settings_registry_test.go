package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func clearSettingEnv(t *testing.T) {
	t.Helper()
	for _, d := range Settings {
		t.Setenv(d.Env, "")
	}
	for _, k := range []string{"UPLINK_DEV_HTTP", "COOKIE_CONTENT", "DATA_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("ADMIN_PASSWORD", "registry-test-pass")
	t.Setenv("API_KEY_SEED", "registry-seed")
	t.Setenv("ROUTER_AUTH", "u:p")
}

func quietFromEnv(t *testing.T) *Config {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Registry: every row's name, env, type, default, range and scope.
func TestSettingsRegistryTable(t *testing.T) {
	want := []struct {
		name, env string
		kind      SettingKind
		def       string
		min, max  int
		scope     SettingScope
		group     string
	}{
		{"sessionDurationDefault", "SESSION_DURATION_SECONDS", KindInt, "600", 600, 86400, ScopeLive, ""},
		{"sessionDurationMax", "UPLINK_SESSION_DURATION_MAX", KindInt, "1800", 600, 86400, ScopeLive, ""},
		{"sessionIdleGrace", "UPLINK_SESSION_IDLE_GRACE_SECONDS", KindInt, "300", 60, 3600, ScopeLive, ""},
		{"forwardIdle", "UPLINK_FORWARD_IDLE_SECONDS", KindInt, "180", 30, 600, ScopeLive, ""},
		{"logLevel", "UPLINK_LOG_LEVEL", KindEnum, "info", 0, 0, ScopeLive, ""},
		{"sessionFailover", "SESSION_FAILOVER", KindBool, "true", 0, 0, ScopeRestart, ""},
		{"providerCooldown", "UPLINK_PROVIDER_COOLDOWN_SECONDS", KindInt, "600", 0, 86400, ScopeRestart, ""},
		{"providerExclude", "UPLINK_PROVIDER_EXCLUDE", KindString, "", 0, 0, ScopeRestart, ""},
		{"inflightMax", "UPLINK_INFLIGHT_MAX", KindInt, "32", 1, 1024, ScopeRestart, "inflight"},
		{"longPerKeyModel", "UPLINK_LONG_PER_KEY_MODEL", KindInt, "1", 0, 16, ScopeRestart, "long"},
		{"longPerKey", "UPLINK_LONG_PER_KEY", KindInt, "2", 0, 64, ScopeRestart, "long"},
		{"longSessionsMax", "UPLINK_LONG_SESSIONS_MAX", KindInt, "3", 0, 256, ScopeRestart, "long"},
		{"opensPerKeyModelHour", "UPLINK_OPENS_PER_KEY_MODEL_HOUR", KindInt, "2", 0, 120, ScopeRestart, "long"},
		{"auditRetentionDays", "AUDIT_RETENTION_DAYS", KindInt, "90", 1, 3650, ScopeRestart, ""},
		{"housekeeping", "HOUSEKEEPING", KindBool, "true", 0, 0, ScopeRestart, ""},
		{"activeModelsUrl", "ACTIVE_MODELS_URL", KindString, "https://active.mor.org/gateway_models.json", 0, 0, ScopeRestart, ""},
		{"gatewayBidsUrl", "GATEWAY_BIDS_URL", KindString, "https://active.mor.org/gateway_bids.json", 0, 0, ScopeRestart, ""},
		{"webPublicUrl", "WEB_PUBLIC_URL", KindString, "", 0, 0, ScopeRestart, ""},
		{"masterKeyAdmin", "MASTER_KEY_ADMIN", KindBool, "false", 0, 0, ScopeEnvOnly, ""},
		{"proxyStoreChatContext", "PROXY_STORE_CHAT_CONTEXT", KindBool, "false", 0, 0, ScopeRouter, ""},
		{"proxyForwardChatContext", "PROXY_FORWARD_CHAT_CONTEXT", KindBool, "false", 0, 0, ScopeRouter, ""},
		{"routerLogLevelApp", "LOG_LEVEL_APP", KindEnum, "warn", 0, 0, ScopeRouter, ""},
		{"routerLogLevelTcp", "LOG_LEVEL_TCP", KindEnum, "warn", 0, 0, ScopeRouter, ""},
		{"routerLogLevelEthRpc", "LOG_LEVEL_ETH_RPC", KindEnum, "warn", 0, 0, ScopeRouter, ""},
	}
	if len(Settings) != len(want) {
		t.Fatalf("registry has %d rows, want %d", len(Settings), len(want))
	}
	for i, w := range want {
		d := Settings[i]
		if d.Name != w.name || d.Env != w.env || d.Kind != w.kind || d.Default != w.def || d.Min != w.min || d.Max != w.max || d.Scope != w.scope || d.Group != w.group {
			t.Errorf("row %d: %+v, want %+v", i, d, w)
		}
	}
}

// Registry: shape rules, restart flags in the tooltips, range checks.
func TestSettingsRegistryRules(t *testing.T) {
	names, envs := map[string]bool{}, map[string]bool{}
	for _, d := range Settings {
		if names[d.Name] || envs[d.Env] {
			t.Fatalf("duplicate %s/%s", d.Name, d.Env)
		}
		names[d.Name], envs[d.Env] = true, true
		if d.Help == "" {
			t.Errorf("%s: no tooltip", d.Name)
		}
		if _, err := d.Validate(d.Default); err != nil && d.Scope != ScopeRouter {
			t.Errorf("%s: default %q invalid: %v", d.Name, d.Default, err)
		}
		switch d.Scope {
		case ScopeLive:
			if strings.Contains(d.Help, "restart") || d.set != nil || !d.Settable() {
				t.Errorf("%s: live row says restart or has a boot setter", d.Name)
			}
		case ScopeRestart:
			if !strings.Contains(d.Help, "after an Uplink restart") || d.set == nil || !d.Settable() {
				t.Errorf("%s: restart row lacks the restart note or setter", d.Name)
			}
		case ScopeEnvOnly:
			if !strings.Contains(d.Help, ".env only") || d.set != nil || d.Settable() {
				t.Errorf("%s: env-only row is settable", d.Name)
			}
		case ScopeRouter:
			if !strings.Contains(d.Help, "restart the router") || d.set != nil || d.get != nil || d.Settable() {
				t.Errorf("%s: router row is settable or lacks the router note", d.Name)
			}
		}
		switch d.Kind {
		case KindInt:
			for _, ok := range []int{d.Min, d.Max} {
				if _, err := d.Validate(itoa(ok)); err != nil {
					t.Errorf("%s: %d refused", d.Name, ok)
				}
			}
			for _, bad := range []string{itoa(d.Min - 1), itoa(d.Max + 1), "1.5", "ten", ""} {
				if _, err := d.Validate(bad); err == nil {
					t.Errorf("%s: %q accepted", d.Name, bad)
				}
			}
		case KindBool:
			if _, err := d.Validate("maybe"); err == nil {
				t.Errorf("%s: maybe accepted", d.Name)
			}
		case KindEnum:
			if _, err := d.Validate("loud"); err == nil {
				t.Errorf("%s: loud accepted", d.Name)
			}
		}
	}
}

// Registry defaults are the values FromEnv uses with nothing set.
func TestSettingsRegistryDefaultsMatchConfig(t *testing.T) {
	clearSettingEnv(t)
	c := quietFromEnv(t)
	for _, d := range Settings {
		if d.Scope == ScopeRouter {
			continue
		}
		if got := d.Effective(c); got != d.Default {
			t.Errorf("%s: config default %q, registry %q", d.Name, got, d.Default)
		}
		if c.EnvPinned[d.Env] {
			t.Errorf("%s pinned with no env", d.Name)
		}
	}
}

// Secrets are never registry rows and are recognised in every spelling.
func TestSettingsRegistryHasNoSecrets(t *testing.T) {
	for _, d := range Settings {
		if IsSecretKey(d.Name) || IsSecretKey(d.Env) {
			t.Errorf("%s/%s is a secret", d.Name, d.Env)
		}
	}
	for _, k := range append(append([]string{}, SecretEnv...), "walletPrivateKey", "ethNodeAddress", "cookieContent", "adminPassword", "apiKeySeed", "masterKey", "routerAuth", "admin_password", "Wallet_Private_Key") {
		if !IsSecretKey(k) {
			t.Errorf("%s not treated as a secret", k)
		}
	}
}

// Env wins: a set env value pins its row (strings whenever set, others
// when FromEnv accepts the value; the cool-down clamp still pins).
func TestSettingsEnvPins(t *testing.T) {
	cases := []struct {
		env, val string
		pinned   bool
	}{
		{"UPLINK_INFLIGHT_MAX", "64", true},
		{"UPLINK_INFLIGHT_MAX", "abc", false},
		{"UPLINK_INFLIGHT_MAX", "0", false},
		{"UPLINK_PROVIDER_COOLDOWN_SECONDS", "999999", true},
		{"SESSION_FAILOVER", "false", true},
		{"SESSION_FAILOVER", "maybe", false},
		{"UPLINK_PROVIDER_EXCLUDE", "junk", true},
		{"MASTER_KEY_ADMIN", "true", true},
		{"UPLINK_LOG_LEVEL", "warning", true},
		{"UPLINK_LONG_PER_KEY", "65", false},
	}
	for _, c := range cases {
		clearSettingEnv(t)
		t.Setenv(c.env, c.val)
		if got := quietFromEnv(t).EnvPinned[c.env]; got != c.pinned {
			t.Errorf("%s=%s pinned %v, want %v", c.env, c.val, got, c.pinned)
		}
	}
}

// Restart-scope values persisted in the console reach Config at boot
// unless the environment pins them; an invalid one is skipped with a note.
func TestApplyPersisted(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("UPLINK_LONG_PER_KEY", "5")
	c := quietFromEnv(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	notes := ApplyPersisted(c, map[string]string{
		"inflightMax": "64", "longPerKey": "9", "sessionFailover": "false", "housekeeping": "false",
		"providerCooldown": "120", "auditRetentionDays": "400", "activeModelsUrl": "https://feed.example/m.json",
		"providerExclude": "m1=0x" + strings.Repeat("a", 40), "longSessionsMax": "999",
		"forwardIdle": "60", // live: applied by the API server, not here
		"webPublicUrl": "http://console.example",
	})
	if c.InflightMax != 64 || c.LongPerKey != 5 || c.SessionFailover || c.Housekeeping || c.ProviderCooldownSec != 120 ||
		c.AuditRetentionDays != 400 || c.ActiveModelsURL != "https://feed.example/m.json" || len(c.ProviderExclude["m1"]) != 1 {
		t.Fatalf("persisted values not applied: %+v", c)
	}
	if c.LongSessionsMax != 3 || c.ForwardIdleSec != 180 || c.WebPublicURL != "" {
		t.Fatalf("invalid or live values applied: long %d fwd %d web %q", c.LongSessionsMax, c.ForwardIdleSec, c.WebPublicURL)
	}
	if len(notes) != 2 || !strings.Contains(strings.Join(notes, ";"), "longSessionsMax") {
		t.Fatalf("notes %v", notes)
	}
}

func TestValidateProviderExclude(t *testing.T) {
	a, b := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("B", 40)
	for _, ok := range []string{"", "m1=" + a, "m1=" + a + "," + b + "; m2=" + b, "0x" + strings.Repeat("c", 64) + "=" + a} {
		if err := ValidateProviderExclude(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"m1", "m1=", "m1=0x12", "m1=" + a + ",x", "0x12=" + a, "=" + a} {
		if err := ValidateProviderExclude(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	got := FormatProviderExclude(ParseProviderExclude("m2=" + a + ";m1=" + b))
	if got != "m1="+strings.ToLower(b)+";m2="+a {
		t.Errorf("format %q", got)
	}
}
