package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_PASSWORD", "test-admin-pass-ok")
	t.Setenv("API_KEY_SEED", "seed")
	t.Setenv("ROUTER_AUTH", "")
	t.Setenv("COOKIE_CONTENT", "")
	t.Setenv("UPLINK_DEV_HTTP", "")
}

func TestRouterAuthEnv(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	t.Setenv("COOKIE_CONTENT", "admin:cookie")

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
		t.Fatalf("got %s:%s", c.RouterUser, c.RouterPass)
	}
}

func TestMissingAuth(t *testing.T) {
	setRequired(t)
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when neither ROUTER_AUTH nor COOKIE_CONTENT is set")
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

func TestProviderCooldownFallback(t *testing.T) {
	for v, want := range map[string]int{"": 600, "0": 0, "45": 45, "-5": 600, "ten": 600} {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "admin:secret")
		t.Setenv("UPLINK_PROVIDER_COOLDOWN_SECONDS", v)
		var buf bytes.Buffer
		log.SetOutput(&buf)
		c, err := FromEnv()
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		if c.ProviderCooldownSec != want {
			t.Errorf("%q: got %d, want %d", v, c.ProviderCooldownSec, want)
		}
		warned := strings.Contains(buf.String(), "UPLINK_PROVIDER_COOLDOWN_SECONDS")
		if bad := v == "-5" || v == "ten"; warned != bad {
			t.Errorf("%q: warned=%v", v, warned)
		}
		if strings.Contains(buf.String(), "ten") || strings.Contains(buf.String(), "-5") {
			t.Errorf("%q: warning carries the value: %s", v, buf.String())
		}
	}
}
