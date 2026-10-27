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

func (e *failoverEnv) admin(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
	for _, bad := range []string{"admin", "absolute", "master", "password", "seed", "key", "wallet", "cookie"} {
		if strings.Contains(all, bad) {
			t.Fatalf("settings expose %q: %s", bad, all)
		}
	}
	if len(v.Settings) != 5 {
		t.Fatalf("settings %v", names)
	}
}
