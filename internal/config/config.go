// Package config loads UPLINK configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	// Listen address for the gateway, e.g. ":8080".
	Listen string
	// Base URL of the proxy-router admin API, e.g. "http://proxy-router:8082".
	RouterURL string
	// Basic-auth credentials for the proxy-router ("user:password").
	// Scoped router user (never "admin"). Loaded from ROUTER_AUTH_FILE, then
	// ROUTER_AUTH; COOKIE_CONTENT only when UPLINK_DEV_HTTP is set.
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
	// Optional MOR agent allowance cap (wei). Unset/empty = init uses the
	// wallet MOR balance at boot. When set must be digits only; 0 is an
	// explicit zero cap (never "use balance"). Leading zeros are normalized.
	UplinkMORAllowanceWei string
	// Path to allowance metadata JSON written by router-user-init
	// (wei, source, warning). Defaults beside ROUTER_AUTH_FILE.
	MORAllowanceFile string
	// Effective allowance from file (or env override when no file).
	MORAllowanceWei     string
	MORAllowanceSource  string // balance | override | zero | unknown
	MORAllowanceWarning string
	// Optional path to a file containing "user:password" for ROUTER_AUTH
	// (written by the init sidecar, mode 0400).
	RouterAuthFile string
	// Router wallet address recorded by router-user-init
	// (UPLINK_ROUTER_WALLET_FILE, default router.wallet.json beside the auth
	// file). Public; used so the funding address shows even when router
	// calls fail.
	RouterWalletFile    string
	RouterWalletAddress string
	// Exit (so the container restart re-reads the current credential) after
	// the router has rejected the scoped credential for this long.
	// UPLINK_ROUTER_401_EXIT_SECONDS; 0 disables. Default 120.
	Router401ExitSec int
	// Allowance manager status file (read-only to Uplink) and the directory
	// where Uplink drops the presence-only resync request flag. Both are
	// separate volumes and must never be under DATA_DIR.
	AllowanceStatusFile string
	AllowanceCtlDir     string
	// Manager poll interval; status is "fresh" within 3 polls.
	AllowancePollSec int
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
		Listen:                getenv("UPLINK_LISTEN", ":8080"),
		RouterURL:             strings.TrimRight(getenv("ROUTER_URL", "http://proxy-router:8082"), "/"),
		AdminPassword:         os.Getenv("ADMIN_PASSWORD"),
		APIKeySeed:            os.Getenv("API_KEY_SEED"),
		DataDir:               getenv("DATA_DIR", "./data"),
		ActiveModelsURL:       getenv("ACTIVE_MODELS_URL", "https://active.mor.org/gateway_models.json"),
		GatewayBidsURL:        getenv("GATEWAY_BIDS_URL", "https://active.mor.org/gateway_bids.json"),
		SessionDurationSec:    getenvInt("SESSION_DURATION_SECONDS", 600),
		SessionFailover:       getenvBool("SESSION_FAILOVER", true),
		DirectPaymentIgnored:  getenvBool("SESSION_DIRECT_PAYMENT", false),
		CloseSessionsOnExit:   getenvBool("CLOSE_SESSIONS_ON_EXIT", true),
		DiamondAddress:        getenv("DIAMOND_CONTRACT_ADDRESS", "0x6aBE1d282f72B474E54527D93b979A4f64d3030a"),
		EthNodeChainID:        int64(getenvInt("ETH_NODE_CHAIN_ID", 8453)),
		Housekeeping:          getenvBool("HOUSEKEEPING", true),
		UsagePruneDays:        getenvInt("USAGE_PRUNE_DAYS", 30),
		UplinkMORAllowanceWei: strings.TrimSpace(os.Getenv("UPLINK_MOR_ALLOWANCE_WEI")),
		MORAllowanceFile:      strings.TrimSpace(os.Getenv("UPLINK_MOR_ALLOWANCE_FILE")),
		RouterAuthFile:        os.Getenv("ROUTER_AUTH_FILE"),
		RouterWalletFile:      strings.TrimSpace(os.Getenv("UPLINK_ROUTER_WALLET_FILE")),
		Router401ExitSec:      getenvInt("UPLINK_ROUTER_401_EXIT_SECONDS", 120),
		AllowanceStatusFile:   strings.TrimSpace(os.Getenv("UPLINK_ALLOWANCE_STATUS_FILE")),
		AllowanceCtlDir:       strings.TrimSpace(os.Getenv("UPLINK_ALLOWANCE_CTL_DIR")),
		AllowancePollSec:      getenvInt("UPLINK_ALLOWANCE_POLL_SECONDS", 60),
		EthRPCURL:             firstNonEmpty(os.Getenv("ETH_RPC_URL"), os.Getenv("ETH_NODE_ADDRESS")),
		WebPublicURL:          strings.TrimRight(os.Getenv("WEB_PUBLIC_URL"), "/"),
		MasterKeyAdmin:        getenvBool("MASTER_KEY_ADMIN", false),
		Admin2FARequired:      getenvBool("ADMIN_2FA_REQUIRED", false),
		DevHTTP:               getenvBool("UPLINK_DEV_HTTP", false),
		TrustedProxyCIDRs:     splitCSV(os.Getenv("TRUSTED_PROXY_CIDR")),
		Reset2FA:              getenvBool("UPLINK_RESET_2FA", false),
	}

	if err := checkAllowancePaths(c); err != nil {
		return nil, err
	}

	auth, authSrc, err := loadRouterAuth(c)
	if err != nil {
		return nil, err
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
		missing = append(missing, "ROUTER_AUTH_FILE or ROUTER_AUTH (user:password)")
	}
	if err := requireNonNegativeWei("UPLINK_MOR_ALLOWANCE_WEI", c.UplinkMORAllowanceWei); err != nil {
		return nil, err
	}
	c.UplinkMORAllowanceWei = normalizeWei(c.UplinkMORAllowanceWei)
	if err := loadMORAllowance(c); err != nil {
		return nil, err
	}
	loadRouterWallet(c)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if strings.EqualFold(c.RouterUser, "admin") {
		return nil, fmt.Errorf("router user %q refused: use scoped ROUTER_AUTH (not admin); source=%s", c.RouterUser, authSrc)
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

// loadMORAllowance reads the init-written allowance file (if present) and
// fills MORAllowance* fields. Env override is used when the file is absent.
func loadMORAllowance(c *Config) error {
	path := c.MORAllowanceFile
	if path == "" && c.RouterAuthFile != "" {
		if strings.HasSuffix(c.RouterAuthFile, ".auth") {
			path = strings.TrimSuffix(c.RouterAuthFile, ".auth") + ".mor-allowance.json"
		} else {
			path = c.RouterAuthFile + ".mor-allowance.json"
		}
		c.MORAllowanceFile = path
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			var meta struct {
				Wei     string `json:"wei"`
				Source  string `json:"source"`
				Warning string `json:"warning"`
			}
			if json.Unmarshal(b, &meta) != nil {
				return fmt.Errorf("UPLINK_MOR_ALLOWANCE_FILE %s: invalid JSON", path)
			}
			c.MORAllowanceWei = strings.TrimSpace(meta.Wei)
			c.MORAllowanceSource = strings.TrimSpace(meta.Source)
			c.MORAllowanceWarning = strings.TrimSpace(meta.Warning)
			if c.MORAllowanceSource == "" {
				c.MORAllowanceSource = "unknown"
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("UPLINK_MOR_ALLOWANCE_FILE %s: %w", path, err)
		}
	}
	// No file: surface env override (including "0") for status/Security.
	// Mirrors router-user-init.sh: set (all digits) = explicit cap, 0 = zero.
	if c.UplinkMORAllowanceWei == "" {
		c.MORAllowanceSource = "unknown"
		return nil
	}
	c.MORAllowanceWei = c.UplinkMORAllowanceWei
	if c.UplinkMORAllowanceWei == "0" {
		c.MORAllowanceSource = "zero"
		c.MORAllowanceWarning = "MOR allowance is zero (UPLINK_MOR_ALLOWANCE_WEI=0); sessions will not open until the cap is raised and Uplink is restarted."
	} else {
		c.MORAllowanceSource = "override"
	}
	return nil
}

// loadRouterWallet reads the init-recorded wallet address, best effort.
// Only a well-formed 0x + 40 hex address is accepted.
func loadRouterWallet(c *Config) {
	path := c.RouterWalletFile
	if path == "" && c.RouterAuthFile != "" {
		dir := c.RouterAuthFile
		if i := strings.LastIndex(dir, "/"); i >= 0 {
			dir = dir[:i]
		} else {
			dir = "."
		}
		path = dir + "/router.wallet.json"
		c.RouterWalletFile = path
	}
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var meta struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return
	}
	if IsWalletAddress(meta.Address) {
		c.RouterWalletAddress = meta.Address
	}
}

