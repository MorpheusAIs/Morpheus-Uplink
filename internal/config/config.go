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
	// URL of the on-chain model catalog JSON.
	ActiveModelsURL string
	// Session behavior.
	SessionDurationSec int
	SessionFailover    bool
	DirectPayment      bool
	// Close pooled sessions on shutdown to recover stake sooner.
	CloseSessionsOnExit bool
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen:              getenv("UPLINK_LISTEN", ":8080"),
		RouterURL:           strings.TrimRight(getenv("ROUTER_URL", "http://proxy-router:8082"), "/"),
		AdminPassword:       os.Getenv("ADMIN_PASSWORD"),
		APIKeySeed:          os.Getenv("API_KEY_SEED"),
		DataDir:             getenv("DATA_DIR", "./data"),
		ActiveModelsURL:     getenv("ACTIVE_MODELS_URL", "https://active.mor.org/active_models.json"),
		SessionDurationSec:  getenvInt("SESSION_DURATION_SECONDS", 3600),
		SessionFailover:     getenvBool("SESSION_FAILOVER", true),
		DirectPayment:       getenvBool("SESSION_DIRECT_PAYMENT", false),
		CloseSessionsOnExit: getenvBool("CLOSE_SESSIONS_ON_EXIT", true),
	}

	// ROUTER_AUTH mirrors the proxy-router's COOKIE_CONTENT format.
	auth := getenv("ROUTER_AUTH", "")
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
		missing = append(missing, "ROUTER_AUTH (user:password)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return c, nil
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
