package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/pquerna/otp/totp"
)

func postJSON(t *testing.T, c *http.Client, ts *httptest.Server, cfg *config.Config, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withCSRF(req, cfg)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func auditEvents(t *testing.T, c *http.Client, ts *httptest.Server) []AuditEvent {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/audit", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Events []AuditEvent `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Events
}

func hasAudit(evs []AuditEvent, kind, result, msgSuffix string) bool {
	for _, e := range evs {
		if e.Kind == kind && e.Result == result && strings.HasSuffix(e.Message, msgSuffix) {
			return true
		}
	}
	return false
}

// Wrong password / wrong code on 2FA remove and recovery regen must be
// audited on every attempt, not only when the lockout trips.
func TestTOTPRemoveRegenAuditFailures(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)

	// Enroll so the second-factor branch is reachable.
	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", "{}")
	if code != http.StatusOK {
		t.Fatalf("enroll start: %d %s", code, b)
	}
	var start struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(b, &start); err != nil {
		t.Fatal(err)
	}
	otpCode, err := totp.GenerateCode(start.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, b = postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", `{"code":"`+otpCode+`"}`)
	if code != http.StatusOK {
		t.Fatalf("enroll confirm: %d %s", code, b)
	}

	const wrongPW = "definitely-wrong-password-xyz"
	for _, tc := range []struct{ path, kind string }{
		{"/admin/2fa/remove", "2fa_remove"},
		{"/admin/2fa/recovery/regen", "recovery_codes_regen"},
	} {
		code, _ = postJSON(t, ac, ts, cfg, tc.path, `{"password":"`+wrongPW+`","code":"000000"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s wrong pw: %d want 401", tc.path, code)
		}
		code, _ = postJSON(t, ac, ts, cfg, tc.path, `{"password":"`+cfg.AdminPassword+`","code":"NOTACODE"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s wrong code: %d want 401", tc.path, code)
		}
		evs := auditEvents(t, ac, ts)
		if !hasAudit(evs, tc.kind, "fail", "bad_password") {
			t.Fatalf("%s: missing bad_password audit: %+v", tc.kind, evs)
		}
		if !hasAudit(evs, tc.kind, "fail", "bad_totp") {
			t.Fatalf("%s: missing bad second-factor audit: %+v", tc.kind, evs)
		}
		for _, e := range evs {
			if strings.Contains(e.Message, wrongPW) || strings.Contains(e.Message, cfg.AdminPassword) {
				t.Fatalf("audit leaked a password: %+v", e)
			}
		}
	}
}
