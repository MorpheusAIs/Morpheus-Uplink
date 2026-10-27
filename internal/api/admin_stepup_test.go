package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
	"github.com/pquerna/otp/totp"
)

// s1Clock is a fake clock for the admin session store.
type s1Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *s1Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *s1Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// s1Server is newTestServerWith that also returns the *Server, with the
// session store on a fake clock.
func s1Server(t *testing.T, edit func(*config.Config)) (*httptest.Server, *Server, *config.Config, *s1Clock) {
	t.Helper()
	var opened atomic.Int32
	rt := mockRouter(t, &opened)
	t.Cleanup(rt.Close)
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)
	cfg := &config.Config{
		RouterURL: rt.URL, RouterUser: "admin", RouterPass: "routerpass",
		AdminPassword: "adminpass", APIKeySeed: "test-seed", DataDir: t.TempDir(),
		ActiveModelsURL: cat.URL, SessionDurationSec: 600, DevHTTP: true,
	}
	if edit != nil {
		edit(cfg)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, cfg.SessionDurationSec), rc, nil)
	clk := &s1Clock{t: time.Now()}
	srv.sessions.SetClock(clk.Now)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	cfg.WebPublicURL = u.Scheme + "://" + u.Host
	return ts, srv, cfg, clk
}

// s1Do sends one request with CSRF headers; hdr may edit the headers.
func s1Do(t *testing.T, c *http.Client, ts *httptest.Server, cfg *config.Config, method, path, body string, hdr func(http.Header)) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	withCSRF(req, cfg)
	if hdr != nil {
		hdr(req.Header)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func pollHdr(h http.Header) { h.Set("X-Uplink-Poll", "1") }

// rawCookie is the admin session token held by c (test-only; never logged).
func rawCookie(t *testing.T, c *http.Client, ts *httptest.Server) string {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == adminauth.CookieName(true) {
			return ck.Value
		}
	}
	t.Fatal("no admin cookie")
	return ""
}

func stepUpAt(t *testing.T, srv *Server, raw string) time.Time {
	t.Helper()
	sess, ok := srv.sessions.Lookup(raw, false)
	if !ok {
		t.Fatal("session gone")
	}
	return sess.StepUpAt
}

func setSwitch(t *testing.T, srv *Server, on bool) {
	t.Helper()
	if err := srv.store.UpdateAuthState(func(a *store.AuthState) error {
		a.TwoFactorOn = on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func adminSessionStatus(t *testing.T, c *http.Client, ts *httptest.Server, cfg *config.Config) map[string]any {
	t.Helper()
	code, b := s1Do(t, c, ts, cfg, http.MethodGet, "/admin/status", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, b)
	}
	var st struct {
		AdminSession map[string]any `json:"adminSession"`
	}
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	return st.AdminSession
}

func pwBody(cfg *config.Config, code string) string {
	b, _ := json.Marshal(map[string]string{"password": cfg.AdminPassword, "code": code})
	return string(b)
}

// twoFAState prepares one of the three 2FA states on a fresh server and
// returns a logged-in client (session created at the fake clock's now).
func twoFAState(t *testing.T, state string) (*httptest.Server, *Server, *config.Config, *s1Clock, *http.Client) {
	t.Helper()
	ts, srv, cfg, clk := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	switch state {
	case "off":
	case "on-not-enrolled":
		setSwitch(t, srv, true)
	case "on-enrolled":
		enroll2FA(t, ac, ts, cfg) // re-issues the caller's session
	default:
		t.Fatal(state)
	}
	return ts, srv, cfg, clk, ac
}

var twoFAStates = []string{"off", "on-not-enrolled", "on-enrolled"}

// S1 test 1: unset -> idle 4h, absolute 12h on Status; with 2FA off an
// informational "2FA is off" warning, which goes away once enrolled.
func TestS1StatusShowsSessionLengthsAndTwoFactorOffNote(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	st := adminSessionStatus(t, ac, ts, cfg)
	if st["idleSec"] != float64(4*3600) || st["absoluteSec"] != float64(12*3600) || st["idle"] != "4h0m0s" || st["absolute"] != "12h0m0s" {
		t.Fatalf("defaults: %#v", st)
	}
	if w, _ := st["warnings"].([]any); len(w) != 1 || w[0] != "2FA is off" {
		t.Fatalf("warnings with 2FA off: %#v", st["warnings"])
	}
	enroll2FA(t, ac, ts, cfg)
	st = adminSessionStatus(t, ac, ts, cfg)
	if w, _ := st["warnings"].([]any); len(w) != 0 {
		t.Fatalf("warnings with 2FA on: %#v", st["warnings"])
	}
	if st["idleSec"] != float64(4*3600) || st["absoluteSec"] != float64(12*3600) {
		t.Fatalf("lengths changed with 2FA on: %#v", st)
	}
}

// S1 test 2: idle expires at 4h + 1s and not before in every 2FA state, and
// flipping the 2FA switch mid-session changes nothing. Poll requests are
// used as probes, so they do not extend idle (S1 test 5).
func TestS1IdleIndependentOfTwoFactor(t *testing.T) {
	for _, state := range twoFAStates {
		t.Run(state, func(t *testing.T) {
			ts, srv, cfg, clk, ac := twoFAState(t, state)
			clk.Advance(2 * time.Hour)
			if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", pollHdr); code != http.StatusOK {
				t.Fatalf("at 2h: %d %s", code, b)
			}
			setSwitch(t, srv, state == "off")
			setSwitch(t, srv, state != "off")
			clk.Advance(2 * time.Hour)
			if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", pollHdr); code != http.StatusOK {
				t.Fatalf("at 4h: %d %s", code, b)
			}
			clk.Advance(time.Second)
			if code, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", pollHdr); code != http.StatusUnauthorized {
				t.Fatalf("at 4h+1s: %d, want 401", code)
			}
		})
	}
}

