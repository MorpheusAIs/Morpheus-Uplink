package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

func intp(n int) *int { return &n }

// s6Env sets the required envs, then the given ones, and builds a config.
func s6Config(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	for _, k := range []string{"SESSION_DURATION_SECONDS", "UPLINK_SESSION_DURATION_MAX", "UPLINK_SESSION_IDLE_GRACE_SECONDS", "UPLINK_FORWARD_IDLE_SECONDS", "UPLINK_LOG_LEVEL", "UPLINK_DEV_HTTP", "COOKIE_CONTENT"} {
		t.Setenv(k, "")
	}
	t.Setenv("ADMIN_PASSWORD", "test-admin-pass-ok")
	t.Setenv("API_KEY_SEED", "s6-seed")
	t.Setenv("ROUTER_AUTH", "admin:routerpass")
	for k, v := range env {
		t.Setenv(k, v)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	c, err := config.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.DataDir = t.TempDir()
	return c
}

func viewOf(t *testing.T, s *Server, name string) settingView {
	t.Helper()
	for _, v := range s.settingsView()["settings"].([]settingView) {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no setting %s", name)
	return settingView{}
}

// S6: precedence matrix (env set / persisted set / both / neither) for
// every setting: env > persisted > default.
func TestS6PrecedenceMatrix(t *testing.T) {
	defer LogLevel.Set(slog.LevelInfo)
	type row struct {
		name, env, envVal string
		persist           func(*store.Settings)
		envWant, perWant  any
		def               any
	}
	rows := []row{
		{"sessionDurationDefault", "SESSION_DURATION_SECONDS", "900", func(s *store.Settings) { s.SessionDurationDefault = intp(1200) }, 900, 1200, 600},
		{"sessionDurationMax", "UPLINK_SESSION_DURATION_MAX", "3600", func(s *store.Settings) { s.SessionDurationMax = intp(2400) }, 3600, 2400, 1800},
		{"sessionIdleGrace", "UPLINK_SESSION_IDLE_GRACE_SECONDS", "120", func(s *store.Settings) { s.SessionIdleGrace = intp(900) }, 120, 900, 300},
		{"forwardIdle", "UPLINK_FORWARD_IDLE_SECONDS", "60", func(s *store.Settings) { s.ForwardIdle = intp(300) }, 60, 300, 180},
		{"logLevel", "UPLINK_LOG_LEVEL", "warn", func(s *store.Settings) { s.LogLevel = "debug" }, "warn", "debug", "info"},
	}
	for _, r := range rows {
		for _, mode := range []string{"neither", "env", "persisted", "both", "persisted-out-of-range"} {
			env := map[string]string{}
			if mode == "env" || mode == "both" {
				env[r.env] = r.envVal
			}
			cfg := s6Config(t, env)
			st, err := store.Open(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "persisted", "both":
				_ = st.UpdateSettings(func(s *store.Settings) error { r.persist(s); return nil })
			case "persisted-out-of-range":
				_ = st.UpdateSettings(func(s *store.Settings) error {
					r.persist(s)
					s.SessionDurationDefault, s.SessionDurationMax, s.SessionIdleGrace, s.ForwardIdle = intp(1), intp(1), intp(1), intp(99999)
					s.LogLevel = "loud"
					return nil
				})
			}
			srv := NewServer(cfg, st, nil, nil, nil, nil)
			v := viewOf(t, srv, r.name)
			want, src := r.def, "default"
			switch mode {
			case "env", "both":
				want, src = r.envWant, "env"
			case "persisted":
				want, src = r.perWant, "persisted"
			}
			if v.Value != want || v.Source != src || v.Pinned != (src == "env") {
				t.Errorf("%s/%s: value %v source %s pinned %v; want %v %s", r.name, mode, v.Value, v.Source, v.Pinned, want, src)
			}
		}
	}
}

// admin calls a console route on the admin cookie session (logged in
// once per env), with the same-origin headers a browser sends.
func (e *failoverEnv) admin(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	if e.console == nil {
		code, c, _ := login(t, e.ts, e.srv.cfg.AdminPassword, "")
		if code != http.StatusOK {
			t.Fatalf("console login %d", code)
		}
		e.console = c
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withCSRF(req, e.srv.cfg)
	resp, err := e.console.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// S6: changes apply live, persist (0640, atomic), are audited old→new,
// are validated with the env bounds, and env-pinned fields are refused.
func TestS6SettingsAPI(t *testing.T) {
	defer LogLevel.Set(slog.LevelInfo)
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) {
		c.MasterKeyAdmin = true
		c.EnvPinned = map[string]bool{"UPLINK_SESSION_IDLE_GRACE_SECONDS": true}
		c.SessionIdleGraceSec = 120
	})
	if code, body := e.admin(t, "GET", "/admin/settings", ""); code != 200 || !strings.Contains(body, `"name":"forwardIdle"`) {
		t.Fatalf("GET %d %s", code, body)
	}
	// forwardIdle 180 → 240, live in the next call's timers.
	if code, body := e.admin(t, "POST", "/admin/settings", `{"name":"forwardIdle","value":240}`); code != 200 {
		t.Fatalf("set %d %s", code, body)
	}
	k := e.newKey(t, "s6")
	if r := e.call(t, k, map[string][]string{"X-Uplink-Request-Timeout": {"600"}}, false); r.header.Get("X-Uplink-Timeouts") != "idle=240;total=600" {
		t.Fatalf("not live: %q", r.header.Get("X-Uplink-Timeouts"))
	}
	// Default and max reach the pool.
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"sessionDurationMax","value":3600}`); code != 200 {
		t.Fatal("max")
	}
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"sessionDurationDefault","value":900}`); code != 200 {
		t.Fatal("default")
	}
	if e.srv.pool.DurationSec() != 900 || e.srv.pool.DurationMax() != 3600 {
		t.Fatalf("pool %d/%d", e.srv.pool.DurationSec(), e.srv.pool.DurationMax())
	}
	// Bounds, cross-field, unknown, pinned.
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"name":"forwardIdle","value":29}`, 400},
		{`{"name":"forwardIdle","value":601}`, 400},
		{`{"name":"forwardIdle","value":"240"}`, 400},
		{`{"name":"sessionDurationDefault","value":4000}`, 400},
		{`{"name":"sessionDurationMax","value":800}`, 400},
		{`{"name":"logLevel","value":"loud"}`, 400},
		{`{"name":"adminSessionIdle","value":60}`, 400},
		{`{"name":"MASTER_KEY_ADMIN","value":1}`, 400},
		{`{"name":"forwardIdle","value":240,"extra":1}`, 400},
		{`{"name":"sessionIdleGrace","value":600}`, 409},
	} {
		if code, body := e.admin(t, "POST", "/admin/settings", c.body); code != c.code {
			t.Errorf("%s: %d %s", c.body, code, body)
		}
	}
	if e.SessionIdleGrace() != 120 {
		t.Fatal("pinned grace changed")
	}
	// Persisted beside AuthState in the state file, mode 0640.
	path := filepath.Join(e.srv.cfg.DataDir, "uplink-state.json")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("state file %v %v", fi, err)
	}
	st2, err := store.Open(e.srv.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if p := st2.GetSettings(); p.ForwardIdle == nil || *p.ForwardIdle != 240 || *p.SessionDurationDefault != 900 {
		t.Fatalf("not persisted: %+v", p)
	}
	// Reset drops the persisted value.
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"forwardIdle","reset":true}`); code != 200 || e.srv.forwardIdle() != 180 || viewOf(t, e.srv, "forwardIdle").Source != "default" {
		t.Fatal("reset")
	}
	// Audit: setting_change, name old→new, high-value stream.
	var got []string
	for _, ev := range e.srv.audit.Recent(1000) {
		if ev.Kind == "setting_change" {
			got = append(got, ev.Message)
			if AuditStreamFor(ev.Kind, ev.Result) != AuditStreamHighValue {
				t.Fatal("setting_change not high-value")
			}
		}
	}
	want := []string{"forwardIdle 180→240", "sessionDurationMax 1800→3600", "sessionDurationDefault 600→900", "forwardIdle 240→180"}
	if strings.Join(got, "|") != strings.Join(want, "|") && strings.Join(reverse(got), "|") != strings.Join(want, "|") {
		t.Fatalf("audit %v", got)
	}
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

