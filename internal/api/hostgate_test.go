package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestConsoleHostAllowed(t *testing.T) {
	cases := []struct {
		web, host string
		want      bool
	}{
		// unset: unchanged, every host allowed
		{"", "anything.example", true},
		{"", "203.0.113.7", true},
		// matching host, case-insensitive, trailing dot, default port
		{"https://console.example.org", "console.example.org", true},
		{"https://Console.Example.ORG", "CONSOLE.example.org", true},
		{"https://console.example.org/", "console.example.org.", true},
		{"https://console.example.org.", "console.example.org", true},
		{"https://console.example.org", "console.example.org:443", true},
		{"https://console.example.org:443", "console.example.org", true},
		{"http://console.example.org", "console.example.org:80", true},
		// default port of the other scheme is a mismatch
		{"https://console.example.org", "console.example.org:80", false},
		{"https://console.example.org", "console.example.org:8443", false},
		// explicit non-default port: exactly that port only
		{"https://console.example.org:8443", "console.example.org:8443", true},
		{"https://console.example.org:8443", "console.example.org", false},
		{"https://console.example.org:8443", "console.example.org:443", false},
		{"https://console.example.org:8443", "console.example.org:9443", false},
		// wrong host, look-alikes, bare IPs
		{"https://console.example.org", "other.example.org", false},
		{"https://console.example.org", "console.example.org.evil.test", false},
		{"https://console.example.org", "evilconsole.example.org", false},
		{"https://console.example.org", "203.0.113.7", false},
		{"https://console.example.org", "203.0.113.7:443", false},
		{"https://console.example.org", "[2001:db8::1]", false},
		{"https://console.example.org", "[2001:db8::1]:443", false},
		{"https://console.example.org", "2001:db8::1", false},
		{"https://console.example.org", "", false},
		{"https://console.example.org", ":443", false},
		{"https://console.example.org", "console.example.org:", false},
		// an IP origin (dev) matches itself only
		{"http://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"http://127.0.0.1:8080", "localhost:8080", false},
		{"http://[::1]:8080", "[::1]:8080", true},
		// unparseable origin refuses every host
		{"https://", "console.example.org", false},
	}
	for _, c := range cases {
		if got := consoleHostAllowed(c.web, c.host); got != c.want {
			t.Errorf("consoleHostAllowed(%q, %q) = %v, want %v", c.web, c.host, got, c.want)
		}
	}
}

// hostReq sends path to ts with the given Host header and extra headers.
func hostReq(t *testing.T, base, path, host string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestConsoleHostGateRoutes(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.WebPublicURL = "https://console.example.org"
	const good = "console.example.org"

	// matching host passes (auth still applies after the host check)
	for _, h := range []string{good, "CONSOLE.Example.org", good + ":443", good + "."} {
		if code, _ := hostReq(t, ts.URL, "/gui/", h, nil); code != http.StatusOK {
			t.Errorf("/gui/ on %q: %d, want 200", h, code)
		}
		if code, _ := hostReq(t, ts.URL, "/admin/status", h, nil); code != http.StatusUnauthorized {
			t.Errorf("/admin/status on %q: %d, want 401", h, code)
		}
	}

	// wrong host, bare IPs, port mismatch: plain 404 before auth, no hint
	bad := []string{"other.example.org", "203.0.113.7", "203.0.113.7:443", "[2001:db8::1]", "[2001:db8::1]:443", good + ":8443", good + ":80"}
	paths := []string{"/admin", "/admin/", "/admin/status", "/admin/login", "/admin/2fa/status", "/gui", "/gui/", "/gui/index.html"}
	for _, h := range bad {
		for _, p := range paths {
			code, body := hostReq(t, ts.URL, p, h, nil)
			if code != http.StatusNotFound {
				t.Errorf("%s on %q: %d, want 404", p, h, code)
			}
			if strings.Contains(strings.ToLower(body), "console.example") {
				t.Errorf("%s on %q: 404 body names the expected host: %q", p, h, body)
			}
		}
	}

	// forwarded-host spoofs are ignored: only r.Host counts
	spoof := map[string]string{"X-Forwarded-Host": good, "Forwarded": "host=" + good, "X-Forwarded-Proto": "https"}
	if code, _ := hostReq(t, ts.URL, "/gui/", "other.example.org", spoof); code != http.StatusNotFound {
		t.Errorf("spoofed forwarded host on a wrong Host: %d, want 404", code)
	}
	if code, _ := hostReq(t, ts.URL, "/gui/", good, map[string]string{"X-Forwarded-Host": "other.example.org", "Forwarded": "host=other.example.org"}); code != http.StatusOK {
		t.Errorf("forwarded host on the right Host: %d, want 200", code)
	}

	// /v1 and /healthcheck are not host-checked
	if code, _ := hostReq(t, ts.URL, "/v1/models", "203.0.113.7", nil); code != http.StatusUnauthorized {
		t.Errorf("/v1/models on a bare IP: %d, want 401 (unchanged)", code)
	}
	if code, _ := hostReq(t, ts.URL, "/healthcheck", "other.example.org", nil); code != http.StatusOK {
		t.Errorf("/healthcheck on another host: %d, want 200 (unchanged)", code)
	}
}

func TestConsoleHostGateExplicitPort(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.WebPublicURL = "https://console.example.org:8443"
	if code, _ := hostReq(t, ts.URL, "/gui/", "console.example.org:8443", nil); code != http.StatusOK {
		t.Errorf("explicit port match: %d, want 200", code)
	}
	for _, h := range []string{"console.example.org", "console.example.org:443", "console.example.org:9443"} {
		if code, _ := hostReq(t, ts.URL, "/gui/", h, nil); code != http.StatusNotFound {
			t.Errorf("explicit port, Host %q: %d, want 404", h, code)
		}
	}
}

func TestConsoleHostGateUnset(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.WebPublicURL = ""
	for _, h := range []string{"other.example.org", "203.0.113.7", "[2001:db8::1]:8080"} {
		if code, _ := hostReq(t, ts.URL, "/gui/", h, nil); code != http.StatusOK {
			t.Errorf("unset, /gui/ on %q: %d, want 200", h, code)
		}
		if code, _ := hostReq(t, ts.URL, "/admin/status", h, nil); code != http.StatusUnauthorized {
			t.Errorf("unset, /admin/status on %q: %d, want 401", h, code)
		}
	}
}