// S1 test 3: absolute expiry at 12h whatever the activity, in every 2FA
// state.
func TestS1AbsoluteIndependentOfTwoFactor(t *testing.T) {
	for _, state := range twoFAStates {
		t.Run(state, func(t *testing.T) {
			ts, _, cfg, clk, ac := twoFAState(t, state)
			for h := 1; h <= 12; h++ {
				clk.Advance(time.Hour)
				if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", nil); code != http.StatusOK {
					t.Fatalf("at %dh: %d %s", h, code, b)
				}
			}
			clk.Advance(time.Second)
			if code, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", nil); code != http.StatusUnauthorized {
				t.Fatalf("at 12h+1s: %d, want 401", code)
			}
		})
	}
}

// S1 test 4: idle 4h and absolute 12h are fixed; setting ADMIN_SESSION_*
// env vars has no effect on Status, expiry or the cookie.
func TestS1SessionEnvVarsHaveNoEffect(t *testing.T) {
	t.Setenv("ADMIN_SESSION_IDLE", "1m")
	t.Setenv("ADMIN_SESSION_ABSOLUTE", "1h")
	t.Setenv("ADMIN_SESSION_TIMEOUT", "1m")
	ts, _, cfg, clk := s1Server(t, nil)
	code, ac, resp := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	for _, c := range resp.Cookies() {
		if c.Name == adminauth.CookieName(true) && c.MaxAge != 12*3600 {
			t.Fatalf("cookie MaxAge %d, want 43200", c.MaxAge)
		}
	}
	st := adminSessionStatus(t, ac, ts, cfg)
	if st["idle"] != "4h0m0s" || st["absolute"] != "12h0m0s" {
		t.Fatalf("status with ADMIN_SESSION_* set: %#v", st)
	}
	clk.Advance(2 * time.Hour) // past the env values
	if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", pollHdr); code != http.StatusOK {
		t.Fatalf("session ended by an ADMIN_SESSION_* env var: %d %s", code, b)
	}
	clk.Advance(2*time.Hour + time.Second)
	if code, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/2fa/status", "", pollHdr); code != http.StatusUnauthorized {
		t.Fatalf("at 4h+1s idle: %d, want 401", code)
	}
}

// S1 test 4 (config side): FromEnv accepts and ignores ADMIN_SESSION_*.
func TestS1FromEnvIgnoresSessionEnvVars(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "a-long-enough-test-value")
	t.Setenv("API_KEY_SEED", "test-seed")
	t.Setenv("ROUTER_AUTH", "admin:router-test-value")
	t.Setenv("ADMIN_SESSION_IDLE", "banana")
	t.Setenv("ADMIN_SESSION_ABSOLUTE", "48h")
	if _, err := config.FromEnv(); err != nil {
		t.Fatalf("FromEnv with ADMIN_SESSION_* set: %v", err)
	}
	if adminauth.NewSessionStore().Idle() != 4*time.Hour || adminauth.NewSessionStore().Absolute() != 12*time.Hour {
		t.Fatal("session lengths changed")
	}
}

