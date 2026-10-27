package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// settingView is one row of GET /admin/settings (every config.Settings row).
type settingView struct {
	Name    string   `json:"name"`
	Env     string   `json:"env"`
	Kind    string   `json:"kind"` // int | bool | enum | string
	Value   any      `json:"value"`
	Default any      `json:"default"`
	Min     int      `json:"min,omitempty"`
	Max     int      `json:"max,omitempty"`
	Options []string `json:"options,omitempty"`
	Source  string   `json:"source"` // env | persisted | default | router
	Pinned  bool     `json:"pinned"` // set by environment: read-only here
	// Scope: live | restart | router | env (see config.SettingScope).
	Scope string `json:"scope"`
	// Restart: "" (live), "uplink" or "router": what must restart.
	Restart  string `json:"restart,omitempty"`
	Settable bool   `json:"settable"`
	Group    string `json:"group,omitempty"`
	Help     string `json:"help"`
	// Effective is the value Uplink runs with now (restart scope); it
	// differs from Value until the next Uplink start (PendingRestart).
	Effective      any  `json:"effective,omitempty"`
	PendingRestart bool `json:"pendingRestart,omitempty"`
}

func typedSetting(d config.SettingDef, v string) any {
	switch d.Kind {
	case config.KindInt:
		n, _ := strconv.Atoi(v)
		return n
	case config.KindBool:
		b, _ := strconv.ParseBool(v)
		return b
	}
	return v
}

// restartValue is the value a restart-scope setting will have on the next
// start (env > persisted > default) and its source.
func (s *Server) restartValue(d config.SettingDef, st store.Settings) (string, string) {
	if s.cfg.EnvPinned[d.Env] {
		return d.Effective(s.cfg), "env"
	}
	if raw, ok := st.Value(d.Name); ok {
		if v, err := d.Validate(raw); err == nil {
			return v, "persisted"
		}
	}
	return d.Default, "default"
}

func (s *Server) settingsView() map[string]any {
	var st store.Settings
	if s.store != nil {
		st = s.store.GetSettings()
	}
	s.settings.mu.RLock()
	defer s.settings.mu.RUnlock()
	out := make([]settingView, 0, len(config.Settings))
	for _, d := range config.Settings {
		v := settingView{Name: d.Name, Env: d.Env, Kind: string(d.Kind), Default: typedSetting(d, d.Default),
			Min: d.Min, Max: d.Max, Options: d.Options, Scope: string(d.Scope), Settable: d.Settable(), Group: d.Group, Help: d.Help}
		switch d.Scope {
		case config.ScopeLive:
			v.Source = s.settings.source[d.Name]
			if d.Kind == config.KindEnum {
				v.Value = s.settings.level
			} else {
				v.Value = s.settings.ints[d.Name]
			}
		case config.ScopeRestart:
			val, src := s.restartValue(d, st)
			eff := d.Effective(s.cfg)
			v.Value, v.Source, v.Restart = typedSetting(d, val), src, "uplink"
			v.Effective, v.PendingRestart = typedSetting(d, eff), val != eff
		case config.ScopeEnvOnly:
			v.Value, v.Restart, v.Source = typedSetting(d, d.Effective(s.cfg)), "uplink", "default"
			if s.cfg.EnvPinned[d.Env] {
				v.Source = "env"
			}
		case config.ScopeRouter:
			v.Value, v.Source, v.Restart = nil, "router", "router"
		}
		v.Pinned = v.Source == "env"
		out = append(out, v)
	}
	return map[string]any{"settings": out}
}

// scalarString turns a JSON number, bool or string into its text.
func scalarString(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" || t[0] == '{' || t[0] == '[' {
		return "", false
	}
	if t[0] == '"' {
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", false
		}
		return v, true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return t, true
}

// registryLookup resolves a settable registry row; secrets and unknown
// names are refused the same way and never echoed beyond a safe name.
func registryLookup(name string) (config.SettingDef, int, string) {
	if config.IsSecretKey(name) {
		return config.SettingDef{}, http.StatusBadRequest, "secrets are set in .env only"
	}
	d, ok := config.SettingByName(name)
	if !ok {
		return d, http.StatusBadRequest, "unknown setting"
	}
	if !d.Settable() {
		if d.Scope == config.ScopeRouter {
			return d, http.StatusBadRequest, d.Name + " is a router setting: set it in the router's .env"
		}
		return d, http.StatusBadRequest, d.Name + " is set in .env only"
	}
	return d, 0, ""
}

