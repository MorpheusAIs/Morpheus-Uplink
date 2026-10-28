package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Operator settings registry (#51). One row per setting the console shows.
// Each setting is managed in one place. Live and restart rows are managed
// in the console only (saved > default); the environment is not read for
// them. Env-only and router rows are set in .env only.
//
// Scope says when a change takes effect:
//   - ScopeLive: at once (the API server re-reads it).
//   - ScopeRestart: on the next Uplink start (read once at boot from Config).
//   - ScopeRouter: a proxy-router setting. The router reads it from its own
//     environment (deploy/secretvm: the router_network_env config); Uplink
//     cannot deliver a value to it, so the console shows it read-only.
//   - ScopeEnvOnly: shown read-only; set in .env only (security switches,
//     the console origin and the feed URLs).
//
// Group tags optional features so each can be dropped by deleting its rows
// here: "release" (the session release endpoint).
//
// The settings code names no setting outside this table except the five
// live ones it applies at once.
type SettingKind string

const (
	KindInt    SettingKind = "int"
	KindBool   SettingKind = "bool"
	KindEnum   SettingKind = "enum"
	KindString SettingKind = "string"
)

type SettingScope string

const (
	ScopeLive    SettingScope = "live"
	ScopeRestart SettingScope = "restart"
	ScopeRouter  SettingScope = "router"
	ScopeEnvOnly SettingScope = "env"
)

// SettingDef is one setting.
type SettingDef struct {
	Name string // console and settings-file key
	// Env: for env-only and router rows, the variable that sets it. For
	// live and restart rows, the former variable name: it is not read, only
	// named in a startup warning when it is still set.
	Env      string
	Kind     SettingKind
	Min, Max int      // KindInt
	Options  []string // KindEnum
	Default  string   // canonical form
	Scope    SettingScope
	Group    string
	Help     string                // tooltip: what it does and the allowed values
	Consumer string                // where the value is read (docs)
	check    func(string) error    // KindString: extra validation
	get      func(*Config) string  // effective Uplink value
	set      func(*Config, string) // live and restart rows: default, then saved (restart) at boot
}

var logLevels = []string{"debug", "info", "warn", "error"}
var routerLogLevels = []string{"debug", "info", "warn", "error", "dpanic", "panic", "fatal"}

func itoa(n int) string  { return strconv.Itoa(n) }
func atoi(s string) int  { n, _ := strconv.Atoi(s); return n }
func btoa(b bool) string { return strconv.FormatBool(b) }
func atob(s string) bool { b, _ := strconv.ParseBool(s); return b }

// Settings is the registry, in console order.
var Settings = []SettingDef{
	{Name: "sessionDurationDefault", Env: "SESSION_DURATION_SECONDS", Kind: KindInt, Min: 600, Max: 86400, Default: "600", Scope: ScopeLive,
		Help: "Session length for an open without X-Uplink-Session-Duration, in seconds (600–86400, at most the session max).", Consumer: "pool.SetDuration (live)",
		get: func(c *Config) string { return itoa(c.SessionDurationSec) }, set: func(c *Config, v string) { c.SessionDurationSec = atoi(v) }},
	{Name: "sessionDurationMax", Env: "UPLINK_SESSION_DURATION_MAX", Kind: KindInt, Min: 600, Max: 86400, Default: "1800", Scope: ScopeLive,
		Help: "Longest session any open may ask for, in seconds (600–86400).", Consumer: "pool.SetDurationMax (live)",
		get: func(c *Config) string { return itoa(c.SessionDurationMax) }, set: func(c *Config, v string) { c.SessionDurationMax = atoi(v) }},
	{Name: "sessionIdleGrace", Env: "UPLINK_SESSION_IDLE_GRACE_SECONDS", Kind: KindInt, Min: 60, Max: 3600, Default: "300", Scope: ScopeLive,
		Help: "Close a pooled session with nothing in flight this long after its last use, in seconds (60–3600).", Consumer: "idle sweep, read every tick (live)",
		get: func(c *Config) string { return itoa(c.SessionIdleGraceSec) }, set: func(c *Config, v string) { c.SessionIdleGraceSec = atoi(v) }},
	{Name: "forwardIdle", Env: "UPLINK_FORWARD_IDLE_SECONDS", Kind: KindInt, Min: 30, Max: 600, Default: "180", Scope: ScopeLive,
		Help: "Default first-byte and idle timer of an inference call, in seconds (30–600).", Consumer: "inference timers, per request (live)",
		get: func(c *Config) string { return itoa(c.ForwardIdleSec) }, set: func(c *Config, v string) { c.ForwardIdleSec = atoi(v) }},
	{Name: "logLevel", Env: "UPLINK_LOG_LEVEL", Kind: KindEnum, Options: logLevels, Default: "info", Scope: ScopeLive,
		Help: "Uplink log level: debug, info, warn or error. Audit lines are always written.", Consumer: "slog.LevelVar (live)",
		get: func(c *Config) string { return c.LogLevel }, set: func(c *Config, v string) { c.LogLevel = v }},

	{Name: "sessionFailover", Env: "SESSION_FAILOVER", Kind: KindBool, Default: "true", Scope: ScopeRestart,
		Help: "Move a model to another provider when the provider fails a prompt (true or false); false relays the provider's error. Takes effect after an Uplink restart.", Consumer: "api/openai.go (Config.SessionFailover)",
		get: func(c *Config) string { return btoa(c.SessionFailover) }, set: func(c *Config, v string) { c.SessionFailover = atob(v) }},
	{Name: "providerCooldown", Env: "UPLINK_PROVIDER_COOLDOWN_SECONDS", Kind: KindInt, Min: 0, Max: MaxProviderCooldownSec, Default: "600", Scope: ScopeRestart,
		Help: "How long a failed provider is avoided for that model, in seconds (0–86400; 0 keeps no memory across requests). Takes effect after an Uplink restart.", Consumer: "cmd/uplink pool.SetProviderCooldown",
		get: func(c *Config) string { return itoa(c.ProviderCooldownSec) }, set: func(c *Config, v string) { c.ProviderCooldownSec = atoi(v) }},
	{Name: "providerExclude", Env: "UPLINK_PROVIDER_EXCLUDE", Kind: KindString, Default: "", Scope: ScopeRestart, check: ValidateProviderExclude,
		Help: "Providers never opened for a model: <model>=<0x provider>[,<0x provider>]…, entries separated by ';' (empty for none). Takes effect after an Uplink restart.", Consumer: "cmd/uplink pool.SetProviderExclude",
		get: func(c *Config) string { return FormatProviderExclude(c.ProviderExclude) }, set: func(c *Config, v string) { c.ProviderExclude = ParseProviderExclude(v) }},
	{Name: "sessionRelease", Env: "UPLINK_SESSION_RELEASE", Kind: KindBool, Default: "true", Scope: ScopeRestart, Group: "release",
		Help: "Lets a generated key drop its hold on a model's session with POST /v1/sessions/release (true or false; false answers 403 release_disabled). The session closes when no holder and no request is left; otherwise the idle sweep closes it. Takes effect after an Uplink restart.", Consumer: "api handleSessionRelease (Config.SessionRelease)",
		get: func(c *Config) string { return btoa(c.SessionRelease) }, set: func(c *Config, v string) { c.SessionRelease = atob(v) }},
	{Name: "auditRetentionDays", Env: "AUDIT_RETENTION_DAYS", Kind: KindInt, Min: 1, Max: 3650, Default: "90", Scope: ScopeRestart,
		Help: "Days the high-value audit stream (audit-security.jsonl) is kept (1–3650). Takes effect after an Uplink restart.", Consumer: "api.NewServer openAuditSink",
		get: func(c *Config) string { return itoa(c.AuditRetentionDays) }, set: func(c *Config, v string) { c.AuditRetentionDays = atoi(v) }},
	{Name: "housekeeping", Env: "HOUSEKEEPING", Kind: KindBool, Default: "true", Scope: ScopeRestart,
		Help: "Post-midnight stake reclaim via the router API (true or false). Takes effect after an Uplink restart.", Consumer: "cmd/uplink housekeep.New",
		get: func(c *Config) string { return btoa(c.Housekeeping) }, set: func(c *Config, v string) { c.Housekeeping = atob(v) }},
	{Name: "activeModelsUrl", Env: "ACTIVE_MODELS_URL", Kind: KindString, Default: "https://active.mor.org/gateway_models.json", Scope: ScopeEnvOnly,
		Help: "Model catalog feed (an https URL); it decides the model-id mapping. Set in .env only; takes effect after an Uplink restart.", Consumer: "cmd/uplink catalog.New",
		get: func(c *Config) string { return c.ActiveModelsURL }},
	{Name: "gatewayBidsUrl", Env: "GATEWAY_BIDS_URL", Kind: KindString, Default: "https://active.mor.org/gateway_bids.json", Scope: ScopeEnvOnly,
		Help: "Rated bids feed (an https URL). Informational: Uplink stores it but does not fetch it today, so it has no effect. Set in .env only.", Consumer: "cmd/uplink catalog.WithBidsURL (stored, not fetched)",
		get: func(c *Config) string { return c.GatewayBidsURL }},
	{Name: "webPublicUrl", Env: "WEB_PUBLIC_URL", Kind: KindString, Default: "", Scope: ScopeEnvOnly,
		Help: "Public origin of this console, https://host[:port] (http://localhost only with UPLINK_DEV_HTTP). Admin changes are accepted from it only, and /admin, /gui and /node answer only on its host. Set in .env only; takes effect after an Uplink restart.", Consumer: "api CSRF, settings origin and console host checks (Config.WebPublicURL)",
		get: func(c *Config) string { return c.WebPublicURL }},

	{Name: "masterKeyAdmin", Env: "MASTER_KEY_ADMIN", Kind: KindBool, Default: "false", Scope: ScopeEnvOnly,
		Help: "Lets the master API key call /admin (true or false). Security switch: set in .env only; takes effect after an Uplink restart.", Consumer: "api requireAdmin (Config.MasterKeyAdmin)",
		get: func(c *Config) string { return btoa(c.MasterKeyAdmin) }},

	{Name: "proxyStoreChatContext", Env: "PROXY_STORE_CHAT_CONTEXT", Kind: KindBool, Default: "false", Scope: ScopeRouter,
		Help: "Router: store chat context in its storage (true or false). Router setting: change it in the router's .env and restart the router."},
	{Name: "proxyForwardChatContext", Env: "PROXY_FORWARD_CHAT_CONTEXT", Kind: KindBool, Default: "false", Scope: ScopeRouter,
		Help: "Router: prepend the stored history to each prompt (true or false). Router setting: change it in the router's .env and restart the router."},
	{Name: "routerLogLevelApp", Env: "LOG_LEVEL_APP", Kind: KindEnum, Options: routerLogLevels, Default: "warn", Scope: ScopeRouter,
		Help: "Router application log level (debug, info, warn, error, dpanic, panic, fatal). Router setting: change it in the router's .env and restart the router."},
	{Name: "routerLogLevelTcp", Env: "LOG_LEVEL_TCP", Kind: KindEnum, Options: routerLogLevels, Default: "warn", Scope: ScopeRouter,
		Help: "Router TCP log level (debug, info, warn, error, dpanic, panic, fatal). Router setting: change it in the router's .env and restart the router."},
	{Name: "routerLogLevelEthRpc", Env: "LOG_LEVEL_ETH_RPC", Kind: KindEnum, Options: routerLogLevels, Default: "warn", Scope: ScopeRouter,
		Help: "Router chain RPC log level (debug, info, warn, error, dpanic, panic, fatal). Router setting: change it in the router's .env and restart the router."},
}

// SecretEnv are the secrets that stay in .env only: never a setting,
// never in a settings file, never in an API answer.
var SecretEnv = []string{"WALLET_PRIVATE_KEY", "ETH_NODE_ADDRESS", "ETH_RPC_URL", "COOKIE_CONTENT", "ROUTER_AUTH", "ADMIN_PASSWORD", "API_KEY_SEED", "MASTER_KEY"}

var secretNames = []string{"walletprivatekey", "privatekey", "ethnodeaddress", "ethnodeurl", "ethrpcurl", "cookiecontent", "cookie", "routerauth", "adminpassword", "password", "apikeyseed", "seed", "masterkey", "promptkey"}

// IsSecretKey reports whether k names a secret (env or console form, any case).
func IsSecretKey(k string) bool {
	n := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(k))
	for _, e := range SecretEnv {
		if strings.EqualFold(k, e) || n == strings.ToLower(strings.ReplaceAll(e, "_", "")) {
			return true
		}
	}
	for _, s := range secretNames {
		if n == s {
			return true
		}
	}
	return false
}