// S1 test 5: cookie MaxAge equals the 12h absolute; flags unchanged.
func TestS1CookieMaxAgeFollowsAbsolute(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	code, _, resp := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name != adminauth.CookieName(true) {
			continue
		}
		found = true
		if c.MaxAge != 12*3600 || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
			t.Fatalf("cookie: %+v, want MaxAge 43200", c)
		}
	}
	if !found {
		t.Fatal("no session cookie")
	}
}

// S1 tests 6 and 15: 2fa/remove and 2fa/reset succeed with password + a
// recovery code and with password + TOTP, and revoke every session.
func TestS1RemoveResetFactorsAndRevokeAll(t *testing.T) {
	for _, ep := range []string{"/admin/2fa/remove", "/admin/2fa/reset"} {
		for _, factor := range []string{"recovery", "totp"} {
			t.Run(ep+"/"+factor, func(t *testing.T) {
				ts, _, cfg, _ := s1Server(t, nil)
				ac := adminClient(t, ts, cfg)
				secret, codes := enroll2FA(t, ac, ts, cfg)
				code, other, _ := login(t, ts, cfg.AdminPassword, codes[1])
				if code != http.StatusOK {
					t.Fatalf("second login: %d", code)
				}
				f := codes[0]
				if factor == "totp" {
					f = nextStepCode(t, secret)
				}
				if c, b := postJSON(t, ac, ts, cfg, ep, pwBody(cfg, f)); c != http.StatusOK {
					t.Fatalf("%s: %d %s", ep, c, b)
				}
				for i, c := range []*http.Client{ac, other} {
					if got := httpCode(t, c, ts, "/admin/2fa/status"); got != http.StatusUnauthorized {
						t.Fatalf("session %d after %s: %d, want 401", i, ep, got)
					}
				}
			})
		}
	}
}

// S1 test 7: enroll start/confirm need the password (lockout applies, no
// secret, nothing revoked); a good confirm revokes the other sessions and
// re-issues the caller's.
func TestS1EnrollNeedsPassword(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	other := adminClient(t, ts, cfg)

	for _, body := range []string{`{}`, `{"password":"wrong"}`} {
		code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", body)
		if code != http.StatusUnauthorized || strings.TrimSpace(string(b)) != errMsgBadPassword || strings.Contains(string(b), "otpauth") {
			t.Fatalf("start %s: %d %s", body, code, b)
		}
	}
	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", `{"password":"`+cfg.AdminPassword+`"}`)
	if code != http.StatusOK {
		t.Fatalf("start: %d %s", code, b)
	}
	var start struct{ Secret string }
	_ = json.Unmarshal(b, &start)
	otp, _ := totp.GenerateCode(start.Secret, time.Now())
	for _, body := range []string{`{"code":"` + otp + `"}`, `{"password":"wrong","code":"` + otp + `"}`} {
		if code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", body); code != http.StatusUnauthorized {
			t.Fatalf("confirm without the password: %d %s", code, b)
		}
	}
	if httpCode(t, other, ts, "/admin/2fa/status") != http.StatusOK {
		t.Fatal("a refused enrolment revoked another session")
	}
	if st := twoFAStatus(t, ac, ts); st["enrolled"] != false {
		t.Fatalf("enrolled after refused confirm: %#v", st)
	}
	evs := auditEvents(t, ac, ts)
	fails := 0
	for _, e := range evs {
		if e.Kind == "2fa_enroll" && e.Result == "fail" {
			fails++
		}
	}
	if fails != 4 {
		t.Fatalf("2fa_enroll fail lines: %d, want 4", fails)
	}

	oldRaw := rawCookie(t, ac, ts)
	if code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/confirm", pwBody(cfg, otp)); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, b)
	}
	if httpCode(t, other, ts, "/admin/2fa/status") != http.StatusUnauthorized {
		t.Fatal("other session alive after confirm")
	}
	if _, ok := srv.sessions.Lookup(oldRaw, false); ok {
		t.Fatal("caller's old session token still valid")
	}
	if rawCookie(t, ac, ts) == oldRaw || httpCode(t, ac, ts, "/admin/2fa/status") != http.StatusOK {
		t.Fatal("caller's session not re-issued")
	}
}

// S1 test 7 (lockout): failed enrol password checks count toward the per-IP
// lockout.
func TestS1EnrollPasswordCountsTowardLockout(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	for i := 1; i <= 5; i++ {
		if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", `{"password":"wrong"}`); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", `{"password":"wrong"}`); code != http.StatusTooManyRequests {
		t.Fatalf("sixth failure: %d, want 429", code)
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/2fa/enroll/start", `{"password":"`+cfg.AdminPassword+`"}`); code != http.StatusTooManyRequests {
		t.Fatalf("locked start with the password: %d, want 429", code)
	}
}

