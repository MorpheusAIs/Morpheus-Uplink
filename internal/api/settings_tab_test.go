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
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// settingsEnv is a console server built from FromEnv (every registry env
// cleared first) with a fake session clock and a logged-in cookie client.
type settingsEnv struct {
	ts  *httptest.Server
	srv *Server
	cfg *config.Config
	st  *store.Store
	c   *http.Client
	clk *s1Clock
}

func newSettingsEnv(t *testing.T, env map[string]string, persist map[string]string) *settingsEnv {
	t.Helper()
	return newSettingsEnvPrep(t, env, persist, nil)
}

// newSettingsEnvPrep is newSettingsEnv with prep run on the server before
// the test server starts listening (for fields set before serving).
func newSettingsEnvPrep(t *testing.T, env map[string]string, persist map[string]string, prep func(*Server)) *settingsEnv {
	t.Helper()
	for _, d := range config.Settings {
		t.Setenv(d.Env, "")
	}
	cfg := s6Config(t, env)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if persist != nil {
		_ = st.UpdateSettings(func(s *store.Settings) error {
			for k, v := range persist {
				v := v
				if err := s.SetValue(k, &v); err != nil {
					return err
				}
			}
			return nil
		})
	}
	config.ApplyPersisted(cfg, st.GetSettings().All())
	srv := NewServer(cfg, st, nil, nil, nil, nil)
	clk := &s1Clock{t: time.Now()}
	srv.sessions.SetClock(clk.Now)
	ts := httptest.NewUnstartedServer(srv.Handler())
	if cfg.WebPublicURL == "" {
		cfg.WebPublicURL = "http://" + ts.Listener.Addr().String()
	}
	if prep != nil {
		prep(srv)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	code, c, _ := login(t, ts, cfg.AdminPassword, "")
	if code != http.StatusOK {
		t.Fatalf("login %d", code)
	}
	return &settingsEnv{ts: ts, srv: srv, cfg: cfg, st: st, c: c, clk: clk}
}

// do sends a same-origin console request; hdr overrides headers ("" deletes).
func (e *settingsEnv) do(t *testing.T, c *http.Client, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withCSRF(req, e.cfg)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	if c == nil {
		c = e.c
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (e *settingsEnv) view(t *testing.T) map[string]settingView {
	t.Helper()
	code, body := e.do(t, nil, "GET", "/admin/settings", "", nil)
	if code != 200 {
		t.Fatalf("GET settings %d", code)
	}
	var v struct {
		Settings []settingView `json:"settings"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	out := map[string]settingView{}
	for i, s := range v.Settings {
		if s.Name != config.Settings[i].Name {
			t.Fatalf("row %d is %s, want registry order", i, s.Name)
		}
		out[s.Name] = s
	}
	return out
}

func (e *settingsEnv) audits(kind string) []string {
	var out []string
	for _, ev := range e.srv.audit.Recent(1000) {
		if ev.Kind == kind {
			out = append(out, ev.Message)
		}
	}
	return out
}

// The view lists every registry row with its type, default, range,
// scope and restart flag; router and env-only rows are read-only.
func TestSettingsTabView(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	v := e.view(t)
	if len(v) != len(config.Settings) {
		t.Fatalf("%d rows", len(v))
	}
	for _, d := range config.Settings {
		r := v[d.Name]
		if r.Env != d.Env || r.Kind != string(d.Kind) || r.Scope != string(d.Scope) || r.Help != d.Help || r.Settable != d.Settable() || r.Min != d.Min || r.Max != d.Max {
			t.Errorf("%s: %+v", d.Name, r)
		}
		wantRestart := map[config.SettingScope]string{config.ScopeLive: "", config.ScopeRestart: "uplink", config.ScopeEnvOnly: "uplink", config.ScopeRouter: "router"}[d.Scope]
		if r.Restart != wantRestart {
			t.Errorf("%s: restart %q, want %q", d.Name, r.Restart, wantRestart)
		}
		if d.Scope == config.ScopeRouter && (r.Value != nil || r.Source != "router") {
			t.Errorf("%s: router row shows a value %v", d.Name, r.Value)
		}
	}
	if r := v["auditRetentionDays"]; r.Value != float64(90) || r.Default != float64(90) || r.Source != "default" || r.PendingRestart {
		t.Errorf("auditRetentionDays %+v", r)
	}
	if r := v["sessionFailover"]; r.Value != true || r.Default != true {
		t.Errorf("sessionFailover %+v", r)
	}
	// The release endpoint's control: a settable restart-scope toggle.
	if r := v["sessionRelease"]; r.Kind != "bool" || r.Value != true || r.Default != true || !r.Settable || r.Restart != "uplink" || r.Group != "release" {
		t.Errorf("sessionRelease %+v", r)
	}
	for name, r := range v {
		if l := strings.ToLower(name + " " + r.Env); strings.Contains(l, "inflight") || strings.Contains(l, "long") || strings.Contains(l, "opens_per") {
			t.Errorf("removed limit on the Settings tab: %s", name)
		}
	}
	if r := v["webPublicUrl"]; !r.PendingRestart && r.Effective == "" {
		t.Errorf("webPublicUrl %+v", r)
	}
}

// A restart-scope change is validated, stored, audited and shown as
// pending; the next start (same DATA_DIR) runs with it.
func TestSettingsRestartScopePersists(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	bad := []string{
		`{"name":"auditRetentionDays","value":0}`,
		`{"name":"auditRetentionDays","value":3651}`,
		`{"name":"auditRetentionDays","value":"many"}`,
		`{"name":"sessionFailover","value":"maybe"}`,
		`{"name":"providerExclude","value":"m1=0x12"}`,
		`{"name":"providerExclude","value":"0x12"}`,
		`{"name":"providerExclude","value":"0x` + strings.Repeat("a", 40) + `,nope"}`,
		`{"name":"activeModelsUrl","value":"http://feed.example/m.json"}`,
		`{"name":"webPublicUrl","value":"http://console.example"}`,
		`{"name":"auditRetentionDays","value":{"n":1}}`,
		`{"name":"auditRetentionDays","value":null}`,
		`{"name":"routerLogLevelApp","value":"debug"}`,
		`{"name":"proxyStoreChatContext","value":true}`,
		`{"name":"masterKeyAdmin","value":true}`,
	}
	for _, b := range bad {
		if code, _ := e.do(t, nil, "POST", "/admin/settings", b, nil); code != 400 {
			t.Errorf("%s: %d, want 400", b, code)
		}
	}
	if all := e.st.GetSettings().All(); len(all) != 0 {
		t.Fatalf("refused values stored: %v", all)
	}
	for _, b := range []string{`{"name":"auditRetentionDays","value":400}`, `{"name":"sessionFailover","value":false}`, `{"name":"providerExclude","value":"0x` + strings.Repeat("a", 40) + `"}`, `{"name":"housekeeping","value":"false"}`, `{"name":"housekeeping","reset":true}`} {
		if code, body := e.do(t, nil, "POST", "/admin/settings", b, nil); code != 200 {
			t.Fatalf("%s: %d %s", b, code, body)
		}
	}
	v := e.view(t)
	if r := v["auditRetentionDays"]; r.Value != float64(400) || r.Effective != float64(90) || !r.PendingRestart || r.Source != "persisted" {
		t.Fatalf("auditRetentionDays %+v", r)
	}
	if r := v["housekeeping"]; r.Source != "default" || r.PendingRestart {
		t.Fatalf("housekeeping after reset %+v", r)
	}
	if e.cfg.AuditRetentionDays != 90 || !e.cfg.SessionFailover {
		t.Fatal("a restart-scope change applied live")
	}
	got := strings.Join(e.audits("setting_change"), "|")
	if !strings.Contains(got, "auditRetentionDays 90→400 (restart)") || !strings.Contains(got, "providerExclude changed (restart)") || strings.Contains(got, "0xaaaa") {
		t.Fatalf("audit %s", got)
	}
	// Next start on the same DATA_DIR.
	st2, err := store.Open(e.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := s6Config(t, nil)
	cfg2.DataDir = e.cfg.DataDir
	config.ApplyPersisted(cfg2, st2.GetSettings().All())
	if cfg2.AuditRetentionDays != 400 || cfg2.SessionFailover || !cfg2.Housekeeping || len(cfg2.ProviderExclude) != 1 {
		t.Fatalf("restart did not apply: %+v", cfg2)
	}
	srv2 := NewServer(cfg2, st2, nil, nil, nil, nil)
	if r := viewOf(t, srv2, "auditRetentionDays"); r.Value != 400 || r.Effective != 400 || r.PendingRestart {
		t.Fatalf("after restart %+v", r)
	}
}

// Console settings are not read from the environment: a former variable
// is ignored (saved > default), the row stays settable and is never
// pinned; an env-only row still reports the environment as its source.
func TestSettingsEnvIgnoredSavedWins(t *testing.T) {
	e := newSettingsEnv(t, map[string]string{"AUDIT_RETENTION_DAYS": "30", "SESSION_FAILOVER": "false", "MASTER_KEY_ADMIN": "false"},
		map[string]string{"auditRetentionDays": "400"})
	if e.cfg.AuditRetentionDays != 400 || !e.cfg.SessionFailover {
		t.Fatalf("env applied: %d %v", e.cfg.AuditRetentionDays, e.cfg.SessionFailover)
	}
	if strings.Join(e.cfg.IgnoredEnv, ",") != "SESSION_FAILOVER,AUDIT_RETENTION_DAYS" {
		t.Fatalf("ignored %v", e.cfg.IgnoredEnv)
	}
	v := e.view(t)
	for n, src := range map[string]string{"auditRetentionDays": "persisted", "sessionFailover": "default", "masterKeyAdmin": "env"} {
		if r := v[n]; r.Pinned || r.Source != src || r.PendingRestart {
			t.Errorf("%s %+v", n, r)
		}
	}
	if v["auditRetentionDays"].Value != float64(400) || !v["auditRetentionDays"].Settable || v["masterKeyAdmin"].Settable {
		t.Errorf("rows %+v / %+v", v["auditRetentionDays"], v["masterKeyAdmin"])
	}
	for _, b := range []string{`{"name":"auditRetentionDays","value":20}`, `{"name":"sessionFailover","value":false}`} {
		if code, body := e.do(t, nil, "POST", "/admin/settings", b, nil); code != 200 {
			t.Errorf("%s: %d %s", b, code, body)
		}
	}
	if v, _ := e.st.GetSettings().Value("auditRetentionDays"); v != "20" {
		t.Fatalf("store %q", v)
	}
	if code, _ := e.do(t, nil, "POST", "/admin/settings", `{"name":"auditRetentionDays","reset":true}`, nil); code != 200 {
		t.Fatal("reset")
	}
	if v := e.view(t)["auditRetentionDays"]; v.Value != float64(90) || v.Effective != float64(400) || !v.PendingRestart {
		t.Fatalf("after reset %+v", v)
	}
}

// A live setting set through the old variable is ignored; one saved in the
// console applies; with neither the default applies.
func TestSettingsLiveEnvIgnored(t *testing.T) {
	e := newSettingsEnv(t, map[string]string{"UPLINK_FORWARD_IDLE_SECONDS": "90", "SESSION_DURATION_SECONDS": "900"}, map[string]string{"forwardIdle": "240"})
	if e.srv.forwardIdle() != 240 || e.srv.durationDefault() != 600 {
		t.Fatalf("forwardIdle %d duration %d", e.srv.forwardIdle(), e.srv.durationDefault())
	}
	v := e.view(t)
	if v["forwardIdle"].Source != "persisted" || v["sessionDurationDefault"].Source != "default" || v["forwardIdle"].Pinned {
		t.Fatalf("%+v / %+v", v["forwardIdle"], v["sessionDurationDefault"])
	}
}

// Secrets can never be set, read or exported.
func TestSettingsSecretsNeverExposed(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	// A hand-edited state file with secret-looking keys.
	_ = e.st.UpdateSettings(func(s *store.Settings) error {
		for _, k := range []string{"WALLET_PRIVATE_KEY", "adminPassword", "apiKeySeed"} {
			v := "planted-" + k
			_ = s.SetValue(k, &v)
		}
		return nil
	})
	names := append(append([]string{}, config.SecretEnv...), "walletPrivateKey", "ethNodeAddress", "cookieContent", "routerAuth", "adminPassword", "apiKeySeed", "masterKey", "promptKey")
	for _, n := range names {
		if code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"`+n+`","value":"x"}`, nil); code != 400 || !strings.Contains(body, "secrets are set in .env only") {
			t.Errorf("set %s: %d %s", n, code, body)
		}
		file := `{"format":"uplink-settings","version":1,"settings":{"` + n + `":"x"}}`
		if code, body := e.do(t, nil, "POST", "/admin/settings/import", file, nil); code != 400 || !strings.Contains(body, "secrets are set in .env only") {
			t.Errorf("import %s: %d %s", n, code, body)
		}
	}
	_, view := e.do(t, nil, "GET", "/admin/settings", "", nil)
	code, export := e.do(t, nil, "GET", "/admin/settings/export", "", nil)
	if code != 200 {
		t.Fatalf("export %d", code)
	}
	for _, body := range []string{view, export} {
		low := strings.ToLower(body)
		for _, bad := range []string{"planted-", strings.ToLower(e.cfg.AdminPassword), strings.ToLower(e.cfg.APIKeySeed), "routerpass", strings.ToLower(e.srv.masterKey), "wallet_private_key", "api_key_seed", "admin_password", "cookie_content", "router_auth", "eth_node"} {
			if bad != "" && strings.Contains(low, bad) {
				t.Fatalf("settings output has %q", bad)
			}
		}
	}
}

// Export and import: cookie session, CSRF, no Authorization header, and a
// fresh step-up, as for a key reveal.
func TestSettingsFileAuth(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	file := `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":240}}`
	routes := []struct{ method, path, body string }{{"GET", "/admin/settings/export", ""}, {"POST", "/admin/settings/import", file}}
	for _, r := range routes {
		if code, _ := e.do(t, http.DefaultClient, r.method, r.path, r.body, nil); code != 401 {
			t.Errorf("%s no session: %d", r.path, code)
		}
		for _, auth := range []string{"Bearer " + e.srv.masterKey, "Bearer x", "Basic eDp5"} {
			if code, _ := e.do(t, nil, r.method, r.path, r.body, map[string]string{"Authorization": auth}); code != 403 {
				t.Errorf("%s with Authorization: %d", r.path, code)
			}
		}
		if code, _ := e.do(t, nil, r.method, r.path, r.body, map[string]string{"Origin": "https://elsewhere.example", "Sec-Fetch-Site": "cross-site"}); code != 403 {
			t.Errorf("%s cross-site: %d", r.path, code)
		}
	}
	if code, _ := e.do(t, nil, "POST", "/admin/settings/import", file, map[string]string{"Origin": ""}); code != 403 {
		t.Errorf("import without Origin: %d", code)
	}
	if e.srv.forwardIdle() != 180 {
		t.Fatal("a refused import applied")
	}
	// Login is fresh for 30 minutes; after that both need a step-up.
	e.clk.Advance(StepUpMaxAge + time.Minute)
	for _, r := range routes {
		if code, body := e.do(t, nil, r.method, r.path, r.body, nil); code != 401 || strings.TrimSpace(body) != errMsgStepUp {
			t.Errorf("%s stale: %d %s", r.path, code, body)
		}
	}
	if code, body := e.do(t, nil, "POST", "/admin/step-up", `{"password":"`+e.cfg.AdminPassword+`","code":""}`, nil); code != 200 {
		t.Fatalf("step-up %d %s", code, body)
	}
	for _, r := range routes {
		if code, body := e.do(t, nil, r.method, r.path, r.body, nil); code != 200 {
			t.Errorf("%s after step-up: %d %s", r.path, code, body)
		}
	}
	if code, _ := e.do(t, nil, "GET", "/admin/settings/import", "", nil); code != 405 {
		t.Errorf("GET import: %d", code)
	}
	if len(e.audits("settings_export")) == 0 || len(e.audits("settings_import")) == 0 {
		t.Fatal("export/import not audited")
	}
}

// Export then import round-trips; a bad file (out of range, unknown key,
// secret, read-only, wrong type, wrong format, trailing data, default
// above max) is refused whole and changes nothing.
func TestSettingsExportImport(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	for _, b := range []string{`{"name":"forwardIdle","value":240}`, `{"name":"auditRetentionDays","value":400}`, `{"name":"logLevel","value":"warn"}`, `{"name":"providerExclude","value":"0x` + strings.Repeat("a", 40) + `"}`} {
		if code, _ := e.do(t, nil, "POST", "/admin/settings", b, nil); code != 200 {
			t.Fatalf("%s: %d", b, code)
		}
	}
	defer LogLevel.Set(0)
	code, export := e.do(t, nil, "GET", "/admin/settings/export", "", nil)
	if code != 200 {
		t.Fatalf("export %d", code)
	}
	var f struct {
		Format   string         `json:"format"`
		Version  int            `json:"version"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal([]byte(export), &f); err != nil || f.Format != "uplink-settings" || f.Version != 1 || len(f.Settings) != 4 || f.Settings["forwardIdle"] != float64(240) || f.Settings["auditRetentionDays"] != float64(400) {
		t.Fatalf("export %s", export)
	}
	before := e.st.GetSettings().All()
	bad := map[string]string{
		"out of range":   `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":240,"auditRetentionDays":5000}}`,
		"unknown":        `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":200,"turbo":true}}`,
		"secret":         `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":200,"ADMIN_PASSWORD":"x"}}`,
		"router":         `{"format":"uplink-settings","version":1,"settings":{"routerLogLevelTcp":"debug"}}`,
		"env only":       `{"format":"uplink-settings","version":1,"settings":{"masterKeyAdmin":true}}`,
		"wrong type":     `{"format":"uplink-settings","version":1,"settings":{"sessionFailover":[true]}}`,
		"bad bool":       `{"format":"uplink-settings","version":1,"settings":{"housekeeping":"sometimes"}}`,
		"bad level":      `{"format":"uplink-settings","version":1,"settings":{"logLevel":"loud"}}`,
		"default > max":  `{"format":"uplink-settings","version":1,"settings":{"sessionDurationDefault":3600,"sessionDurationMax":1200}}`,
		"format":         `{"format":"other","version":1,"settings":{}}`,
		"version":        `{"format":"uplink-settings","version":2,"settings":{}}`,
		"no settings":    `{"format":"uplink-settings","version":1}`,
		"extra field":    `{"format":"uplink-settings","version":1,"settings":{},"x":1}`,
		"trailing":       `{"format":"uplink-settings","version":1,"settings":{}} {}`,
		"not json":       `forwardIdle=200`,
		"oversized file": `{"format":"uplink-settings","version":1,"settings":{"providerExclude":"` + strings.Repeat("a", 70<<10) + `"}}`,
	}
	for name, b := range bad {
		if code, body := e.do(t, nil, "POST", "/admin/settings/import", b, nil); code != 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if code, body := e.do(t, nil, "POST", "/admin/settings/import", bad["unknown"], nil); !strings.Contains(body, "unknown setting: turbo") || code != 400 {
		t.Errorf("unknown key message %q", body)
	}
	after := e.st.GetSettings().All()
	if len(after) != len(before) || after["forwardIdle"] != "240" || e.srv.forwardIdle() != 240 {
		t.Fatalf("a refused import changed settings: %v", after)
	}
	// A valid file replaces the persisted set: live values apply now,
	// restart ones are pending, absent ones fall back to the default.
	ok := `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":"300","sessionIdleGrace":120,"auditRetentionDays":120,"housekeeping":false}}`
	if code, body := e.do(t, nil, "POST", "/admin/settings/import", ok, nil); code != 200 {
		t.Fatalf("import %d %s", code, body)
	}
	if e.srv.forwardIdle() != 300 || e.srv.SessionIdleGrace() != 120 {
		t.Fatalf("live values not applied: %d %d", e.srv.forwardIdle(), e.srv.SessionIdleGrace())
	}
	v := e.view(t)
	if v["logLevel"].Source != "default" || v["providerExclude"].Source != "default" || v["auditRetentionDays"].Value != float64(120) || !v["auditRetentionDays"].PendingRestart || v["housekeeping"].Value != false {
		t.Fatalf("after import %+v %+v %+v", v["logLevel"], v["providerExclude"], v["auditRetentionDays"])
	}
	// Re-importing the first export restores it.
	if code, body := e.do(t, nil, "POST", "/admin/settings/import", export, nil); code != 200 {
		t.Fatalf("re-import %d %s", code, body)
	}
	if got := e.st.GetSettings().All(); len(got) != 4 || got["forwardIdle"] != "240" || got["logLevel"] != "warn" {
		t.Fatalf("round trip %v", got)
	}
}

// An import applies even when the former env variable is set (it is
// ignored); the answer keeps its "pinned" field, now always null.
func TestSettingsImportIgnoresEnv(t *testing.T) {
	e := newSettingsEnv(t, map[string]string{"UPLINK_FORWARD_IDLE_SECONDS": "90"}, nil)
	code, body := e.do(t, nil, "POST", "/admin/settings/import", `{"format":"uplink-settings","version":1,"settings":{"forwardIdle":240}}`, nil)
	if code != 200 || !strings.Contains(body, `"pinned":null`) || e.srv.forwardIdle() != 240 {
		t.Fatalf("%d %s fwd %d", code, body, e.srv.forwardIdle())
	}
}

// webPublicUrl (the admin origin anchor) and the two feed URLs are env-only:
// a console set and an import are refused, and a value found in the
// persisted file is ignored at boot and audited as setting_ignored.
func TestSettingsEnvOnlyURLsRefused(t *testing.T) {
	rows := map[string]string{
		"webPublicUrl":    "https://console.example",
		"activeModelsUrl": "https://feed.example/m.json",
		"gatewayBidsUrl":  "https://feed.example/b.json",
	}
	e := newSettingsEnv(t, nil, rows)
	if e.cfg.ActiveModelsURL != "https://active.mor.org/gateway_models.json" || e.cfg.GatewayBidsURL != "https://active.mor.org/gateway_bids.json" || e.cfg.WebPublicURL == rows["webPublicUrl"] {
		t.Fatalf("a persisted env-only value was applied: %q %q %q", e.cfg.ActiveModelsURL, e.cfg.GatewayBidsURL, e.cfg.WebPublicURL)
	}
	ignored := strings.Join(e.audits("setting_ignored"), ";")
	view := e.view(t)
	for name, val := range rows {
		if !strings.Contains(ignored, name) {
			t.Errorf("%s: persisted value not audited as ignored: %q", name, ignored)
		}
		if r := view[name]; r.Settable || r.Scope != "env" || r.Value == val || r.PendingRestart {
			t.Errorf("%s view %+v", name, r)
		}
		set := `{"name":"` + name + `","value":"` + val + `"}`
		file := `{"format":"uplink-settings","version":1,"settings":{"` + name + `":"` + val + `"}}`
		for path, body := range map[string]string{"/admin/settings": set, "/admin/settings/import": file} {
			if code, msg := e.do(t, nil, "POST", path, body, nil); code != 400 || !strings.Contains(msg, name+" is set in .env only") {
				t.Errorf("%s %s: %d %q", path, name, code, msg)
			}
		}
		if code, _ := e.do(t, nil, "POST", "/admin/settings", `{"name":"`+name+`","reset":true}`, nil); code != 400 {
			t.Errorf("reset %s: %d", name, code)
		}
	}
	if len(e.audits("setting_change")) != 0 || len(e.audits("settings_import")) != 0 {
		t.Fatal("a refused env-only write was audited as a change")
	}
}

// A restart-scope single-row write needs a fresh step-up, like import;
// a live row does not.
func TestSettingsSetRestartNeedsStepUp(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	e.clk.Advance(StepUpMaxAge + time.Minute)
	for _, b := range []string{`{"name":"auditRetentionDays","value":1}`, `{"name":"sessionRelease","value":false}`, `{"name":"providerExclude","reset":true}`} {
		if code, body := e.do(t, nil, "POST", "/admin/settings", b, nil); code != 401 || strings.TrimSpace(body) != errMsgStepUp {
			t.Errorf("%s stale: %d %s", b, code, body)
		}
	}
	if _, ok := e.st.GetSettings().Value("auditRetentionDays"); ok {
		t.Fatal("a stale-session write was saved")
	}
	if code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"forwardIdle","value":240}`, nil); code != 200 || e.srv.forwardIdle() != 240 {
		t.Fatalf("live row without step-up: %d %s", code, body)
	}
	if code, body := e.do(t, nil, "POST", "/admin/step-up", `{"password":"`+e.cfg.AdminPassword+`","code":""}`, nil); code != 200 {
		t.Fatalf("step-up %d %s", code, body)
	}
	if code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"auditRetentionDays","value":120}`, nil); code != 200 {
		t.Fatalf("after step-up: %d %s", code, body)
	}
	if v, _ := e.st.GetSettings().Value("auditRetentionDays"); v != "120" {
		t.Fatalf("saved %q", v)
	}
}

// providerExclude is a flat address list: a save stores it lowercase,
// deduped and sorted, and a value in the former per-model form is refused
// with the message to re-enter it (nothing is converted or saved).
func TestSettingsProviderExcludeFlatList(t *testing.T) {
	e := newSettingsEnv(t, nil, nil)
	a, b := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	for _, old := range []string{"m1=" + a, "m1=" + a + "; m2=" + b, a + ";" + b} {
		code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"providerExclude","value":"`+old+`"}`, nil)
		if code != 400 || !strings.Contains(body, "format changed") || !strings.Contains(body, "re-enter it on the Settings tab") || strings.Contains(body, a) {
			t.Fatalf("%q: %d %s", old, code, body)
		}
	}
	if v := e.view(t)["providerExclude"]; v.Source != "default" || v.Value != "" {
		t.Fatalf("old form saved: %+v", v)
	}
	in := " 0x" + strings.Repeat("B", 40) + " , " + a + ",," + strings.ToUpper(a[2:3]) + "," + b
	if code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"providerExclude","value":"`+in+`"}`, nil); code != 400 {
		t.Fatalf("bad entry accepted: %d %s", code, body)
	}
	in = " 0x" + strings.Repeat("B", 40) + " , " + a + ",," + b
	if code, body := e.do(t, nil, "POST", "/admin/settings", `{"name":"providerExclude","value":"`+in+`"}`, nil); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	if v := e.view(t)["providerExclude"]; v.Source != "persisted" || v.Value != a+","+b {
		t.Fatalf("saved %+v, want %q", v, a+","+b)
	}
}
