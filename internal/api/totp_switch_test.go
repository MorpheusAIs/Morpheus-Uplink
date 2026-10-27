package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

// diskTwoFactorOn reads auth.twoFactorOn from the state file and checks
// the file mode.
func diskTwoFactorOn(t *testing.T, dataDir string) any {
	t.Helper()
	p := filepath.Join(dataDir, "uplink-state.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("state file mode %v, want 0640", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Auth map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	return disk.Auth["twoFactorOn"]
}

// restartAt starts a new server on an existing data dir (a restart or
// redeploy with the same volume).
func restartAt(t *testing.T, dataDir string, edit func(*config.Config)) (*httptest.Server, *config.Config) {
	t.Helper()
	ts, cfg, _ := newTestServerWith(t, func(c *config.Config) {
		c.DataDir = dataDir
		edit(c)
	})
	return ts, cfg
}

// A fresh install has 2FA off: password-only login and full admin access.
func TestTwoFactorDefaultOff(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	st := twoFAStatus(t, ac, ts)
	if st["on"] != false || st["enrolled"] != false || st["need2fa"] != false {
		t.Fatalf("fresh 2fa status: %#v", st)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("admin access with 2FA off: %d", got)
	}
	if v := diskTwoFactorOn(t, cfg.DataDir); v != false {
		t.Fatalf("state file twoFactorOn = %v, want false", v)
	}
}

// Starting an enrollment does not turn 2FA on; only a confirmed code does.
func TestTwoFactorOnOnlyAfterConfirm(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	if code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", `{"password":"`+cfg.AdminPassword+`"}`); code != http.StatusOK {
		t.Fatalf("enroll start: %d %s", code, b)
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", `{"password":"`+cfg.AdminPassword+`","code":"000000"}`); code != http.StatusUnauthorized {
		t.Fatalf("confirm with a bad code: %d want 401", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != false || st["enrolled"] != false {
		t.Fatalf("2FA on without a confirmed enrollment: %#v", st)
	}
	if v := diskTwoFactorOn(t, cfg.DataDir); v != false {
		t.Fatalf("state file twoFactorOn = %v before confirm", v)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, ""); code != http.StatusOK {
		t.Fatalf("password-only login with a pending enrollment: %d", code)
	}
}

// off -> on -> restart -> off -> restart: the switch is persisted and
// login follows it.
func TestTwoFactorSwitchPersists(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	dir := cfg.DataDir
	ac := adminClient(t, ts, cfg)
	secret, codes := enroll2FA(t, ac, ts, cfg)
	if st := twoFAStatus(t, ac, ts); st["on"] != true || st["enrolled"] != true {
		t.Fatalf("after enroll: %#v", st)
	}
	if v := diskTwoFactorOn(t, dir); v != true {
		t.Fatalf("state file twoFactorOn = %v after enroll, want true", v)
	}

	// Restart: still on, the password alone is not enough.
	ts, cfg = restartAt(t, dir, func(*config.Config) {})
	if code, _, _ := login(t, ts, cfg.AdminPassword, ""); code != http.StatusUnauthorized {
		t.Fatalf("password-only login after restart with 2FA on: %d want 401", code)
	}
	totpCode := nextStepCode(t, secret)
	code, ac, _ := login(t, ts, cfg.AdminPassword, totpCode)
	if code != http.StatusOK {
		t.Fatalf("password + TOTP login after restart: %d", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != true {
		t.Fatalf("after restart: %#v", st)
	}

	// Turn off: a TOTP step already used is rejected; a recovery code works.
	if c, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/remove", `{"password":"`+cfg.AdminPassword+`","code":"`+totpCode+`"}`); c != http.StatusUnauthorized {
		t.Fatalf("remove with a replayed TOTP step: %d want 401", c)
	}
	if c, b := postJSON(t, ac, ts, cfg, "/admin/2fa/remove", `{"password":"`+cfg.AdminPassword+`","code":"`+codes[0]+`"}`); c != http.StatusOK {
		t.Fatalf("remove with password + recovery code: %d %s", c, b)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusUnauthorized {
		t.Fatalf("session after turning 2FA off: %d want 401", got)
	}
	if v := diskTwoFactorOn(t, dir); v != false {
		t.Fatalf("state file twoFactorOn = %v after remove, want false", v)
	}

	// Restart: still off, password-only login with full access.
	ts, cfg = restartAt(t, dir, func(*config.Config) {})
	code, ac, _ = login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("password-only login after restart with 2FA off: %d", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != false || st["enrolled"] != false || st["recoveryRemaining"] != float64(0) {
		t.Fatalf("after off + restart: %#v", st)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("admin access with 2FA off: %d", got)
	}
}

// UPLINK_RESET_2FA wipes 2FA and the switch: the install comes back with
// 2FA off and stays off after the variable is removed.
func TestBreakGlassTurnsTwoFactorOff(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	dir := cfg.DataDir
	enroll2FA(t, adminClient(t, ts, cfg), ts, cfg)

	ts, cfg = restartAt(t, dir, func(c *config.Config) { c.Reset2FA = true })
	code, ac, _ := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("password-only login after break-glass: %d", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != false || st["enrolled"] != false || st["breakGlassWarn"] != true {
		t.Fatalf("after break-glass: %#v", st)
	}
	if v := diskTwoFactorOn(t, dir); v != false {
		t.Fatalf("state file twoFactorOn = %v after break-glass", v)
	}

	ts, cfg = restartAt(t, dir, func(*config.Config) {})
	code, ac, _ = login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("password-only login after removing UPLINK_RESET_2FA: %d", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != false || st["breakGlassWarn"] != false {
		t.Fatalf("after removing UPLINK_RESET_2FA: %#v", st)
	}
}

// A state file whose switch says off while a TOTP secret is stored (hand
// edit, partial write) fails closed: 2FA stays on and enforced.
func TestTwoFactorFlagOffWithSecretFailsClosed(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	dir := cfg.DataDir
	secret, _ := enroll2FA(t, adminClient(t, ts, cfg), ts, cfg)
	p := filepath.Join(dir, "uplink-state.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []any{false, nil} {
		auth := doc["auth"].(map[string]any)
		if flag == nil {
			delete(auth, "twoFactorOn")
		} else {
			auth["twoFactorOn"] = flag
		}
		out, _ := json.Marshal(doc)
		if err := os.WriteFile(p, out, 0o640); err != nil {
			t.Fatal(err)
		}
		ts, cfg = restartAt(t, dir, func(*config.Config) {})
		if code, _, _ := login(t, ts, cfg.AdminPassword, ""); code != http.StatusUnauthorized {
			t.Fatalf("twoFactorOn=%v with a secret: password-only login %d, want 401", flag, code)
		}
	}
	code, ac, _ := login(t, ts, cfg.AdminPassword, nextStepCode(t, secret))
	if code != http.StatusOK {
		t.Fatalf("password + TOTP login: %d", code)
	}
	if st := twoFAStatus(t, ac, ts); st["on"] != true || st["enrolled"] != true {
		t.Fatalf("2fa status: %#v", st)
	}
}
