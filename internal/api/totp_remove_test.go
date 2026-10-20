package api

import (
	"net/http"
	"strings"
	"testing"
)

// Turning 2FA off (POST /admin/2fa/remove) without the password plus a
// valid second factor is rejected and changes nothing: 2FA stays on and the
// session keeps working.
func TestTOTPRemoveNeedsPasswordAndFactor(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, ac, ts, cfg)
	pw := cfg.AdminPassword
	// Five free failures, each 401 (and audited), then the shared lockout.
	for _, tc := range []struct{ name, body string }{
		{"no_code", `{"password":"` + pw + `"}`},
		{"bad_totp", `{"password":"` + pw + `","code":"000000"}`},
		{"bad_recovery", `{"password":"` + pw + `","code":"aaaa-aaaa-aaaa"}`},
		{"no_password", `{"code":"` + codes[0] + `"}`},
		{"bad_password", `{"password":"nope-nope-nope","code":"` + codes[0] + `"}`},
	} {
		if code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/remove", tc.body); code != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s, want 401", tc.name, code, b)
		}
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/remove", `{"password":"`+pw+`","code":""}`); code != http.StatusTooManyRequests {
		t.Fatalf("sixth failure: %d want 429 (lockout)", code)
	}
	// No CSRF headers: forbidden before anything is checked.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/2fa/remove",
		strings.NewReader(`{"password":"`+pw+`","code":"`+codes[0]+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("remove without CSRF: %d want 403", resp.StatusCode)
	}
	// No session: unauthorized.
	anon := &http.Client{}
	if code, _ := postJSON(t, anon, ts, cfg, "/admin/2fa/remove", `{"password":"`+pw+`","code":"`+codes[0]+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("remove without a session: %d want 401", code)
	}
	st := twoFAStatus(t, ac, ts)
	if st["enrolled"] != true || st["on"] != true || st["recoveryRemaining"] != float64(10) {
		t.Fatalf("rejected remove changed 2FA state: %#v", st)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("session after rejected remove: %d want 200", got)
	}
	if !hasAudit(auditEvents(t, ac, ts), "2fa_remove", "fail", "bad_password") {
		t.Fatal("missing 2fa_remove fail audit")
	}
}

// Remove turns 2FA off: it wipes 2FA, ends every admin session (this one
// too) and clears the cookie.
func TestTOTPRemoveRevokesAllSessions(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
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
	if st := twoFAStatus(t, fresh, ts); st["enrolled"] != false || st["on"] != false || st["recoveryRemaining"] != float64(0) {
		t.Fatalf("2fa status after remove: %#v", st)
	}
	if !hasAudit(auditEvents(t, fresh, ts), "2fa_remove", "ok", "sessions_revoked=2") {
		t.Fatal("missing 2fa_remove ok audit with sessions_revoked=2")
	}
}
