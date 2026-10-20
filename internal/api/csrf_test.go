package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

func newCSRFServer(webPublic string) *Server {
	return &Server{cfg: &config.Config{WebPublicURL: webPublic}}
}

func TestCSRFRequiresBothHeaders(t *testing.T) {
	s := newCSRFServer("https://uplink.example")
	cases := []struct {
		name   string
		site   string
		origin string
		want   bool
	}{
		{"both ok", "same-origin", "https://uplink.example", true},
		{"trailing slash on configured URL", "same-origin", "https://uplink.example/", true},
		{"site missing", "", "https://uplink.example", false},
		{"origin missing", "same-origin", "", false},
		{"both missing", "", "", false},
		{"foreign origin", "same-origin", "https://evil.example", false},
		{"cross-site", "cross-site", "https://uplink.example", false},
		{"same-site", "same-site", "https://uplink.example", false},
		{"unset web public", "same-origin", "https://uplink.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := s
			if tc.name == "unset web public" {
				srv = newCSRFServer("")
			}
			if tc.name == "trailing slash on configured URL" {
				srv = newCSRFServer("https://uplink.example/")
			}
			req := httptest.NewRequest(http.MethodPost, "/admin/keys", nil)
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := srv.checkCSRF(req); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestCSRFSafeMethodsSkip(t *testing.T) {
	s := newCSRFServer("https://uplink.example")
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(m, "/admin/status", nil)
		if !s.checkCSRF(req) {
			t.Fatalf("%s should skip CSRF", m)
		}
	}
}