// S1 test 8 and N3-2: regen needs password + TOTP; a recovery code is
// refused before anything is consumed (remaining count unchanged, and the
// code still works afterwards).
func TestS1RegenNeedsPasswordAndTOTP(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	secret, codes := enroll2FA(t, ac, ts, cfg)
	before := twoFAStatus(t, ac, ts)["recoveryRemaining"]

	if code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/recovery/regen", `{"code":"`+nextStepCode(t, secret)+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("regen without password: %d %s", code, b)
	}
	code, b := postJSON(t, ac, ts, cfg, "/admin/2fa/recovery/regen", pwBody(cfg, codes[0]))
	if code != http.StatusUnauthorized || !strings.Contains(string(b), "recovery codes are not accepted") {
		t.Fatalf("regen with a recovery code: %d %s", code, b)
	}
	if after := twoFAStatus(t, ac, ts)["recoveryRemaining"]; after != before {
		t.Fatalf("recoveryRemaining %v -> %v after a refused recovery code", before, after)
	}
	code, b = postJSON(t, ac, ts, cfg, "/admin/2fa/recovery/regen", pwBody(cfg, nextStepCode(t, secret)))
	if code != http.StatusOK {
		t.Fatalf("regen with password + TOTP: %d %s", code, b)
	}
	var out struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	_ = json.Unmarshal(b, &out)
	if len(out.RecoveryCodes) != 10 {
		t.Fatalf("new codes: %d", len(out.RecoveryCodes))
	}
	// The refused code was not consumed but is now replaced.
	if code, _, _ := login(t, ts, cfg.AdminPassword, codes[0]); code != http.StatusUnauthorized {
		t.Fatalf("old recovery code after regen: %d", code)
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, out.RecoveryCodes[0]); code != http.StatusOK {
		t.Fatalf("new recovery code: %d", code)
	}
}

// S1 test 9: reveal works within 30 min of login, then needs a step-up;
// masked listing never carries the full keys.
func TestS1RevealNeedsFreshSession(t *testing.T) {
	ts, _, cfg, clk := s1Server(t, nil)
	master := keymaker.MasterKey(cfg.APIKeySeed)
	ac := adminClient(t, ts, cfg)

	if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys", "", nil); code != http.StatusOK || strings.Contains(b, master) {
		t.Fatalf("masked listing: %d (full key present: %v)", code, strings.Contains(b, master))
	}
	if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys?reveal=1", "", nil); code != http.StatusOK || !strings.Contains(b, master) {
		t.Fatalf("reveal just after login: %d", code)
	}
	if n := countAudit(auditEvents(t, ac, ts), "key_reveal"); n != 1 {
		t.Fatalf("key_reveal lines: %d, want 1", n)
	}

	clk.Advance(30*time.Minute + time.Second)
	code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys?reveal=1", "", nil)
	if code != http.StatusUnauthorized || !strings.Contains(b, "step_up_required") || strings.Contains(b, master) {
		t.Fatalf("stale reveal: %d %q", code, b)
	}
	if n := countAudit(auditEvents(t, ac, ts), "key_reveal"); n != 1 {
		t.Fatalf("key_reveal lines after a refused reveal: %d, want 1", n)
	}
	if httpCode(t, ac, ts, "/admin/2fa/status") != http.StatusOK {
		t.Fatal("step_up_required ended the session")
	}

	if code, b := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, "")); code != http.StatusOK {
		t.Fatalf("step-up: %d %s", code, b)
	}
	if code, b := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys?reveal=1", "", nil); code != http.StatusOK || !strings.Contains(b, master) {
		t.Fatalf("reveal after step-up: %d", code)
	}
	clk.Advance(30*time.Minute + time.Second)
	if code, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys?reveal=true", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("reveal 30m01s after step-up: %d, want 401", code)
	}
}

// S1 test 10: reveal over the master Bearer is 403 whatever
// MASTER_KEY_ADMIN is; the masked listing over the Bearer still works when
// it is true.
func TestS1RevealRefusedOverBearer(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("MASTER_KEY_ADMIN=%v", on), func(t *testing.T) {
			ts, _, cfg, _ := s1Server(t, func(c *config.Config) { c.MasterKeyAdmin = on })
			master := keymaker.MasterKey(cfg.APIKeySeed)
			ac := adminClient(t, ts, cfg)
			bearer := func(h http.Header) { h.Set("Authorization", "Bearer "+master) }
			for _, q := range []string{"reveal=1", "reveal=true", "reveal=0&reveal=1"} {
				for _, c := range []*http.Client{http.DefaultClient, ac} {
					code, b := s1Do(t, c, ts, cfg, http.MethodGet, "/admin/keys?"+q, "", bearer)
					if code != http.StatusForbidden || strings.Contains(b, master) {
						t.Fatalf("Bearer %s: %d", q, code)
					}
				}
			}
			code, b := s1Do(t, http.DefaultClient, ts, cfg, http.MethodGet, "/admin/keys", "", bearer)
			want := http.StatusUnauthorized
			if on {
				want = http.StatusOK
			}
			if code != want || strings.Contains(b, master) {
				t.Fatalf("masked listing over Bearer: %d, want %d", code, want)
			}
		})
	}
}

// xffTransport sets X-Forwarded-For from a variable (behind a trusted proxy).
type xffTransport struct {
	mu  sync.Mutex
	xff string
}

func (x *xffTransport) set(v string) { x.mu.Lock(); x.xff = v; x.mu.Unlock() }

func (x *xffTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	x.mu.Lock()
	v := x.xff
	x.mu.Unlock()
	r = r.Clone(r.Context())
	if v != "" {
		r.Header.Set("X-Forwarded-For", v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func xffAdminClient(t *testing.T, ts *httptest.Server, cfg *config.Config, first string) (*http.Client, *xffTransport) {
	t.Helper()
	tr := &xffTransport{xff: first}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: tr}
	resp, err := c.Post(ts.URL+"/admin/login", "application/json", strings.NewReader(`{"password":"`+cfg.AdminPassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	return c, tr
}

// S1 test 11: a new client address on a live session writes one
// admin_session_ip_change line per change; no binding, no logout.
func TestS1SessionAddressChangeAudited(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, func(c *config.Config) { c.TrustedProxyCIDRs = []string{"127.0.0.0/8"} })
	c, tr := xffAdminClient(t, ts, cfg, "198.51.100.1")
	if httpCode(t, c, ts, "/admin/status") != http.StatusOK {
		t.Fatal("status")
	}
	if n := countAudit(auditEvents(t, c, ts), "admin_session_ip_change"); n != 0 {
		t.Fatalf("changes before any change: %d", n)
	}
	tr.set("198.51.100.2")
	for i := 0; i < 3; i++ {
		if got := httpCode(t, c, ts, "/admin/status"); got != http.StatusOK {
			t.Fatalf("request from the new address: %d (session must stay valid)", got)
		}
	}
	evs := auditEvents(t, c, ts)
	if n := countAudit(evs, "admin_session_ip_change"); n != 1 {
		t.Fatalf("admin_session_ip_change lines: %d, want 1", n)
	}
	for _, e := range evs {
		if e.Kind == "admin_session_ip_change" && (e.IP != "198.51.100.2" || !strings.Contains(e.Message, "previous=198.51.100.1")) {
			t.Fatalf("change line: %#v", e)
		}
	}
	if httpCode(t, c, ts, "/admin/2fa/status") != http.StatusOK {
		t.Fatal("session-only route after the change")
	}
}

