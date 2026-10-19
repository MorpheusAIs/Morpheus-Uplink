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
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "1000000000000000000")
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

func TestAllowanceRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "uplink:secret")
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when UPLINK_MOR_ALLOWANCE_WEI missing")
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "0")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when UPLINK_MOR_ALLOWANCE_WEI is zero")
	}
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "1000000000000000000")
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
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