// SettingByName finds a setting by its console name.
func SettingByName(name string) (SettingDef, bool) {
	for _, d := range Settings {
		if d.Name == name {
			return d, true
		}
	}
	return SettingDef{}, false
}

// Settable: the console (and a settings file) may set it.
func (d SettingDef) Settable() bool { return d.Scope == ScopeLive || d.Scope == ScopeRestart }

// Effective is Uplink's value now ("" for router settings).
func (d SettingDef) Effective(c *Config) string {
	if d.get == nil || c == nil {
		return ""
	}
	return d.get(c)
}

// Validate checks v and returns its canonical form.
func (d SettingDef) Validate(v string) (string, error) {
	switch d.Kind {
	case KindInt:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < d.Min || n > d.Max {
			return "", fmt.Errorf("%s must be a whole number %d…%d", d.Name, d.Min, d.Max)
		}
		return itoa(n), nil
	case KindBool:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return "", fmt.Errorf("%s must be true or false", d.Name)
		}
		return btoa(b), nil
	case KindEnum:
		l := strings.ToLower(strings.TrimSpace(v))
		if d.Name == "logLevel" {
			l = ParseLogLevel(l)
		}
		for _, o := range d.Options {
			if l == o {
				return l, nil
			}
		}
		return "", fmt.Errorf("%s must be one of %s", d.Name, strings.Join(d.Options, ", "))
	case KindString:
		v = strings.TrimSpace(v)
		if len(v) > 4096 {
			return "", fmt.Errorf("%s is too long", d.Name)
		}
		if d.check != nil {
			if err := d.check(v); err != nil {
				return "", fmt.Errorf("%s: %v", d.Name, err)
			}
		}
		return v, nil
	}
	return "", fmt.Errorf("%s cannot be set", d.Name)
}

