package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/pquerna/otp/totp"
)

var recoveryFormat = regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$`)

// enroll2FA enrolls TOTP on an admin client and returns the secret and the
// recovery codes. The enrollment uses the current TOTP step.
func enroll2FA(t *testing.T, ac *http.Client, ts *httptest.Server, cfg *config.Config) (string, []string) {
	t.Helper()
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
	otp, err := totp.GenerateCode(start.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, b = postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", `{"code":"`+otp+`"}`)
	if code != http.StatusOK {
		t.Fatalf("enroll confirm: %d %s", code, b)
	}
	var conf struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if err := json.Unmarshal(b, &conf); err != nil {
		t.Fatal(err)
	}
	return start.Secret, conf.RecoveryCodes
}

// nextStepCode is the TOTP code one step ahead (accepted by the ±1 skew and
// newer than the step used at enrollment).
func nextStepCode(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// login posts /admin/login on a fresh cookie jar.
func login(t *testing.T, ts *httptest.Server, password, code string) (int, *http.Client, *http.Response) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"password": password, "code": code})
	resp, err := c.Post(ts.URL+"/admin/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, c, resp
}

func httpCode(t *testing.T, c *http.Client, ts *httptest.Server, path string) int {
	t.Helper()
	resp, err := c.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func twoFAStatus(t *testing.T, c *http.Client, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := c.Get(ts.URL + "/admin/2fa/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestRecoveryCodesIssuedHashedSingleUse(t *testing.T) {
	logs := captureSlog(t)
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, ac, ts, cfg)
	if len(codes) != 10 {
		t.Fatalf("got %d recovery codes, want 10", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !recoveryFormat.MatchString(c) || seen[c] {
			t.Fatalf("bad or duplicate recovery code %q", c)
		}
		seen[c] = true
	}

	// Only salted hashes are stored, in the same state file as the TOTP blob.
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "uplink-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range codes {
		if bytes.Contains(raw, []byte(c)) || bytes.Contains(raw, []byte(strings.ReplaceAll(c, "-", ""))) {
			t.Fatal("state file contains a plaintext recovery code")
		}
	}
	var st struct {
		Auth struct {
			TOTPBlob      string                        `json:"totpBlob"`
			RecoveryCodes []struct{ Salt, Hash string } `json:"recoveryCodes"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Auth.TOTPBlob == "" || len(st.Auth.RecoveryCodes) != 10 {
		t.Fatalf("state: totpBlob=%v codes=%d", st.Auth.TOTPBlob != "", len(st.Auth.RecoveryCodes))
	}
	salts := map[string]bool{}
	for _, h := range st.Auth.RecoveryCodes {
		if h.Salt == "" || salts[h.Salt] {
			t.Fatal("recovery hashes must each have their own salt")
		}
		salts[h.Salt] = true
	}

	// Recovery login works once (case and dashes do not matter), then the code is gone.
	use := strings.ToUpper(strings.ReplaceAll(codes[3], "-", ""))
	if code, _, _ := login(t, ts, cfg.AdminPassword, use); code != http.StatusOK {
		t.Fatalf("recovery login: %d", code)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, codes[3]); code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code: %d want 401", code)
	}
	if got := twoFAStatus(t, ac, ts)["recoveryRemaining"]; got != float64(9) {
		t.Fatalf("recoveryRemaining=%v want 9", got)
	}
	evs := auditEvents(t, ac, ts)
	if !hasAudit(evs, "recovery_codes_issued", "ok", "count=10") {
		t.Fatalf("missing recovery_codes_issued: %+v", evs)
	}
	if !hasAudit(evs, "recovery_code_used", "ok", "remaining=9") {
		t.Fatalf("missing recovery_code_used remaining=9: %+v", evs)
	}
	if !hasAudit(evs, "login", "fail", "bad_recovery_code") {
		t.Fatalf("missing bad_recovery_code on reuse: %+v", evs)
	}
	for _, c := range codes {
		n := strings.ReplaceAll(c, "-", "")
		for _, e := range evs {
			if strings.Contains(strings.ToLower(e.Message), n) {
				t.Fatalf("audit leaked a recovery code: %+v", e)
			}
		}
		if strings.Contains(strings.ToLower(logs.String()), n) {
			t.Fatal("log output contains a recovery code")
		}
	}
}

func TestRecoveryCodeConsumedOnceUnderConcurrency(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, ac, ts, cfg)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := login(t, ts, cfg.AdminPassword, codes[0])
			results <- code
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for c := range results {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("recovery code accepted %d times, want exactly 1", ok)
	}
}

func TestTOTPStepReuseRejected(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	secret, _ := enroll2FA(t, ac, ts, cfg)
	next := nextStepCode(t, secret)
	if code, _, _ := login(t, ts, cfg.AdminPassword, next); code != http.StatusOK {
		t.Fatalf("login with next-step code: %d", code)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, next); code != http.StatusUnauthorized {
		t.Fatalf("same TOTP step reused: %d want 401", code)
	}
	if !hasAudit(auditEvents(t, ac, ts), "login", "fail", "totp_reuse") {
		t.Fatal("missing totp_reuse audit")
	}
}

