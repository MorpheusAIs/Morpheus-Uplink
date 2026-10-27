// Package config loads UPLINK configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func sortStrings(s []string) { sort.Strings(s) }

type Config struct {
	// Listen address for the gateway, e.g. ":8080".
	Listen string
	// Base URL of the proxy-router admin API, e.g. "http://proxy-router:8082".
	RouterURL string
	// Basic-auth credentials for the proxy-router ("user:password", the
	// same value as the router's COOKIE_CONTENT).
	RouterUser string
	RouterPass string
	// Password for the admin API / GUI (username is always "admin").
	AdminPassword string
	// Seed for deterministic master API key derivation. Rotating it
	// invalidates the master key. This is what makes the gateway survive
	// a full storage wipe: re-deploy with the same seed, same key works.
	APIKeySeed string
	// Directory for the JSON state file (keys, usage). May be ephemeral.
	DataDir string
	// URL of the gateway model catalog JSON (ACTIVE_MODELS_URL).
	ActiveModelsURL string
	// Companion gateway bids feed (GATEWAY_BIDS_URL).
	GatewayBidsURL string
	// Session behavior.
	SessionDurationSec int
	// SessionFailover turns on Uplink's provider failover on prompt errors
	// (the router's own failover is always off).
	SessionFailover bool
	// ProviderCooldownSec: per model+provider memory after a provider
	// failure (0 = none across requests).
	ProviderCooldownSec int
	// SESSION_DIRECT_PAYMENT (true) is no longer honoured (sessions are always
	// staking-mode); set so main can warn.
	DirectPaymentIgnored bool
	// Close pooled sessions on shutdown to recover stake sooner.
	CloseSessionsOnExit bool
	// Optional Diamond address (legacy on-hold helpers; reclaim uses router).
	DiamondAddress string
	EthNodeChainID int64
	Housekeeping   bool
	// Retention for admin usage-history prune (POST /admin/disk/prune-usage).
	UsagePruneDays int
	// Boot session reconcile: how long opens wait for the first session
	// list, and the background retry interval after that.
	ReconcileGateSec  int
	ReconcileRetrySec int
	// Generic read-only JSON-RPC URL for optional on-chain alerts (no wallet key).
	EthRPCURL string
	// Public HTTPS origin for CSRF Origin checks (required for cookie admin auth).
	WebPublicURL string
	// MASTER_KEY_ADMIN: when true, Bearer master key may access /admin (audited). Default false.
	MasterKeyAdmin bool
	// MalformedBoolVars names security switches that were set to something
	// that is not a boolean (names only, never values). Each fell back to its
	// safe value (MASTER_KEY_ADMIN: false).
	MalformedBoolVars []string
	// UPLINK_DEV_HTTP: allow non-Secure cookie for localhost only.
	DevHTTP bool
	// TRUSTED_PROXY_CIDR: comma-separated CIDRs that may supply X-Forwarded-For.
	TrustedProxyCIDRs []string
	// UPLINK_RESET_2FA: break-glass wipe of 2FA/sessions/lockouts once per marker cycle.
	Reset2FA bool
	// AUDIT_RETENTION_DAYS: retention of the high-value audit stream
	// (audit-security.jsonl), 1…3650, default 90. The failure/ops stream
	// keeps a fixed 30 days.
	AuditRetentionDays int
	// UPLINK_PROVIDER_EXCLUDE: providers never opened for a model,
	// "<model>=<provider>[,<provider>]…", entries separated by ";". Model
	// is a registration id or a catalog name; providers are 0x addresses.
	// Validated at boot: a bad entry is warned and ignored.
	ProviderExclude map[string][]string

	// Long-job core (S5).
	// SessionDurationMax caps X-Uplink-Session-Duration and every open
	// (UPLINK_SESSION_DURATION_MAX, 600…86400, default 1800).
	SessionDurationMax int
	// ForwardIdleSec is the default first-byte/idle timer of an inference
	// call (UPLINK_FORWARD_IDLE_SECONDS, 30…600, default 180).
	ForwardIdleSec int
	// SessionIdleGraceSec: the idle sweep closes a pooled session with
	// nothing in flight this long after its last use
	// (UPLINK_SESSION_IDLE_GRACE_SECONDS, 60…3600, default 300).
	SessionIdleGraceSec int
	// SessionRelease turns on POST /v1/sessions/release
	// (UPLINK_SESSION_RELEASE, default true).
	SessionRelease bool

	// LogLevel is UPLINK_LOG_LEVEL (debug, info, warn, error; default info).
	LogLevel string
	// EnvPinned names the S6 settings whose env var holds a valid value;
	// those cannot be changed in the admin console.
	EnvPinned map[string]bool
	// SettingNotes are persisted settings ignored at boot (names only).
	SettingNotes []string
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen:               getenv("UPLINK_LISTEN", ":8080"),
		RouterURL:            strings.TrimRight(getenv("ROUTER_URL", "http://proxy-router:8082"), "/"),
		AdminPassword:        os.Getenv("ADMIN_PASSWORD"),
		APIKeySeed:           os.Getenv("API_KEY_SEED"),
		DataDir:              getenv("DATA_DIR", "./data"),
		ActiveModelsURL:      getenv("ACTIVE_MODELS_URL", "https://active.mor.org/gateway_models.json"),
		GatewayBidsURL:       getenv("GATEWAY_BIDS_URL", "https://active.mor.org/gateway_bids.json"),
		SessionDurationSec:   getenvRangeInt("SESSION_DURATION_SECONDS", 600, 600, 86400),
		SessionFailover:      getenvBool("SESSION_FAILOVER", true),
		ProviderCooldownSec:  getenvClampInt("UPLINK_PROVIDER_COOLDOWN_SECONDS", 600, 0, MaxProviderCooldownSec),
		DirectPaymentIgnored: getenvBool("SESSION_DIRECT_PAYMENT", false),
		CloseSessionsOnExit:  getenvBool("CLOSE_SESSIONS_ON_EXIT", true),
		DiamondAddress:       getenv("DIAMOND_CONTRACT_ADDRESS", "0x6aBE1d282f72B474E54527D93b979A4f64d3030a"),
		EthNodeChainID:       int64(getenvInt("ETH_NODE_CHAIN_ID", 8453)),
		Housekeeping:         getenvBool("HOUSEKEEPING", true),
		UsagePruneDays:       getenvInt("USAGE_PRUNE_DAYS", 30),
		ReconcileGateSec:     getenvInt("UPLINK_RECONCILE_GATE_SECONDS", 60),
		ReconcileRetrySec:    getenvInt("UPLINK_RECONCILE_RETRY_SECONDS", 30),
		EthRPCURL:            firstNonEmpty(os.Getenv("ETH_RPC_URL"), os.Getenv("ETH_NODE_ADDRESS")),
		WebPublicURL:         strings.TrimRight(os.Getenv("WEB_PUBLIC_URL"), "/"),
		DevHTTP:              getenvBool("UPLINK_DEV_HTTP", false),
		TrustedProxyCIDRs:    splitCSV(os.Getenv("TRUSTED_PROXY_CIDR")),
		Reset2FA:             getenvBool("UPLINK_RESET_2FA", false),
		AuditRetentionDays:   getenvRangeInt("AUDIT_RETENTION_DAYS", 90, 1, 3650),
		ProviderExclude:      ParseProviderExclude(os.Getenv("UPLINK_PROVIDER_EXCLUDE")),

		SessionDurationMax:  getenvRangeInt("UPLINK_SESSION_DURATION_MAX", 1800, 600, 86400),
		ForwardIdleSec:      getenvRangeInt("UPLINK_FORWARD_IDLE_SECONDS", 180, 30, 600),
		SessionIdleGraceSec: getenvRangeInt("UPLINK_SESSION_IDLE_GRACE_SECONDS", 300, 60, 3600),
		SessionRelease:      getenvBool("UPLINK_SESSION_RELEASE", true),
	}

	c.LogLevel = getenvLogLevel("UPLINK_LOG_LEVEL")
	// S6: settings set in the environment win over the admin console and
	// show there as "set by environment". Only a valid value pins.
	c.EnvPinned = map[string]bool{}
	for _, d := range Settings {
		if d.Scope != ScopeRouter && d.envPins(os.Getenv(d.Env)) {
			c.EnvPinned[d.Env] = true
		}
	}

	// The cap wins: the default is clamped down to the max, never the
	// max raised to the default.
	if c.SessionDurationSec > c.SessionDurationMax {
		warnf("config: SESSION_DURATION_SECONDS is above UPLINK_SESSION_DURATION_MAX; using %d", c.SessionDurationMax)
		c.SessionDurationSec = c.SessionDurationMax
	}

	// Security switches fall back to their safe value when malformed.
	c.MasterKeyAdmin = c.securityBool("MASTER_KEY_ADMIN", false)

	// ROUTER_AUTH mirrors the proxy-router's COOKIE_CONTENT format.
	// COOKIE_CONTENT is accepted as a fallback so a single secret can feed
	// both containers via env_file (no compose ${} interpolation needed).
	auth := strings.TrimSpace(getenv("ROUTER_AUTH", os.Getenv("COOKIE_CONTENT")))
	if user, pass, ok := strings.Cut(auth, ":"); ok {
		c.RouterUser, c.RouterPass = user, pass
	}

	var missing []string
	if c.AdminPassword == "" {
		missing = append(missing, "ADMIN_PASSWORD")
	}
	if c.APIKeySeed == "" {
		missing = append(missing, "API_KEY_SEED")
	}
	if c.RouterUser == "" || c.RouterPass == "" {
		missing = append(missing, "ROUTER_AUTH or COOKIE_CONTENT (user:password)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	for _, cred := range []string{auth, os.Getenv("COOKIE_CONTENT")} {
		if err := ValidateAdminPassword(c.AdminPassword, cred); err != nil {
			return nil, err
		}
	}
	if c.DevHTTP {
		u := strings.ToLower(c.WebPublicURL)
		if u != "http://localhost" && u != "http://127.0.0.1" &&
			!strings.HasPrefix(u, "http://localhost:") && !strings.HasPrefix(u, "http://127.0.0.1:") {
			return nil, fmt.Errorf("UPLINK_DEV_HTTP=true requires WEB_PUBLIC_URL http://localhost or http://127.0.0.1")
		}
	}
	return c, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// MaxProviderCooldownSec caps UPLINK_PROVIDER_COOLDOWN_SECONDS (one day).
const MaxProviderCooldownSec = 86400

// getenvClampInt reads a whole number at most hi: a larger value is
// clamped to hi with a warning (name only). Unset: def. Malformed or below
// lo: a warning and def.
func getenvClampInt(key string, def, lo, hi int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		warnf("config: %s is not a whole number; using the default %d", key, def)
		return def
	}
	if n < lo {
		warnf("config: %s is below %d; using the default %d", key, lo, def)
		return def
	}
	if n > hi {
		warnf("config: %s is above %d; using %d", key, hi, hi)
		return hi
	}
	return n
}

