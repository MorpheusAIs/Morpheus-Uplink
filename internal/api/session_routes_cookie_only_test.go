package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

// sessionOnlyRoutes are the routes wrapped by requireSession (server.go).
var sessionOnlyRoutes = []struct{ method, path string }{
	{http.MethodPost, "/admin/logout"},
	{http.MethodGet, "/admin/2fa/status"},
	{http.MethodPost, "/admin/2fa/enroll/start"},
	{http.MethodPost, "/admin/2fa/enroll/confirm"},
}

func doReq(t *testing.T, c *http.Client, ts *httptest.Server, cfg *config.Config, method, path, bearer string) (int, string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, _ := http.NewRequest(method, ts.URL+path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	withCSRF(req, cfg)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func countAudit(evs []AuditEvent, kind string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Session-only admin routes require a cookie session: the master key as a
// Bearer token gets the same 401 as an unauthenticated request, whatever
// MASTER_KEY_ADMIN says, and writes no admin_key_use record.
func TestSessionRoutesRefuseMasterKeyBearer(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "MASTER_KEY_ADMIN=false"
		if on {
			name = "MASTER_KEY_ADMIN=true"
		}
		t.Run(name, func(t *testing.T) {
			ts, cfg, _ := newTestServer(t)
			cfg.MasterKeyAdmin = on
			master := keymaker.MasterKey(cfg.APIKeySeed)
			ac := adminClient(t, ts, cfg)

			for _, rt := range sessionOnlyRoutes {
				wantCode, wantBody := doReq(t, http.DefaultClient, ts, cfg, rt.method, rt.path, "")
				if wantCode != http.StatusUnauthorized {
					t.Fatalf("%s %s unauthenticated: %d, want 401", rt.method, rt.path, wantCode)
				}
				code, body := doReq(t, http.DefaultClient, ts, cfg, rt.method, rt.path, master)
				if code != wantCode || body != wantBody {
					t.Fatalf("%s %s master Bearer: %d %q, want %d %q", rt.method, rt.path, code, body, wantCode, wantBody)
				}
				// A Bearer header alongside a valid cookie session is refused too.
				code, body = doReq(t, ac, ts, cfg, rt.method, rt.path, master)
				if code != wantCode || body != wantBody {
					t.Fatalf("%s %s cookie+master Bearer: %d %q, want %d %q", rt.method, rt.path, code, body, wantCode, wantBody)
				}
			}

			// The cookie session is untouched (logout did not run) and no
			// admin_key_use or session_logout record was written.
			if code, b := doReq(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", ""); code != http.StatusOK {
				t.Fatalf("cookie 2fa status after refusals: %d %s", code, b)
			}
			evs := auditEvents(t, ac, ts)
			if n := countAudit(evs, "admin_key_use"); n != 0 {
				t.Fatalf("admin_key_use records: %d, want 0 (%#v)", n, evs)
			}
			if n := countAudit(evs, "session_logout"); n != 0 {
				t.Fatalf("session_logout records: %d, want 0", n)
			}
			// enroll/start did not run: there is no pending enrollment.
			code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", `{"code":"000000"}`)
			if code != http.StatusBadRequest || !strings.Contains(string(b), "no pending enrollment") {
				t.Fatalf("cookie confirm after refusals: %d %s, want 400 no pending enrollment", code, b)
			}
		})
	}
}

// A real admin cookie session still works on every session-only route.
func TestSessionRoutesCookieSessionWorks(t *testing.T) {
	for _, on := range []bool{false, true} {
		ts, cfg, _ := newTestServer(t)
		cfg.MasterKeyAdmin = on
		ac := adminClient(t, ts, cfg)

		if code, b := doReq(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", ""); code != http.StatusOK {
			t.Fatalf("2fa status: %d %s", code, b)
		}
		// enroll/start + enroll/confirm over the cookie session.
		enroll2FA(t, ac, ts, cfg)
		if code, b := doReq(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", ""); code != http.StatusOK || !strings.Contains(b, `"enrolled":true`) {
			t.Fatalf("2fa status after enroll: %d %s", code, b)
		}
		if code, b := doReq(t, ac, ts, cfg, http.MethodPost, "/admin/logout", ""); code != http.StatusOK {
			t.Fatalf("logout: %d %s", code, b)
		}
		if code, _ := doReq(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", ""); code != http.StatusUnauthorized {
			t.Fatalf("2fa status after logout: %d, want 401", code)
		}
	}
}

// requireAdmin is unchanged: with MASTER_KEY_ADMIN=true the master Bearer is
// accepted and writes admin_key_use; with it false the Bearer gets 401.
func TestRequireAdminMasterKeyBearerUnchanged(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	master := keymaker.MasterKey(cfg.APIKeySeed)
	ac := adminClient(t, ts, cfg)

	cfg.MasterKeyAdmin = false
	if code, _ := doReq(t, http.DefaultClient, ts, cfg, http.MethodGet, "/admin/status", master); code != http.StatusUnauthorized {
		t.Fatalf("master Bearer /admin/status (off): %d, want 401", code)
	}
	if n := countAudit(auditEvents(t, ac, ts), "admin_key_use"); n != 0 {
		t.Fatalf("admin_key_use with MASTER_KEY_ADMIN=false: %d, want 0", n)
	}

	cfg.MasterKeyAdmin = true
	for _, p := range []string{"/admin/status", "/admin/keys"} {
		if code, b := doReq(t, http.DefaultClient, ts, cfg, http.MethodGet, p, master); code != http.StatusOK {
			t.Fatalf("master Bearer %s (on): %d %s", p, code, b)
		}
	}
	evs := auditEvents(t, ac, ts)
	for _, p := range []string{"/admin/status", "/admin/keys"} {
		if !hasAudit(evs, "admin_key_use", "ok", p) {
			t.Fatalf("missing admin_key_use ok for %s: %#v", p, evs)
		}
	}
}