func (e *failoverEnv) SessionIdleGrace() int { return e.srv.SessionIdleGrace() }

// S6: the log level applies without a restart.
func TestS6LogLevelLive(t *testing.T) {
	defer LogLevel.Set(slog.LevelInfo)
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.MasterKeyAdmin = true })
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: LogLevel}))
	lg.Debug("hidden-at-info")
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"logLevel","value":"debug"}`); code != 200 {
		t.Fatal("set debug")
	}
	lg.Debug("shown-at-debug")
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"logLevel","value":"error"}`); code != 200 {
		t.Fatal("set error")
	}
	lg.Warn("hidden-at-error")
	out := buf.String()
	if strings.Contains(out, "hidden-") || !strings.Contains(out, "shown-at-debug") || !lg.Enabled(context.Background(), slog.LevelError) {
		t.Fatalf("log output %q", out)
	}
}

// S6: the admin session timeouts (and MASTER_KEY_ADMIN, secrets) are not
// in the settings API.
func TestS6AdminTimeoutsAbsent(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.MasterKeyAdmin = true })
	_, body := e.admin(t, "GET", "/admin/settings", "")
	var v struct {
		Settings []settingView `json:"settings"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range v.Settings {
		names = append(names, s.Name+"/"+s.Env)
	}
	all := strings.ToLower(strings.Join(names, " "))
	for _, bad := range []string{"absolute", "adminsession", "admin_session", "password", "seed", "wallet", "cookie", "eth_node", "rpc_url", "router_auth"} {
		if strings.Contains(all, bad) {
			t.Fatalf("settings expose %q: %s", bad, all)
		}
	}
	// MASTER_KEY_ADMIN is shown read-only; it can never be set here.
	for _, s := range v.Settings {
		if (strings.Contains(strings.ToLower(s.Name), "admin") || strings.Contains(strings.ToLower(s.Name), "master")) && (s.Settable || s.Scope != "env") {
			t.Fatalf("%s is settable", s.Name)
		}
	}
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"masterKeyAdmin","value":false}`); code != 400 || !e.srv.cfg.MasterKeyAdmin {
		t.Fatalf("masterKeyAdmin set: %d", code)
	}
	if len(v.Settings) != len(config.Settings) {
		t.Fatalf("settings %v", names)
	}
}

