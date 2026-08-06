package config

import "testing"

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("API_KEY_SEED", "seed")
	// Clear auth vars so a developer's exported shell/.env cannot leak in.
	t.Setenv("ROUTER_AUTH", "")
	t.Setenv("COOKIE_CONTENT", "")
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
		t.Fatal("expected error when neither ROUTER_AUTH nor COOKIE_CONTENT is set")
	}
}