// applySettingDefaults sets every live and restart setting to its
// default. FromEnv calls it; the saved values are applied on top.
func applySettingDefaults(c *Config) {
	for _, d := range Settings {
		if d.Settable() && d.set != nil {
			d.set(c, d.Default)
		}
	}
}

// ignoredSettingEnv names the former environment variables of live and
// restart settings that are still set (names only, never values).
func ignoredSettingEnv() []string {
	var out []string
	for _, d := range Settings {
		if d.Settable() && d.Env != "" && strings.TrimSpace(os.Getenv(d.Env)) != "" {
			out = append(out, d.Env)
		}
	}
	return out
}

// ApplyPersisted applies persisted ScopeRestart values (by console name).
// A value that no longer validates is
// skipped, and so is a persisted value for an env-only or router row
// (never applied from the file); the returned notes (names only) go to the
// ops audit stream.
func ApplyPersisted(c *Config, vals map[string]string) []string {
	var notes []string
	for _, d := range Settings {
		raw, ok := vals[d.Name]
		if !ok {
			continue
		}
		if d.Scope == ScopeEnvOnly || d.Scope == ScopeRouter {
			warnf("config: persisted %s is not a console setting; ignored", d.Name)
			notes = append(notes, d.Name+" is set in .env only; persisted value ignored")
			continue
		}
		if d.Scope != ScopeRestart {
			continue
		}
		v, err := d.Validate(raw)
		if err != nil {
			warnf("config: persisted %s is invalid; ignored", d.Name)
			notes = append(notes, d.Name+" persisted value invalid; ignored")
			continue
		}
		d.set(c, v)
	}
	c.SettingNotes = append(c.SettingNotes, notes...)
	return notes
}

// ValidateProviderExclude is ParseProviderExclude without the forgiveness:
// any bad entry or provider is an error.
func ValidateProviderExclude(v string) error {
	for i, ent := range strings.Split(v, ";") {
		ent = strings.TrimSpace(ent)
		if ent == "" {
			continue
		}
		model, provs, ok := strings.Cut(ent, "=")
		model = strings.ToLower(strings.TrimSpace(model))
		if !ok || (!reModelID.MatchString(model) && (strings.HasPrefix(model, "0x") || !reModelName.MatchString(model))) {
			return fmt.Errorf("entry %d is not <model>=<provider>[,<provider>]", i+1)
		}
		for j, pr := range strings.Split(provs, ",") {
			if !reProviderAddr.MatchString(strings.TrimSpace(pr)) {
				return fmt.Errorf("entry %d provider %d is not a 0x address", i+1, j+1)
			}
		}
	}
	return nil
}

// FormatProviderExclude is the canonical string form of a parsed exclusion map.
func FormatProviderExclude(m map[string][]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(m[k], ","))
	}
	return strings.Join(parts, ";")
}
