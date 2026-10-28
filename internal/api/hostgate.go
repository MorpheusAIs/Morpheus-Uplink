package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// consoleHostAllowed reports whether a request for the console (/admin*,
// /gui*, /node/*) may be served on the Host it was sent to. WEB_PUBLIC_URL is the
// only source of the expected host; when it is unset every host is
// allowed, as before. Only r.Host is read: X-Forwarded-Host and Forwarded
// are client-controllable and are ignored.
//
// Matching rules:
//   - host names compare case-insensitively, ignoring one trailing dot;
//   - WEB_PUBLIC_URL without a port (or with the scheme's default port,
//     443 for https, 80 for http) accepts a request Host with no port or
//     with that default port;
//   - WEB_PUBLIC_URL with a non-default port accepts only a request Host
//     carrying exactly that port (a Host with no port, or any other port,
//     is refused);
//   - a WEB_PUBLIC_URL that does not parse to a host refuses every host.
func consoleHostAllowed(webPublicURL, reqHost string) bool {
	raw := strings.TrimSpace(webPublicURL)
	if raw == "" {
		return true
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Hostname() == "" {
		return false
	}
	def := defaultPort(u.Scheme)
	wantHost, wantPort := normHost(u.Hostname()), u.Port()
	if wantPort == def {
		wantPort = ""
	}
	gotHost, gotPort, ok := splitRequestHost(reqHost)
	if !ok || gotHost != wantHost {
		return false
	}
	if wantPort == "" {
		return gotPort == "" || (def != "" && gotPort == def)
	}
	return gotPort == wantPort
}

// splitRequestHost splits a Host header value into a normalised host name
// and port ("" when absent). IPv6 literals must be bracketed.
func splitRequestHost(h string) (host, port string, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", "", false
	}
	if hh, pp, err := net.SplitHostPort(h); err == nil {
		if pp == "" {
			return "", "", false
		}
		return normHost(hh), pp, hh != ""
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	} else if strings.Contains(h, ":") {
		return "", "", false
	}
	return normHost(h), "", h != ""
}

func normHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// isConsolePath is true for /admin, /admin/*, /gui, /gui/* and the raw
// router passthrough /node/* (there is no /node route), on either the
// cleaned or the raw request path.
func isConsolePath(r *http.Request) bool {
	for _, p := range []string{r.URL.Path, rawRequestPath(r)} {
		if p == "/admin" || strings.HasPrefix(p, "/admin/") || p == "/gui" || strings.HasPrefix(p, "/gui/") ||
			strings.HasPrefix(p, "/node/") {
			return true
		}
	}
	return false
}
