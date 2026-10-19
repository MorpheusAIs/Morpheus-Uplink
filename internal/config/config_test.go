package config

import "testing"

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_PASSWORD", "test-admin-pass-ok")
	t.Setenv("API_KEY_SEED", "seed")
	// Clear auth vars so a developer's exported shell/.env cannot leak in.
	t.Setenv("ROUTER_AUTH", "")
	t.Setenv("COOKIE_CONTENT", "")
	t.Setenv("ROUTER_AUTH_FILE", "")
	t.Setenv("UPLINK_MOR_ALLOWANCE_WEI", "1000000000000000000")
}

func TestRouterAuthPrimary(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	t.Setenv("COOKIE_CONTENT", "other:value")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterUser != "admin" || c.RouterPass != "secret" {
		t.Fatalf("ROUTER_AUTH should win: got %s:%s", c.RouterUser, c.RouterPass)
	}
}

func TestCookieContentFallback(t *testing.T) {
	setRequired(t)
	t.Setenv("COOKIE_CONTENT", "admin:cookiepass")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterUser != "admin" || c.RouterPass != "cookiepass" {
		t.Fatalf("COOKIE_CONTENT fallback failed: got %s:%s", c.RouterUser, c.RouterPass)
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
	t.Setenv("COOKIE_CONTENT", "admin:x")
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
	// ROUTER_AUTH wins for router creds, but cookie equality still checked
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected refuse when ADMIN_PASSWORD equals COOKIE_CONTENT password")
	}
}
