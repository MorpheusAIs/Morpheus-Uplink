package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_PASSWORD", "test-admin-pass-ok")
	t.Setenv("API_KEY_SEED", "seed")
	t.Setenv("ROUTER_AUTH", "")
	t.Setenv("COOKIE_CONTENT", "")
	t.Setenv("ROUTER_AUTH_FILE", "")
	t.Setenv("UPLINK_DEV_HTTP", "")
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "")
	t.Setenv("UPLINK_MOR_ALLOWANCE_FILE", "")
}

func TestRouterAuthFilePreferred(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "router.auth")
	if err := os.WriteFile(path, []byte("uplink:fromfile\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_AUTH_FILE", path)
	t.Setenv("ROUTER_AUTH", "uplink:fromenv")
	t.Setenv("COOKIE_CONTENT", "admin:cookie")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterUser != "uplink" || c.RouterPass != "fromfile" {
		t.Fatalf("file should win: got %s:%s", c.RouterUser, c.RouterPass)
	}
}

func TestRouterAuthEnvWhenNoFile(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterUser != "uplink" || c.RouterPass != "secret" {
		t.Fatalf("got %s:%s", c.RouterUser, c.RouterPass)
	}
}

func TestCookieContentOnlyWithDevHTTP(t *testing.T) {
	setRequired(t)
	t.Setenv("COOKIE_CONTENT", "uplink:cookiepass")
	if _, err := FromEnv(); err == nil {
		t.Fatal("COOKIE_CONTENT without DevHTTP must not boot")
	}
	t.Setenv("UPLINK_DEV_HTTP", "true")
	t.Setenv("WEB_PUBLIC_URL", "http://127.0.0.1:8080")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterUser != "uplink" || c.RouterPass != "cookiepass" {
		t.Fatalf("dev fallback: got %s:%s", c.RouterUser, c.RouterPass)
	}
}

func TestRefuseRouterAdminUser(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected admin refuse, got %v", err)
	}
}

func TestMissingAuth(t *testing.T) {
	setRequired(t)
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when neither ROUTER_AUTH nor ROUTER_AUTH_FILE is set")
	}
}

func TestCatalogURLDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:x")
	t.Setenv("ACTIVE_MODELS_URL", "")
	t.Setenv("GATEWAY_BIDS_URL", "")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	wantModels := "https://active.mor.org/gateway_models.json"
	wantBids := "https://active.mor.org/gateway_bids.json"
	if c.ActiveModelsURL != wantModels {
		t.Fatalf("ActiveModelsURL default: got %q want %q", c.ActiveModelsURL, wantModels)
	}
	if c.GatewayBidsURL != wantBids {
		t.Fatalf("GatewayBidsURL default: got %q want %q", c.GatewayBidsURL, wantBids)
	}
}

func TestAllowanceOptional(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("missing allowance env must boot: %v", err)
	}
	if c.UplinkMORAllowanceWei != "" {
		t.Fatalf("expected empty override, got %q", c.UplinkMORAllowanceWei)
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "0")
	c, err = FromEnv()
	if err != nil {
		t.Fatalf("zero allowance env must boot: %v", err)
	}
	if c.MORAllowanceWei != "0" || c.MORAllowanceSource != "zero" {
		t.Fatalf("zero env: wei=%q source=%q", c.MORAllowanceWei, c.MORAllowanceSource)
	}
	if c.MORAllowanceWarning == "" {
		t.Fatal("expected warning for zero allowance")
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "1000000000000000000")
	c, err = FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceSource != "override" || c.MORAllowanceWei != "1000000000000000000" {
		t.Fatalf("override: wei=%q source=%q", c.MORAllowanceWei, c.MORAllowanceSource)
	}
}

func TestAllowanceInvalid(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "not-a-number")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for non-integer wei")
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for negative wei")
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "+5")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for signed wei (init rejects non-digits)")
	}
	// Internal whitespace is rejected (init rejects it too; never "1 000" -> 1000).
	for _, raw := range []string{"1 000", "12\t34"} {
		t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", raw)
		if _, err := FromEnv(); err == nil {
			t.Fatalf("expected error for internal whitespace %q", raw)
		}
	}
}

// Explicit 0 (any zero padding) is a zero cap, never "use balance".
func TestAllowanceZeroOverrideNormalized(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")
	for _, raw := range []string{"0", "000", " 0 "} {
		t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", raw)
		c, err := FromEnv()
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if c.UplinkMORAllowanceWei != "0" || c.MORAllowanceWei != "0" || c.MORAllowanceSource != "zero" {
			t.Fatalf("%q: override=%q wei=%q source=%q", raw, c.UplinkMORAllowanceWei, c.MORAllowanceWei, c.MORAllowanceSource)
		}
		if c.MORAllowanceWarning == "" {
			t.Fatalf("%q: expected zero warning", raw)
		}
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "00123")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceWei != "123" || c.MORAllowanceSource != "override" {
		t.Fatalf("padded: wei=%q source=%q", c.MORAllowanceWei, c.MORAllowanceSource)
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "")
	c, err = FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceSource != "unknown" {
		t.Fatalf("empty env, no file: source=%q want unknown", c.MORAllowanceSource)
	}
}

func TestAllowanceFileLoad(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "router.auth")
	if err := os.WriteFile(auth, []byte("uplink:fromfile\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	allowPath := filepath.Join(dir, "router.mor-allowance.json")
	meta := `{"wei":"5000000000000000000","source":"balance","warning":""}`
	if err := os.WriteFile(allowPath, []byte(meta), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_AUTH_FILE", auth)
	// Env override must not replace file metadata for status.
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "999")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceWei != "5000000000000000000" || c.MORAllowanceSource != "balance" {
		t.Fatalf("file should win: wei=%q source=%q", c.MORAllowanceWei, c.MORAllowanceSource)
	}
	if c.MORAllowanceFile != allowPath {
		t.Fatalf("derived path: got %q want %q", c.MORAllowanceFile, allowPath)
	}
}

func TestAllowanceFileZeroWarning(t *testing.T) {
	setRequired(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "router.auth")
	if err := os.WriteFile(auth, []byte("uplink:fromfile\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	allowPath := filepath.Join(dir, "router.mor-allowance.json")
	warn := "MOR wallet balance is zero; sessions will not open until funded and Uplink is restarted."
	meta := `{"wei":"0","source":"zero","warning":"` + warn + `"}`
	if err := os.WriteFile(allowPath, []byte(meta), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_AUTH_FILE", auth)
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceWei != "0" || c.MORAllowanceSource != "zero" {
		t.Fatalf("got wei=%q source=%q", c.MORAllowanceWei, c.MORAllowanceSource)
	}
	if c.MORAllowanceWarning != warn {
		t.Fatalf("warning: got %q", c.MORAllowanceWarning)
	}
}

func TestRefuseWeakAdminPassword(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")
	t.Setenv("ADMIN_PASSWORD", "password")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected weak password refuse")
	}
	t.Setenv("ADMIN_PASSWORD", "test-admin-pass-ok")
	t.Setenv("COOKIE_CONTENT", "admin:test-admin-pass-ok")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected refuse when ADMIN_PASSWORD equals COOKIE_CONTENT password")
	}
}
