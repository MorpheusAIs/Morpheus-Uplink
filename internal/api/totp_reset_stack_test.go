package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// These tests go through the real mux and middleware (httptest server from
// newTestServer: requireAdmin / requireSession, CSRF, cookie sessions).

func getBody(t *testing.T, c *http.Client, ts string, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(ts + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestTOTPResetThroughStack: reset with password + current TOTP wipes 2FA
// on disk, ends every session, and the next login must enroll a NEW secret;
// the old secret and old recovery codes never validate again.
func TestTOTPResetThroughStack(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	oldSecret, oldCodes := enroll2FA(t, ac, ts, cfg)
	code, other, _ := login(t, ts, cfg.AdminPassword, oldCodes[5])
	if code != http.StatusOK {
		t.Fatalf("second session login: %d", code)
	}

	c, b := postJSON(t, ac, ts, cfg, "/admin/2fa/reset", `{"password":"`+cfg.AdminPassword+`","code":"`+nextStepCode(t, oldSecret)+`"}`)
	if c != http.StatusOK {
		t.Fatalf("reset with password + TOTP: %d %s", c, b)
	}
	var rr struct {
		OK       bool `json:"ok"`
		Reenroll bool `json:"reenroll"`
	}
	if err := json.Unmarshal(b, &rr); err != nil || !rr.OK || !rr.Reenroll {
		t.Fatalf("reset response: %s", b)
	}

	// Every session is gone, this one included.
	for n, cl := range map[string]*http.Client{"current": ac, "other": other} {
		if got := httpCode(t, cl, ts, "/admin/2fa/status"); got != http.StatusUnauthorized {
			t.Fatalf("%s session after reset: %d want 401", n, got)
		}
	}

	// The wipe is on disk, not only in memory.
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "uplink-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Auth map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"totpBlob", "totpSalt", "recoveryCodes", "enrolled", "lastTotpStep", "pendingSecretEnc"} {
		if v, ok := disk.Auth[k]; ok {
			t.Fatalf("state file still has auth.%s = %v after reset", k, v)
		}
	}

	// The switch stays on in the file; password-only login can only enroll.
	if disk.Auth["twoFactorOn"] != true {
		t.Fatalf("state file twoFactorOn = %v after reset, want true", disk.Auth["twoFactorOn"])
	}
	code, fresh, _ := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("password login after reset: %d", code)
	}
	st := twoFAStatus(t, fresh, ts)
	if st["enrolled"] != false || st["on"] != true || st["need2fa"] != true || st["recoveryRemaining"] != float64(0) {
		t.Fatalf("2fa status after reset: %#v", st)
	}
	if c, body := getBody(t, fresh, ts.URL, "/admin/status"); c != http.StatusForbidden || !strings.Contains(body, "2fa enrollment required") {
		t.Fatalf("admin access before re-enroll: %d %q, want 403 2fa enrollment required", c, body)
	}

	// New enrollment: fresh secret; the old secret is rejected.
	c, b = postJSON(t, fresh, ts, cfg, "/admin/2fa/enroll/start", "{}")
	if c != http.StatusOK {
		t.Fatalf("enroll start after reset: %d %s", c, b)
	}
	var start struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(b, &start)
	if start.Secret == "" || start.Secret == oldSecret {
		t.Fatal("re-enrollment must use a new secret")
	}
	oldNow, _ := totp.GenerateCode(oldSecret, time.Now())
	newNow, _ := totp.GenerateCode(start.Secret, time.Now())
	if oldNow != newNow {
		if c, _ := postJSON(t, fresh, ts, cfg, "/admin/2fa/enroll/confirm", `{"code":"`+oldNow+`"}`); c != http.StatusUnauthorized {
			t.Fatalf("old secret at enroll confirm: %d want 401", c)
		}
	}
	c, b = postJSON(t, fresh, ts, cfg, "/admin/2fa/enroll/confirm", `{"code":"`+newNow+`"}`)
	if c != http.StatusOK {
		t.Fatalf("enroll confirm with new secret: %d %s", c, b)
	}
	var conf struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	_ = json.Unmarshal(b, &conf)
	if len(conf.RecoveryCodes) != 10 {
		t.Fatalf("re-enroll issued %d codes", len(conf.RecoveryCodes))
	}

	// Old TOTP and old recovery codes never log in again; the new TOTP does.
	oldNext := nextStepCode(t, oldSecret)
	newNext := nextStepCode(t, start.Secret)
	if oldNext != newNext {
		if c, _, _ := login(t, ts, cfg.AdminPassword, oldNext); c != http.StatusUnauthorized {
			t.Fatalf("old TOTP after re-enroll: %d want 401", c)
		}
	}
	if c, _, _ := login(t, ts, cfg.AdminPassword, oldCodes[2]); c != http.StatusUnauthorized {
		t.Fatalf("old recovery code after re-enroll: %d want 401", c)
	}
	code, nc, _ := login(t, ts, cfg.AdminPassword, newNext)
	if code != http.StatusOK {
		t.Fatalf("login with new TOTP: %d", code)
	}
	if got := httpCode(t, nc, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("admin access after re-enroll: %d", got)
	}
	// /admin/audit is admin-gated, so read it once enrolled again.
	if !hasAudit(auditEvents(t, nc, ts), "2fa_reset", "ok", "sessions_revoked=2") {
		t.Fatal("missing 2fa_reset audit with sessions_revoked=2")
	}
}
