package config

import (
	"strings"
	"testing"
)

// Gateway feed URLs: https, or http only to an exact loopback host name;
// never userinfo. The Load error names the variable, never the value.
func TestFeedURLMustBeHTTPS(t *testing.T) {
	accept := []string{
		"https://feeds.example.invalid/gateway_models.json",
		"http://127.0.0.1:8082/gateway_bids.json",
		"http://[::1]:8082/x.json",
		"http://localhost/x.json",
		"http://LOCALHOST:8082/x.json",
	}
	reject := []string{
		"http://feeds.example.invalid/x.json",
		"http://localhost.attacker.example/x.json",
		"http://127.0.0.1.nip.io/x.json",
		"http://127.0.0.1@evil.example/x.json",
		"http://2130706433/x.json",
		"http://0x7f.1/x.json",
		"http://127.0.0.2/x.json",
		"https://user:pass@example.invalid/x.json",
		"ftp:",
		"https:///x.json",
	}
	for _, name := range []string{"ACTIVE_MODELS_URL", "GATEWAY_BIDS_URL"} {
		for _, u := range accept {
			setRequired(t)
			setFeedURLs(t)
			t.Setenv(name, u)
			if _, err := FromEnv(); err != nil {
				t.Errorf("%s=%s rejected: %v", name, u, err)
			}
		}
		for _, u := range reject {
			setRequired(t)
			setFeedURLs(t)
			t.Setenv(name, u)
			_, err := FromEnv()
			if err == nil {
				t.Errorf("%s=%s accepted", name, u)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%s: error does not name the variable: %v", name, u, err)
			}
			if strings.Contains(err.Error(), u) {
				t.Errorf("%s=%s: error echoes the value: %v", name, u, err)
			}
		}
	}
}

func setFeedURLs(t *testing.T) {
	t.Helper()
	t.Setenv("ROUTER_AUTH", "uplink:routerpass")
	t.Setenv("ACTIVE_MODELS_URL", "https://feeds.example.invalid/gateway_models.json")
	t.Setenv("GATEWAY_BIDS_URL", "https://feeds.example.invalid/gateway_bids.json")
}
