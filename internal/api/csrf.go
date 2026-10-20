package api

import (
	"net/http"
	"net/url"
	"strings"
)

// checkCSRF enforces Sec-Fetch-Site=same-origin and Origin == WEB_PUBLIC_URL
// for non-safe methods when using cookie auth. Returns false → caller sends 403.
func (s *Server) checkCSRF(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || s.cfg.WebPublicURL == "" {
		return false
	}
	return originsMatch(origin, s.cfg.WebPublicURL)
}

func originsMatch(origin, webPublic string) bool {
	o, err1 := url.Parse(strings.TrimRight(origin, "/"))
	w, err2 := url.Parse(strings.TrimRight(webPublic, "/"))
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(o.Scheme, w.Scheme) && strings.EqualFold(o.Host, w.Host)
}

// webPublicOrigin is WEB_PUBLIC_URL as lowercase scheme://host ("" when
// unset or unparseable), for the GUI's origin warning.
func webPublicOrigin(raw string) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}