// handleRegistrySet sets or resets one restart-scope setting. It is stored
// now and read on the next Uplink start.
func (s *Server) handleRegistrySet(w http.ResponseWriter, r *http.Request, name string, raw json.RawMessage, reset bool) {
	d, code, msg := registryLookup(name)
	if code != 0 {
		http.Error(w, msg, code)
		return
	}
	var val *string
	if !reset {
		t, ok := scalarString(raw)
		if !ok {
			http.Error(w, "invalid value", http.StatusBadRequest)
			return
		}
		v, err := d.Validate(t)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		val = &v
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.cfg.EnvPinned[d.Env] {
		http.Error(w, "set by environment ("+d.Env+")", http.StatusConflict)
		return
	}
	oldStr, _ := s.restartValue(d, s.store.GetSettings())
	if err := s.store.UpdateSettings(func(st *store.Settings) error { return st.SetValue(d.Name, val) }); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	newStr, _ := s.restartValue(d, s.store.GetSettings())
	s.auditAdmin(r, "setting_change", settingChangeText(d, oldStr, newStr))
	writeJSON(w, http.StatusOK, s.settingsView())
}

// settingChangeText: "name old→new (restart)"; string values are not
// copied into the audit line, only that they changed.
func settingChangeText(d config.SettingDef, oldStr, newStr string) string {
	if d.Kind == config.KindString {
		return d.Name + " changed (restart)"
	}
	return d.Name + " " + oldStr + "→" + newStr + " (restart)"
}

// Settings file (export/import). Same guard as the settings API
// (requireConsoleSession: cookie session, CSRF, no Authorization header)
// plus a fresh step-up, like a key reveal.
const (
	settingsFileFormat  = "uplink-settings"
	settingsFileVersion = 1
	settingsFileMax     = 64 << 10
)

type settingsFile struct {
	Format   string                     `json:"format"`
	Version  int                        `json:"version"`
	Settings map[string]json.RawMessage `json:"settings"`
}

func (s *Server) requireStepUp(w http.ResponseWriter, r *http.Request) bool {
	auth := adminAuthFrom(r)
	if auth.bearer || auth.session == nil {
		http.Error(w, "settings need an admin session", http.StatusForbidden)
		return false
	}
	if !auth.session.Fresh(s.sessions.Now(), StepUpMaxAge) {
		http.Error(w, errMsgStepUp, http.StatusUnauthorized)
		return false
	}
	return true
}

// handleSettingsExport: the persisted settable values, typed. Secrets are
// never settings, so they cannot appear.
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireStepUp(w, r) {
		return
	}
	all := s.store.GetSettings().All()
	out := map[string]any{}
	for _, d := range config.Settings {
		raw, ok := all[d.Name]
		if !ok || !d.Settable() {
			continue
		}
		if v, err := d.Validate(raw); err == nil {
			out[d.Name] = typedSetting(d, v)
		}
	}
	s.auditAdmin(r, "settings_export", strconv.Itoa(len(out))+" settings")
	w.Header().Set("Content-Disposition", `attachment; filename="uplink-settings.json"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"format": settingsFileFormat, "version": settingsFileVersion, "settings": out})
}

var reSafeName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// handleSettingsImport replaces the persisted settings with the file's.
// Every value is checked first (unknown, secret, read-only, wrong type or
// out of range refuses the whole file); nothing is written on error. Live
// settings apply at once; restart-scope ones on the next start. A value
// the environment pins is stored but the env value still wins.
func (s *Server) handleSettingsImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireStepUp(w, r) {
		return
	}
	var f settingsFile
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, settingsFileMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		http.Error(w, "invalid settings file", http.StatusBadRequest)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid settings file", http.StatusBadRequest)
		return
	}
	if f.Format != settingsFileFormat || f.Version != settingsFileVersion || f.Settings == nil {
		http.Error(w, "not an uplink-settings v1 file", http.StatusBadRequest)
		return
	}
	names := make([]string, 0, len(f.Settings))
	for k := range f.Settings {
		names = append(names, k)
	}
	sort.Strings(names)
	vals := map[string]string{}
	var pinned []string
	for _, k := range names {
		d, code, msg := registryLookup(k)
		if code != 0 {
			if code == http.StatusBadRequest && msg == "unknown setting" && reSafeName.MatchString(k) {
				msg += ": " + k
			}
			http.Error(w, msg, code)
			return
		}
		t, ok := scalarString(f.Settings[k])
		if !ok {
			http.Error(w, d.Name+": invalid value", http.StatusBadRequest)
			return
		}
		v, err := d.Validate(t)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vals[d.Name] = v
		if s.cfg.EnvPinned[d.Env] {
			pinned = append(pinned, d.Name)
		}
	}
	var cand store.Settings
	for k, v := range vals {
		v := v
		if err := cand.SetValue(k, &v); err != nil {
			http.Error(w, k+": invalid value", http.StatusBadRequest)
			return
		}
	}
	rs, _ := resolveSettings(s.cfg, cand)
	if def, ok := vals["sessionDurationDefault"]; ok && !s.cfg.EnvPinned["SESSION_DURATION_SECONDS"] {
		if n, _ := strconv.Atoi(def); n > rs.ints["sessionDurationMax"] {
			http.Error(w, "sessionDurationDefault must not exceed sessionDurationMax", http.StatusBadRequest)
			return
		}
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := s.store.UpdateSettings(func(st *store.Settings) error { *st = cand; return nil }); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	s.settings.mu.Lock()
	s.settings.ints, s.settings.level, s.settings.source = rs.ints, rs.level, rs.source
	s.settings.mu.Unlock()
	s.applySettings()
	s.auditAdmin(r, "settings_import", strconv.Itoa(len(vals))+" settings: "+strings.Join(names, ","))
	view := s.settingsView()
	view["pinned"] = pinned
	writeJSON(w, http.StatusOK, view)
}
