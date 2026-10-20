package api

import (
	"net/http"
	"strings"
	"testing"
)

// With ADMIN_2FA_REQUIRED=true, remove is refused before any code is
// consumed and points to Reset; 2FA and the session stay as they were.
func TestTOTPRemoveRefusedWhen2FARequired(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.Admin2FARequired = true
	ac := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, ac, ts, cfg)

	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/remove", `{"password":"`+cfg.AdminPassword+`","code":"`+codes[0]+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("remove with 2FA required: %d %s, want 409", code, b)
	}
	if !strings.Contains(string(b), "Reset 2FA") {
		t.Fatalf("refusal must point to Reset 2FA: %q", b)
	}
	st := twoFAStatus(t, ac, ts)
	if st["enrolled"] != true || st["recoveryRemaining"] != float64(10) {
		t.Fatalf("refused remove changed 2FA state: %#v", st)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("session after refused remove: %d want 200", got)
	}
	if !hasAudit(auditEvents(t, ac, ts), "2fa_remove", "blocked", "2fa_required") {
		t.Fatal("missing 2fa_remove blocked audit")
	}
}

// When allowed, remove wipes 2FA and, like reset, ends every admin
// session (this one too) and clears the cookie.
func TestTOTPRemoveRevokesAllSessions(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.Admin2FARequired = false
	ac := adminClient(t, ts, cfg)
	secret, codes := enroll2FA(t, ac, ts, cfg)
	code, other, _ := login(t, ts, cfg.AdminPassword, nextStepCode(t, secret))
	if code != http.StatusOK {
		t.Fatalf("second session login: %d", code)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/2fa/remove",
		strings.NewReader(`{"password":"`+cfg.AdminPassword+`","code":"`+codes[0]+`"}`))
	req.Header.Set("Content-Type", "application/json")
	withCSRF(req, cfg)
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("remove did not clear the session cookie")
	}
	for name, c := range map[string]*http.Client{"current": ac, "other": other} {
		if got := httpCode(t, c, ts, "/admin/status"); got != http.StatusUnauthorized {
			t.Fatalf("%s session after remove: %d want 401", name, got)
		}
	}
	code, fresh, _ := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("password login after remove: %d", code)
	}
	if st := twoFAStatus(t, fresh, ts); st["enrolled"] != false || st["recoveryRemaining"] != float64(0) {
		t.Fatalf("2fa status after remove: %#v", st)
	}
	if !hasAudit(auditEvents(t, fresh, ts), "2fa_remove", "ok", "sessions_revoked=2") {
		t.Fatal("missing 2fa_remove ok audit with sessions_revoked=2")
	}
}