// getenvRangeInt reads a whole number in [lo, hi]. Unset: def. Malformed
// or out of range: a warning (name only) and def.
var (
	reProviderAddr = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	reModelID      = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	reModelName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,127}$`)
)

// ParseProviderExclude parses UPLINK_PROVIDER_EXCLUDE. Entries are
// "<model>=<provider>[,<provider>]…" separated by ";". An entry with a bad
// model or provider is logged (by position, not value) and ignored;
// a bad provider drops only that provider.
func ParseProviderExclude(v string) map[string][]string {
	out := map[string][]string{}
	if strings.TrimSpace(v) == "" {
		return out
	}
	for i, ent := range strings.Split(v, ";") {
		ent = strings.TrimSpace(ent)
		if ent == "" {
			continue
		}
		model, provs, ok := strings.Cut(ent, "=")
		model = strings.ToLower(strings.TrimSpace(model))
		if !ok || (!reModelID.MatchString(model) && (strings.HasPrefix(model, "0x") || !reModelName.MatchString(model))) {
			warnf("config: UPLINK_PROVIDER_EXCLUDE entry %d is not <model>=<provider>[,<provider>]; ignored", i+1)
			continue
		}
		for j, pr := range strings.Split(provs, ",") {
			pr = strings.ToLower(strings.TrimSpace(pr))
			if !reProviderAddr.MatchString(pr) {
				warnf("config: UPLINK_PROVIDER_EXCLUDE entry %d provider %d is not a 0x address; ignored", i+1, j+1)
				continue
			}
			out[model] = append(out[model], pr)
		}
	}
	return out
}

func getenvRangeInt(key string, def, lo, hi int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		warnf("config: %s must be a whole number %d…%d; using the default %d", key, lo, hi, def)
		return def
	}
	return n
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// securityBool reads a security switch. Unset, empty or blank: false (the
// code default). A boolean (strconv.ParseBool, surrounding space ignored):
// that value. Anything else: safe, and the name is recorded in
// MalformedBoolVars so it can be logged and shown (without the value).
func (c *Config) securityBool(key string, safe bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		c.MalformedBoolVars = append(c.MalformedBoolVars, key)
		return safe
	}
	return b
}

var weakPasswords = map[string]struct{}{
	"": {}, "password": {}, "admin": {}, "admin123": {}, "changeme": {},
	"secret": {}, "123456": {}, "12345678": {}, "password123": {},
	"letmein": {}, "qwerty": {}, "uplink": {}, "morpheus": {},
	"YOUR_STRONG_PASSWORD": {}, "<gui_admin_password>": {}, "<strong_password>": {},
	"YOUR_STRONG_PASSWORD_HERE": {},
}

// ValidateAdminPassword refuses weak, template, or cookie-equal admin passwords.
func ValidateAdminPassword(adminPass, cookieContent string) error {
	p := strings.TrimSpace(adminPass)
	if len(p) < 12 {
		return fmt.Errorf("ADMIN_PASSWORD must be at least 12 characters")
	}
	low := strings.ToLower(p)
	if _, bad := weakPasswords[low]; bad {
		return fmt.Errorf("ADMIN_PASSWORD is a known-weak or template value")
	}
	if _, bad := weakPasswords[p]; bad {
		return fmt.Errorf("ADMIN_PASSWORD is a known-weak or template value")
	}
	for w := range weakPasswords {
		if w != "" && (low == w || strings.ReplaceAll(low, w, "") == "") {
			return fmt.Errorf("ADMIN_PASSWORD is a known-weak or template value")
		}
	}
	if cookieContent != "" {
		if _, pass, ok := strings.Cut(cookieContent, ":"); ok && pass == p {
			return fmt.Errorf("ADMIN_PASSWORD must not equal COOKIE_CONTENT password")
		}
		if cookieContent == p {
			return fmt.Errorf("ADMIN_PASSWORD must not equal COOKIE_CONTENT")
		}
	}
	return nil
}

// ParseLogLevel returns the canonical level name or "" when v is not one.
func ParseLogLevel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return "debug"
	case "info":
		return "info"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	}
	return ""
}

func getenvLogLevel(key string) string {
	v := os.Getenv(key)
	if v == "" {
		return "info"
	}
	if l := ParseLogLevel(v); l != "" {
		return l
	}
	warnf("config: %s is not debug, info, warn or error; using info", key)
	return "info"
}
