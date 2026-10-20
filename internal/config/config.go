// Package config loads UPLINK configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
	SessionFailover    bool
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
	// ADMIN_2FA_REQUIRED: when true and nothing enrolled, only login+enroll work.
	// Unset or empty: false (the code default). Set but not a boolean (for
	// example a literal "${ADMIN_2FA_REQUIRED:-true}" from a host that does not
	// interpolate): true, fail closed, and listed in MalformedBoolVars.
	Admin2FARequired bool
	// MalformedBoolVars names security switches that were set to something
	// that is not a boolean (names only, never values). Each fell back to its
	// safe value: ADMIN_2FA_REQUIRED true, MASTER_KEY_ADMIN false.
	MalformedBoolVars []string
	// UPLINK_DEV_HTTP: allow non-Secure cookie for localhost only.
	DevHTTP bool
	// TRUSTED_PROXY_CIDR: comma-separated CIDRs that may supply X-Forwarded-For.
	TrustedProxyCIDRs []string
	// UPLINK_RESET_2FA: break-glass wipe of 2FA/sessions/lockouts once per marker cycle.
	Reset2FA bool
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
		SessionDurationSec:   getenvInt("SESSION_DURATION_SECONDS", 600),
		SessionFailover:      getenvBool("SESSION_FAILOVER", true),
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
	}

	// Security switches fall back to their safe value when malformed.
	c.Admin2FARequired = c.securityBool("ADMIN_2FA_REQUIRED", true)
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