func TestRecoveryCodesRegenerate(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	secret, old := enroll2FA(t, ac, ts, cfg)
	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/recovery/regen", `{"password":"`+cfg.AdminPassword+`","code":"`+nextStepCode(t, secret)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("regen: %d %s", code, b)
	}
	var out struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	_ = json.Unmarshal(b, &out)
	if len(out.RecoveryCodes) != 10 {
		t.Fatalf("regen returned %d codes", len(out.RecoveryCodes))
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, old[0]); code != http.StatusUnauthorized {
		t.Fatalf("old code after regen: %d want 401", code)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, out.RecoveryCodes[0]); code != http.StatusOK {
		t.Fatalf("new code after regen: %d", code)
	}
	if !hasAudit(auditEvents(t, ac, ts), "recovery_codes_regenerated", "ok", "count=10") {
		t.Fatal("missing recovery_codes_regenerated audit")
	}
}

// Revoke-all and logout must force a full password + TOTP login: no
// remember-device state lets a password alone back in.
func TestRevokeAllAndLogoutForceFreshTOTP(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	secret, _ := enroll2FA(t, ac, ts, cfg)

	used := nextStepCode(t, secret)
	code, sess, _ := login(t, ts, cfg.AdminPassword, used)
	if code != http.StatusOK {
		t.Fatalf("password+totp login: %d", code)
	}
	if got := httpCode(t, sess, ts, "/admin/status"); got != http.StatusOK {
		t.Fatalf("status with session: %d", got)
	}
	u, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	oldCookies := sess.Jar.Cookies(u.URL)

	if code, b := postJSON(t, sess, ts, cfg, "/admin/sessions/revoke-all", "{}"); code != http.StatusOK {
		t.Fatalf("revoke-all: %d %s", code, b)
	}
	// Replay the old cookie explicitly (the response cleared it from the jar).
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(u.URL, oldCookies)
	replay := &http.Client{Jar: jar}
	if got := httpCode(t, replay, ts, "/admin/status"); got != http.StatusUnauthorized {
		t.Fatalf("old cookie after revoke-all: %d want 401", got)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusUnauthorized {
		t.Fatalf("other session after revoke-all: %d want 401", got)
	}

	// Password only: stops at 2fa_required, no session cookie.
	code, pwOnly, resp := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("password-only login after revoke: %d want 401", code)
	}
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			t.Fatalf("password-only login set cookie %s", c.Name)
		}
	}
	if got := httpCode(t, pwOnly, ts, "/admin/status"); got != http.StatusUnauthorized {
		t.Fatalf("status after password-only login: %d want 401", got)
	}

	// The TOTP code from before the revoke cannot be replayed either.
	if code, _, _ := login(t, ts, cfg.AdminPassword, used); code != http.StatusUnauthorized {
		t.Fatalf("pre-revoke TOTP step replayed: %d want 401", code)
	}
}

func TestLogoutForcesFreshTOTP(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	_, rc := enroll2FA(t, ac, ts, cfg)
	if code, b := postJSON(t, ac, ts, cfg, "/admin/logout", "{}"); code != http.StatusOK {
		t.Fatalf("logout: %d %s", code, b)
	}
	if got := httpCode(t, ac, ts, "/admin/status"); got != http.StatusUnauthorized {
		t.Fatalf("status after logout: %d want 401", got)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, ""); code != http.StatusUnauthorized {
		t.Fatalf("password-only login after logout: %d want 401", code)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, rc[0]); code != http.StatusOK {
		t.Fatalf("password+recovery login after logout: %d", code)
	}
}

func TestTOTPResetRevokesSessionsAndForcesReenroll(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	secret, codes := enroll2FA(t, ac, ts, cfg)

	// Wrong second factor: rejected, nothing wiped.
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/reset", `{"password":"`+cfg.AdminPassword+`","code":"000000"}`); code != http.StatusUnauthorized {
		t.Fatalf("reset with bad code: %d want 401", code)
	}
	// Wrong password: rejected.
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/reset", `{"password":"nope-nope-nope","code":"`+codes[1]+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("reset with bad password: %d want 401", code)
	}
	if twoFAStatus(t, ac, ts)["enrolled"] != true {
		t.Fatal("failed reset must not wipe 2FA")
	}
	// Missing CSRF headers: forbidden.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/2fa/reset", strings.NewReader(`{"password":"`+cfg.AdminPassword+`","code":"`+codes[1]+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reset without CSRF headers: %d want 403", resp.StatusCode)
	}

	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/reset", `{"password":"`+cfg.AdminPassword+`","code":"`+codes[0]+`"}`)
	if code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, b)
	}
	if got := httpCode(t, ac, ts, "/admin/2fa/status"); got != http.StatusUnauthorized {
		t.Fatalf("current session after reset: %d want 401", got)
	}
	// Old secret and codes are gone.
	if code, _, _ := login(t, ts, cfg.AdminPassword, nextStepCode(t, secret)); code != http.StatusOK {
		// Not enrolled any more: the password alone logs in (code ignored)...
		t.Fatalf("login after reset: %d", code)
	}
	// ...but the 2FA switch stays on, so the session can only enroll.
	_, fresh, _ := login(t, ts, cfg.AdminPassword, "")
	if got := httpCode(t, fresh, ts, "/admin/status"); got != http.StatusForbidden {
		t.Fatalf("admin access before re-enroll: %d want 403", got)
	}
	st := twoFAStatus(t, fresh, ts)
	if st["enrolled"] != false || st["on"] != true || st["need2fa"] != true || st["recoveryRemaining"] != float64(0) {
		t.Fatalf("2fa status after reset: %#v", st)
	}
	_, newCodes := enroll2FA(t, fresh, ts, cfg)
	if len(newCodes) != 10 {
		t.Fatalf("re-enroll issued %d codes", len(newCodes))
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, codes[2]); code != http.StatusUnauthorized {
		t.Fatalf("pre-reset recovery code after re-enroll: %d want 401", code)
	}
	evs := auditEvents(t, fresh, ts)
	if !hasAudit(evs, "2fa_reset", "ok", "sessions_revoked=1") {
		t.Fatalf("missing 2fa_reset audit: %+v", evs)
	}
	if !hasAudit(evs, "recovery_codes_issued", "ok", "count=10") {
		t.Fatal("missing recovery_codes_issued after re-enroll")
	}
}