// S1 test 12: POST /admin/step-up is cookie-only, refuses any
// Authorization header and failed CSRF without stamping; wrong password or
// TOTP counts toward the lockout; a recovery code is refused (N3-2: not
// consumed); a TOTP code cannot be used twice; success stamps and audits.
func TestS1StepUpChecks(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	master := keymaker.MasterKey(cfg.APIKeySeed)
	ac := adminClient(t, ts, cfg)
	secret, codes := enroll2FA(t, ac, ts, cfg)
	raw := rawCookie(t, ac, ts)
	good := nextStepCode(t, secret)

	if code, _ := s1Do(t, http.DefaultClient, ts, cfg, http.MethodPost, "/admin/step-up", pwBody(cfg, good), nil); code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", code)
	}
	refusals := []struct {
		name string
		want int
		hdr  func(http.Header)
	}{
		{"master Bearer", http.StatusUnauthorized, func(h http.Header) { h.Set("Authorization", "Bearer "+master) }},
		{"other Authorization", http.StatusUnauthorized, func(h http.Header) { h.Set("Authorization", "Basic Zm9vOmJhcg==") }},
		{"cross-site", http.StatusForbidden, func(h http.Header) { h.Set("Sec-Fetch-Site", "cross-site") }},
		{"wrong Origin", http.StatusForbidden, func(h http.Header) { h.Set("Origin", "https://other.example") }},
	}
	for _, rf := range refusals {
		if code, _ := s1Do(t, ac, ts, cfg, http.MethodPost, "/admin/step-up", pwBody(cfg, good), rf.hdr); code != rf.want {
			t.Fatalf("%s: %d, want %d", rf.name, code, rf.want)
		}
	}
	if !stepUpAt(t, srv, raw).IsZero() {
		t.Fatal("stamped by a refused request")
	}

	remaining := twoFAStatus(t, ac, ts)["recoveryRemaining"]
	code, b := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, codes[0]))
	if code != http.StatusUnauthorized || !strings.Contains(string(b), "recovery codes are not accepted") {
		t.Fatalf("recovery code: %d %s", code, b)
	}
	if got := twoFAStatus(t, ac, ts)["recoveryRemaining"]; got != remaining {
		t.Fatalf("recoveryRemaining %v -> %v (recovery code consumed)", remaining, got)
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", `{"password":"wrong","code":"`+good+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, "")); code != http.StatusUnauthorized {
		t.Fatalf("missing code with 2FA enrolled: %d", code)
	}
	if !stepUpAt(t, srv, raw).IsZero() {
		t.Fatal("stamped by a failed step-up")
	}

	if code, b := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, good)); code != http.StatusOK {
		t.Fatalf("step-up: %d %s", code, b)
	}
	at := stepUpAt(t, srv, raw)
	if at.IsZero() {
		t.Fatal("not stamped")
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, good)); code != http.StatusUnauthorized {
		t.Fatalf("replayed TOTP: %d, want 401", code)
	}
	evs := auditEvents(t, ac, ts)
	if !hasAudit(evs, "step_up", "ok", "method=totp") {
		t.Fatalf("no step_up ok: %#v", evs)
	}
	fails := 0
	for _, e := range evs {
		if e.Kind == "step_up" && e.Result == "fail" {
			fails++
		}
	}
	if fails != 4 {
		t.Fatalf("step_up fail lines: %d, want 4", fails)
	}
}

// S1 test 12 (lockout): failed step-ups count toward the per-IP lockout.
func TestS1StepUpCountsTowardLockout(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	secret, _ := enroll2FA(t, ac, ts, cfg)
	raw := rawCookie(t, ac, ts)
	for i := 1; i <= 5; i++ {
		body := `{"password":"wrong"}`
		if i%2 == 0 {
			body = pwBody(cfg, "000000")
		}
		if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", body); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", `{"password":"wrong"}`); code != http.StatusTooManyRequests {
		t.Fatalf("sixth failure: %d, want 429", code)
	}
	if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, nextStepCode(t, secret))); code != http.StatusTooManyRequests {
		t.Fatalf("good step-up while locked: %d, want 429", code)
	}
	if !stepUpAt(t, srv, raw).IsZero() {
		t.Fatal("stamped while locked")
	}
}

// N3-1: step-up checks the lockout before the password: a locked address is
// refused even with the right credentials, and no password check runs
// (no step_up line is written).
func TestN31StepUpLockedAddressRefusedBeforePassword(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	raw := rawCookie(t, ac, ts)
	for i := 0; i < 6; i++ {
		login(t, ts, "wrong", "")
	}
	if code, _, _ := login(t, ts, cfg.AdminPassword, ""); code != http.StatusTooManyRequests {
		t.Fatalf("address not locked: %d", code)
	}
	code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, ""))
	if code != http.StatusTooManyRequests {
		t.Fatalf("step-up from a locked address: %d, want 429", code)
	}
	if !stepUpAt(t, srv, raw).IsZero() {
		t.Fatal("stamped while locked")
	}
	if n := countAudit(auditEvents(t, ac, ts), "step_up"); n != 0 {
		t.Fatalf("step_up lines: %d, want 0 (password not checked)", n)
	}
}

// N3-2: step-up refuses a recovery code before consumeSecondFactor writes
// anything (regen is covered by TestS1RegenNeedsPasswordAndTOTP).
func TestN32StepUpRecoveryCodeNotConsumed(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, ac, ts, cfg)
	before := twoFAStatus(t, ac, ts)["recoveryRemaining"]
	for _, c := range []string{codes[0], strings.ToUpper(codes[0]), strings.ReplaceAll(codes[0], "-", "")} {
		if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, c)); code != http.StatusUnauthorized {
			t.Fatalf("recovery code %q accepted: %d", "form", code)
		}
	}
	if after := twoFAStatus(t, ac, ts)["recoveryRemaining"]; after != before {
		t.Fatalf("recoveryRemaining %v -> %v", before, after)
	}
	// Still usable where recovery codes are accepted.
	if code, _, _ := login(t, ts, cfg.AdminPassword, codes[0]); code != http.StatusOK {
		t.Fatalf("recovery code after refused step-ups: %d", code)
	}
}

// S1 test 13: credentials are never read from a query or a GET, and the
// test password and code never appear in the audit or the log.
func TestS1StepUpNoCredentialsFromQuery(t *testing.T) {
	logs := captureSlog(t)
	ts, srv, cfg, clk := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	secret, _ := enroll2FA(t, ac, ts, cfg)
	raw := rawCookie(t, ac, ts)
	code := nextStepCode(t, secret)
	q := "password=" + url.QueryEscape(cfg.AdminPassword) + "&code=" + code

	if got, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/step-up?"+q, "", nil); got != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/step-up: %d, want 405", got)
	}
	clk.Advance(31 * time.Minute)
	if got, _ := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/keys?reveal=1&"+q, "", nil); got != http.StatusUnauthorized {
		t.Fatalf("reveal with query credentials: %d, want 401", got)
	}
	if got, _ := s1Do(t, ac, ts, cfg, http.MethodPost, "/admin/step-up?"+q, "{}", nil); got != http.StatusUnauthorized {
		t.Fatalf("POST step-up with query credentials only: %d, want 401", got)
	}
	if !stepUpAt(t, srv, raw).IsZero() {
		t.Fatal("stamped from a query string")
	}
	if got, b := postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, code)); got != http.StatusOK {
		t.Fatalf("step-up: %d %s", got, b)
	}

	_, auditJSON := s1Do(t, ac, ts, cfg, http.MethodGet, "/admin/audit", "", nil)
	for name, s := range map[string]string{"audit": auditJSON, "log": logs.String()} {
		if strings.Contains(s, cfg.AdminPassword) || strings.Contains(s, code) {
			t.Fatalf("test credential found in the %s", name)
		}
	}
}

// S1 test 14: with 2FA not enrolled step-up checks the password only.
func TestS1StepUpPasswordOnlyWhenNotEnrolled(t *testing.T) {
	for _, on := range []bool{false, true} {
		ts, srv, cfg, _ := s1Server(t, nil)
		ac := adminClient(t, ts, cfg)
		if on {
			setSwitch(t, srv, true) // switch on, nothing enrolled
		}
		raw := rawCookie(t, ac, ts)
		if code, _ := postJSON(t, ac, ts, cfg, "/admin/step-up", `{"password":"wrong"}`); code != http.StatusUnauthorized {
			t.Fatalf("wrong password: %d", code)
		}
		if code, b := postJSON(t, ac, ts, cfg, "/admin/step-up", `{"password":"`+cfg.AdminPassword+`"}`); code != http.StatusOK {
			t.Fatalf("password only (switch %v): %d %s", on, code, b)
		}
		if stepUpAt(t, srv, raw).IsZero() {
			t.Fatal("not stamped")
		}
	}
}

// N3-3: admin_key_use is aggregated per client address per minute.
func TestAuditAggregatorAdminKeyUse(t *testing.T) {
	var mu sync.Mutex
	var out []AuditEvent
	clk := &s1Clock{t: time.Unix(1_800_000_000, 0)}
	a := newAuditAggregator(time.Minute, 64, 1024, func(_ AuditStream, ev AuditEvent) {
		mu.Lock()
		out = append(out, ev)
		mu.Unlock()
	})
	a.now = clk.Now
	for i := 0; i < 1000; i++ {
		a.Record(AuditStreamHighValue, "192.0.2.7", auditEventOf("admin_key_use", "192.0.2.7", "ok", "/admin/status"))
	}
	if len(out) != 1 {
		t.Fatalf("lines in the first minute: %d, want 1", len(out))
	}
	a.Flush()
	if len(out) != 1 {
		t.Fatal("summary written before the window ended")
	}
	clk.Advance(time.Minute)
	a.Flush()
	if len(out) != 2 || !strings.Contains(out[1].Message, "repeated count=999") || out[1].IP != "192.0.2.7" {
		t.Fatalf("summary: %#v", out)
	}
	a.Record(AuditStreamHighValue, "192.0.2.7", auditEventOf("admin_key_use", "192.0.2.7", "ok", "/admin/status"))
	if len(out) != 3 {
		t.Fatal("first use in a new window not written")
	}
}

// N3-3: rotating the key cannot grow lines or memory past the cap.
func TestAuditAggregatorBoundedUnderKeyRotation(t *testing.T) {
	var out []AuditEvent
	clk := &s1Clock{t: time.Unix(1_800_000_000, 0)}
	a := newAuditAggregator(time.Minute, 64, 1024, func(_ AuditStream, ev AuditEvent) { out = append(out, ev) })
	a.now = clk.Now
	for i := 0; i < 10000; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)
		a.Record(AuditStreamHighValue, ip, auditEventOf("admin_key_use", ip, "ok", "/admin/keys"))
	}
	if len(out) != 65 || len(a.buckets) != 65 {
		t.Fatalf("lines %d buckets %d, want 65 (64 keys + other)", len(out), len(a.buckets))
	}
	clk.Advance(time.Minute)
	a.Flush()
	if len(out) != 66 || len(a.buckets) != 0 {
		t.Fatalf("after flush: lines %d buckets %d", len(out), len(a.buckets))
	}
	last := out[65]
	if last.IP != "" || !strings.Contains(last.Message, "admin_key_use other repeated count=9935") {
		t.Fatalf("other summary: %#v", last)
	}
}

// N3-3: a flood of admin_key_use and admin_session_ip_change cannot push a
// key_reveal line out of the audit ring.
func TestKeyRevealNotFloodedOut(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, func(c *config.Config) {
		c.MasterKeyAdmin = true
		c.TrustedProxyCIDRs = []string{"127.0.0.0/8"}
	})
	master := keymaker.MasterKey(cfg.APIKeySeed)
	c, tr := xffAdminClient(t, ts, cfg, "198.51.100.1")
	if got := httpCode(t, c, ts, "/admin/keys?reveal=1"); got != http.StatusOK {
		t.Fatalf("reveal: %d", got)
	}
	for i := 0; i < 600; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i%250)
		if i >= 250 {
			ip = fmt.Sprintf("192.0.2.%d", i%250)
		}
		tr.set(ip)
		if got := httpCode(t, c, ts, "/admin/2fa/status"); got != http.StatusOK { // session: address changes
			t.Fatalf("session request %d: %d", i, got)
		}
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/keys", nil)
		req.Header.Set("Authorization", "Bearer "+master)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	tr.set("198.51.100.1")
	evs := auditEvents(t, c, ts)
	if n := countAudit(evs, "key_reveal"); n != 1 {
		t.Fatalf("key_reveal lines after the flood: %d, want 1", n)
	}
	if n := countAudit(evs, "admin_key_use"); n > aggMaxKeys+1 {
		t.Fatalf("admin_key_use lines: %d, want <= %d", n, aggMaxKeys+1)
	}
	if n := countAudit(evs, "admin_session_ip_change"); n > 2 {
		t.Fatalf("admin_session_ip_change lines in one minute: %d, want <= 2", n)
	}
}

// recordingSink captures (stream, kind, result).
type recordingSink struct {
	mu  sync.Mutex
	got []string
}

func (r *recordingSink) WriteAudit(st AuditStream, ev AuditEvent) {
	r.mu.Lock()
	r.got = append(r.got, string(st)+":"+ev.Kind+":"+ev.Result)
	r.mu.Unlock()
}

// N3-3: events reach the AuditSink with their stream, so a persistent audit
// can route them.
func TestAuditSinkStreams(t *testing.T) {
	cases := map[[2]string]AuditStream{
		{"admin_key_use", "ok"}:           AuditStreamHighValue,
		{"admin_key_use", "blocked"}:      AuditStreamHighValue,
		{"admin_session_ip_change", "ok"}: AuditStreamHighValue,
		{"key_reveal", "ok"}:              AuditStreamHighValue,
		{"step_up", "ok"}:                 AuditStreamHighValue,
		{"step_up", "fail"}:               AuditStreamOps,
		{"login", "ok"}:                   AuditStreamHighValue,
		{"login", "fail"}:                 AuditStreamOps,
		{"login_lockout", "blocked"}:      AuditStreamOps,
	}
	for in, want := range cases {
		if got := AuditStreamFor(in[0], in[1]); got != want {
			t.Errorf("AuditStreamFor(%s, %s) = %s, want %s", in[0], in[1], got, want)
		}
	}

	ts, srv, cfg, _ := s1Server(t, nil)
	rs := &recordingSink{}
	srv.auditSink = rs
	login(t, ts, "wrong", "")
	ac := adminClient(t, ts, cfg)
	postJSON(t, ac, ts, cfg, "/admin/step-up", pwBody(cfg, ""))
	rs.mu.Lock()
	defer rs.mu.Unlock()
	want := []string{"ops:login:fail", "security:login:ok", "security:step_up:ok"}
	if strings.Join(rs.got, ",") != strings.Join(want, ",") {
		t.Fatalf("sink got %v, want %v", rs.got, want)
	}
}
