// Package config loads UPLINK configuration from environment variables.
package config

import (
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// Listen address for the gateway, e.g. ":8080".
	Listen string
	// Base URL of the proxy-router admin API, e.g. "http://proxy-router:8082".
	RouterURL string
	// Basic-auth credentials for the proxy-router ("user:password").
	// Prefer ROUTER_AUTH (scoped uplink user). COOKIE_CONTENT is a legacy
	// fallback during migration; production should not give Uplink the admin cookie.
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
	DirectPayment      bool
	// Close pooled sessions on shutdown to recover stake sooner.
	CloseSessionsOnExit bool
	// Optional Diamond address (legacy on-hold helpers; reclaim uses router).
	DiamondAddress string
	EthNodeChainID int64
	Housekeeping   bool
	// Retention for admin usage-history prune (POST /admin/disk/prune-usage).
	UsagePruneDays int
	// Required MOR agent allowance (wei) for scoped router user init.
	// Validated in later hardening; optional here until C4 enforces boot refuse.
	UplinkMORAllowanceWei string
	// Optional path to a file containing "user:password" for ROUTER_AUTH
	// (written by the init sidecar, mode 0400).
	RouterAuthFile string
	// Generic read-only JSON-RPC URL for optional on-chain alerts (no wallet key).
	EthRPCURL string
	// Public HTTPS origin for CSRF Origin checks (required for cookie admin auth).
	WebPublicURL string
	// MASTER_KEY_ADMIN: when true, Bearer master key may access /admin (audited). Default false.
	MasterKeyAdmin bool
	// ADMIN_2FA_REQUIRED: when true and nothing enrolled, only login+enroll work. Default false.
	Admin2FARequired bool
	// UPLINK_DEV_HTTP: allow non-Secure cookie for localhost only.
	DevHTTP bool
	// TRUSTED_PROXY_CIDR: comma-separated CIDRs that may supply X-Forwarded-For.
	TrustedProxyCIDRs []string
	// UPLINK_RESET_2FA: break-glass wipe of 2FA/sessions/lockouts once per marker cycle.
	Reset2FA bool
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen:              getenv("UPLINK_LISTEN", ":8080"),
		RouterURL:           strings.TrimRight(getenv("ROUTER_URL", "http://proxy-router:8082"), "/"),
		AdminPassword:       os.Getenv("ADMIN_PASSWORD"),
		APIKeySeed:          os.Getenv("API_KEY_SEED"),
		DataDir:             getenv("DATA_DIR", "./data"),
		ActiveModelsURL:     getenv("ACTIVE_MODELS_URL", "https://active.mor.org/gateway_models.json"),
		GatewayBidsURL:      getenv("GATEWAY_BIDS_URL", "https://active.mor.org/gateway_bids.json"),
		SessionDurationSec:  getenvInt("SESSION_DURATION_SECONDS", 600),
		SessionFailover:     getenvBool("SESSION_FAILOVER", true),
		DirectPayment:       getenvBool("SESSION_DIRECT_PAYMENT", false),
		CloseSessionsOnExit: getenvBool("CLOSE_SESSIONS_ON_EXIT", true),
		DiamondAddress:      getenv("DIAMOND_CONTRACT_ADDRESS", "0x6aBE1d282f72B474E54527D93b979A4f64d3030a"),
		EthNodeChainID:      int64(getenvInt("ETH_NODE_CHAIN_ID", 8453)),
		Housekeeping:        getenvBool("HOUSEKEEPING", true),
		UsagePruneDays:      getenvInt("USAGE_PRUNE_DAYS", 30),
		UplinkMORAllowanceWei: os.Getenv("UPLINK_MOR_ALLOWANCE_WEI"),
		RouterAuthFile:      os.Getenv("ROUTER_AUTH_FILE"),
		EthRPCURL:           firstNonEmpty(os.Getenv("ETH_RPC_URL"), os.Getenv("ETH_NODE_ADDRESS")),
		WebPublicURL:        strings.TrimRight(os.Getenv("WEB_PUBLIC_URL"), "/"),
		MasterKeyAdmin:      getenvBool("MASTER_KEY_ADMIN", false),
		Admin2FARequired:    getenvBool("ADMIN_2FA_REQUIRED", false),
		DevHTTP:             getenvBool("UPLINK_DEV_HTTP", false),
		TrustedProxyCIDRs:   splitCSV(os.Getenv("TRUSTED_PROXY_CIDR")),
		Reset2FA:            getenvBool("UPLINK_RESET_2FA", false),
	}

	auth := getenv("ROUTER_AUTH", os.Getenv("COOKIE_CONTENT"))
	if auth == "" && c.RouterAuthFile != "" {
		if b, err := os.ReadFile(c.RouterAuthFile); err == nil {
			auth = strings.TrimSpace(string(b))
		}
	}
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
		missing = append(missing, "ROUTER_AUTH or ROUTER_AUTH_FILE (user:password)")
	}
	if err := requirePositiveWei("UPLINK_MOR_ALLOWANCE_WEI", c.UplinkMORAllowanceWei); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if err := ValidateAdminPassword(c.AdminPassword, os.Getenv("COOKIE_CONTENT")); err != nil {
		return nil, err
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

// requirePositiveWei refuses boot when name is missing, empty, or zero.
func requirePositiveWei(name, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s is required (positive wei budget for scoped router user)", name)
	}
	n := new(big.Int)
	if _, ok := n.SetString(raw, 10); !ok {
		return fmt.Errorf("%s must be a base-10 integer (wei)", name)
	}
	if n.Sign() <= 0 {
		return fmt.Errorf("%s must be > 0 (got %s)", name, raw)
	}
	return nil
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