// IsWalletAddress reports whether s is 0x followed by 40 hex digits.
func IsWalletAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, r := range s[2:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// loadRouterAuth prefers ROUTER_AUTH_FILE, then ROUTER_AUTH env.
// COOKIE_CONTENT is accepted only when UPLINK_DEV_HTTP is true (local compose).
func loadRouterAuth(c *Config) (auth, source string, err error) {
	if c.RouterAuthFile != "" {
		b, readErr := os.ReadFile(c.RouterAuthFile)
		if readErr != nil {
			return "", "ROUTER_AUTH_FILE", fmt.Errorf("ROUTER_AUTH_FILE %s: %w", c.RouterAuthFile, readErr)
		}
		auth = strings.TrimSpace(string(b))
		if auth != "" {
			return auth, "ROUTER_AUTH_FILE", nil
		}
	}
	if v := strings.TrimSpace(os.Getenv("ROUTER_AUTH")); v != "" {
		return v, "ROUTER_AUTH", nil
	}
	if c.DevHTTP {
		if v := strings.TrimSpace(os.Getenv("COOKIE_CONTENT")); v != "" {
			return v, "COOKIE_CONTENT", nil
		}
	}
	return "", "", nil
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

// requireNonNegativeWei validates optional wei: empty OK; when set must be
// a base-10 integer >= 0 (zero allowed for fail-closed boot).
func requireNonNegativeWei(name, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("%s must be >= 0 (got %s)", name, raw)
	}
	// Digits only, same rule as router-user-init.sh (no sign, no separators).
	for _, r := range raw {
		if r < '0' || r > '9' {
			return fmt.Errorf("%s must be a base-10 integer (wei)", name)
		}
	}
	return nil
}

// normalizeWei strips leading zeros from a validated digit string ("000" -> "0").
// Empty stays empty (= unset: init uses wallet balance).
func normalizeWei(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return raw
	}
	return n.String()
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

// checkAllowancePaths refuses an allowance status file or control dir under
// DATA_DIR: those are separate volumes shared with the allowance manager and
// must never overlap Uplink's private data.
func checkAllowancePaths(c *Config) error {
	if c.AllowancePollSec <= 0 {
		c.AllowancePollSec = 60
	}
	data, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil
	}
	for name, p := range map[string]string{
		"UPLINK_ALLOWANCE_STATUS_FILE": c.AllowanceStatusFile,
		"UPLINK_ALLOWANCE_CTL_DIR":     c.AllowanceCtlDir,
	} {
		if p == "" {
			continue
		}
		ap, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if PathWithin(data, ap) {
			return fmt.Errorf("%s %s must not be under DATA_DIR %s", name, p, c.DataDir)
		}
	}
	return nil
}

// PathWithin reports whether p is dir itself or inside it (lexically).
func PathWithin(dir, p string) bool {
	dir = filepath.Clean(dir)
	p = filepath.Clean(p)
	if p == dir {
		return true
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