// MUST-1: the duration cap is never raised. An env default above the env
// max, or a persisted default above an env-pinned max, is clamped down to
// the max (with a warning and an ops note); the pool opens at most the max.
func TestS6DurationCapNeverRaised(t *testing.T) {
	defer LogLevel.Set(slog.LevelInfo)
	cases := []struct {
		name    string
		env     map[string]string
		persist func(*store.Settings)
	}{
		{"env-over-env", map[string]string{"SESSION_DURATION_SECONDS": "7200", "UPLINK_SESSION_DURATION_MAX": "1800"}, nil},
		{"persisted-over-env-pin", map[string]string{"UPLINK_SESSION_DURATION_MAX": "1800"}, func(s *store.Settings) { s.SessionDurationDefault = intp(3600) }},
		{"env-default-over-persisted-max", map[string]string{"SESSION_DURATION_SECONDS": "1800"}, func(s *store.Settings) { s.SessionDurationMax = intp(900) }},
	}
	for _, c := range cases {
		cfg := s6Config(t, c.env)
		st, err := store.Open(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		if c.persist != nil {
			_ = st.UpdateSettings(func(s *store.Settings) error { c.persist(s); return nil })
		}
		srv := NewServer(cfg, st, nil, nil, nil, nil)
		mx := srv.durationMax()
		if c.name != "env-default-over-persisted-max" && mx != 1800 {
			t.Errorf("%s: max raised to %d", c.name, mx)
		}
		if srv.durationDefault() > mx {
			t.Errorf("%s: default %d above max %d", c.name, srv.durationDefault(), mx)
		}
	}
}

// MUST-1 (live): a no-header call never opens longer than the max, and the
// console cannot set a default above the max.
func TestS6NoHeaderOpenCapped(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.MasterKeyAdmin = true })
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"sessionDurationDefault","value":3600}`); code != 400 {
		t.Fatalf("default above max accepted: %d", code)
	}
	e.srv.pool.SetDuration(3600) // as if a stale default slipped past
	k := e.newKey(t, "cap")
	if r := e.call(t, k, nil, false); r.code != 200 {
		t.Fatalf("call %d", r.code)
	}
	opens, _ := m.snapshot()
	if len(opens) == 0 {
		t.Fatal("no open")
	}
	for _, o := range opens {
		if d, _ := o.body["sessionDuration"].(float64); d <= 0 || d > 1800 {
			t.Fatalf("opened %v s with no header, cap 1800", o.body["sessionDuration"])
		}
	}
}

// Conformance NIT (d): a persisted value ignored at boot also writes an
// ops-stream event (name only).
func TestS6IgnoredPersistedValueAuditedOps(t *testing.T) {
	cfg := s6Config(t, nil)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpdateSettings(func(s *store.Settings) error { s.ForwardIdle = intp(5); return nil })
	srv := NewServer(cfg, st, nil, nil, nil, nil)
	found := false
	for _, ev := range srv.audit.Recent(100) {
		if ev.Kind == "setting_ignored" {
			found = true
			if AuditStreamFor(ev.Kind, ev.Result) != AuditStreamOps || !strings.Contains(ev.Message, "forwardIdle") || strings.Contains(ev.Message, "5") {
				t.Fatalf("event %+v", ev)
			}
		}
	}
	if !found {
		t.Fatal("no setting_ignored event")
	}
}

// Security gate S-1: the operator settings are cookie-only. Any Authorization
// header (the master Bearer with MASTER_KEY_ADMIN=true included) is 403;
// a cross-site request is 403; the cookie session is 200.
func TestS6SettingsCookieOnly(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.MasterKeyAdmin = true })
	do := func(method, body string, hdr map[string]string, c *http.Client) int {
		t.Helper()
		req, _ := http.NewRequest(method, e.ts.URL+"/admin/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if c == nil {
			c = http.DefaultClient
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	set := `{"name":"forwardIdle","value":240}`
	same := map[string]string{"Origin": e.srv.cfg.WebPublicURL, "Sec-Fetch-Site": "same-origin"}
	for _, method := range []string{"GET", "POST"} {
		bearer := map[string]string{"Authorization": "Bearer " + e.key, "Origin": e.srv.cfg.WebPublicURL, "Sec-Fetch-Site": "same-origin"}
		if code := do(method, set, bearer, nil); code != 403 {
			t.Errorf("%s master Bearer: %d, want 403", method, code)
		}
	}
	if e.srv.forwardIdle() != 180 {
		t.Fatal("Bearer changed a setting")
	}
	e.admin(t, "GET", "/admin/settings", "") // logs the console in
	// The cookie with any Authorization header is still refused.
	withBearer := map[string]string{"Authorization": "Bearer x", "Origin": e.srv.cfg.WebPublicURL, "Sec-Fetch-Site": "same-origin"}
	if code := do("POST", set, withBearer, e.console); code != 403 {
		t.Errorf("cookie + Authorization: %d", code)
	}
	cross := map[string]string{"Origin": "https://elsewhere.example", "Sec-Fetch-Site": "cross-site"}
	for _, method := range []string{"GET", "POST"} {
		if code := do(method, set, cross, e.console); code != 403 {
			t.Errorf("%s cross-site: %d, want 403", method, code)
		}
	}
	if code := do("POST", set, map[string]string{"Sec-Fetch-Site": "same-origin"}, e.console); code != 403 {
		t.Errorf("POST without Origin: %d", code)
	}
	if e.srv.forwardIdle() != 180 {
		t.Fatal("refused request changed a setting")
	}
	if code := do("GET", "", same, e.console); code != 200 {
		t.Errorf("cookie GET: %d", code)
	}
	if code := do("POST", set, same, e.console); code != 200 || e.srv.forwardIdle() != 240 {
		t.Errorf("cookie POST: %d forwardIdle %d", code, e.srv.forwardIdle())
	}
	if code := do("GET", "", nil, nil); code != 401 {
		t.Errorf("no session: %d", code)
	}
}

// Security gate N-2: the settings body is one JSON object; trailing data is 400.
func TestS6SettingsRejectsTrailingJSON(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	for _, body := range []string{
		`{"name":"forwardIdle","value":240}{"name":"forwardIdle","value":300}`,
		`{"name":"forwardIdle","value":240} x`,
	} {
		if code, _ := e.admin(t, "POST", "/admin/settings", body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if e.srv.forwardIdle() != 180 {
		t.Fatalf("trailing body applied: %d", e.srv.forwardIdle())
	}
	if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"forwardIdle","value":240}`+"\n"); code != 200 {
		t.Fatalf("trailing newline: %d", code)
	}
}

// Security gate S-2: a console log level of warn or error hides runtime info logs
// but never audit lines (setting_change included).
func TestS6AuditLinesIgnoreLogLevel(t *testing.T) {
	defer LogLevel.Set(slog.LevelInfo)
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: LogLevel})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	for _, lvl := range []string{"warn", "error"} {
		if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"logLevel","value":"`+lvl+`"}`); code != 200 {
			t.Fatalf("set %s: %d", lvl, code)
		}
		mu.Lock()
		buf.Reset()
		mu.Unlock()
		slog.Info("runtime info line")
		if code, _ := e.admin(t, "POST", "/admin/settings", `{"name":"forwardIdle","reset":true}`); code != 200 {
			t.Fatalf("reset: %d", code)
		}
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if strings.Contains(out, "runtime info line") {
			t.Errorf("%s: info line not filtered", lvl)
		}
		if !strings.Contains(out, "event=setting_change") || !strings.Contains(out, "msg=audit") {
			t.Errorf("%s: audit line hidden: %q", lvl, out)
		}
	}
}
