package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// LogLevel is the live slog level (UPLINK_LOG_LEVEL or the persisted
// setting). main builds the default handler on it; a setting change takes
// effect without a restart.
var LogLevel = new(slog.LevelVar)

// settingDef is one operator setting (#51). Bounds match the env values.
// The admin session timeouts, MASTER_KEY_ADMIN and secrets are deliberately
// not settings: they cannot be changed from an admin session.
type settingDef struct {
	name     string
	env      string
	min, max int
	def      int
	level    bool // logLevel: a string setting
}

var settingDefs = []settingDef{
	{name: "sessionDurationDefault", env: "SESSION_DURATION_SECONDS", min: 600, max: 86400, def: 600},
	{name: "sessionDurationMax", env: "UPLINK_SESSION_DURATION_MAX", min: 600, max: 86400, def: 1800},
	{name: "sessionIdleGrace", env: "UPLINK_SESSION_IDLE_GRACE_SECONDS", min: 60, max: 3600, def: 300},
	{name: "forwardIdle", env: "UPLINK_FORWARD_IDLE_SECONDS", min: 30, max: 600, def: 180},
	{name: "logLevel", env: "UPLINK_LOG_LEVEL", level: true},
}

func settingByName(name string) (settingDef, bool) {
	for _, d := range settingDefs {
		if d.name == name {
			return d, true
		}
	}
	return settingDef{}, false
}

// runtimeSettings holds the effective values; read on every request.
type runtimeSettings struct {
	mu     sync.RWMutex
	ints   map[string]int
	level  string
	source map[string]string // env | persisted | default
}

func persistedInt(st store.Settings, name string) *int {
	switch name {
	case "sessionDurationDefault":
		return st.SessionDurationDefault
	case "sessionDurationMax":
		return st.SessionDurationMax
	case "sessionIdleGrace":
		return st.SessionIdleGrace
	case "forwardIdle":
		return st.ForwardIdle
	}
	return nil
}

func setPersistedInt(st *store.Settings, name string, v *int) {
	switch name {
	case "sessionDurationDefault":
		st.SessionDurationDefault = v
	case "sessionDurationMax":
		st.SessionDurationMax = v
	case "sessionIdleGrace":
		st.SessionIdleGrace = v
	case "forwardIdle":
		st.ForwardIdle = v
	}
}

// cfgInt is the value from the environment (or the built-in default when
// the env var is unset or invalid: config already fell back to it).
func cfgInt(cfg *config.Config, name string, def int) int {
	v := 0
	switch name {
	case "sessionDurationDefault":
		v = cfg.SessionDurationSec
	case "sessionDurationMax":
		v = cfg.SessionDurationMax
	case "sessionIdleGrace":
		v = cfg.SessionIdleGraceSec
	case "forwardIdle":
		v = cfg.ForwardIdleSec
	}
	if v <= 0 {
		return def
	}
	return v
}

func levelOf(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// resolveSettings applies env > persisted > default. A persisted value out
// of bounds (hand-edited file) is ignored with a warning (name only); the
// returned notes are written to the ops audit stream by the caller. The
// duration cap always wins: a default above the max is clamped down to it.
func resolveSettings(cfg *config.Config, st store.Settings) (*runtimeSettings, []string) {
	rs := &runtimeSettings{ints: map[string]int{}, source: map[string]string{}}
	var notes []string
	for _, d := range settingDefs {
		pinned := cfg.EnvPinned[d.env]
		if d.level {
			switch l := config.ParseLogLevel(st.LogLevel); {
			case pinned:
				rs.level, rs.source[d.name] = config.ParseLogLevel(cfg.LogLevel), "env"
			case l != "":
				rs.level, rs.source[d.name] = l, "persisted"
			default:
				if st.LogLevel != "" {
					slog.Warn("settings: persisted value is invalid; using the default", "setting", d.name)
					notes = append(notes, d.name+" persisted value invalid; ignored")
				}
				rs.level, rs.source[d.name] = "info", "default"
			}
			continue
		}
		p := persistedInt(st, d.name)
		switch {
		case pinned:
			rs.ints[d.name], rs.source[d.name] = cfgInt(cfg, d.name, d.def), "env"
		case p != nil && *p >= d.min && *p <= d.max:
			rs.ints[d.name], rs.source[d.name] = *p, "persisted"
		default:
			if p != nil {
				slog.Warn("settings: persisted value is out of range; using the default", "setting", d.name)
				notes = append(notes, d.name+" persisted value out of range; ignored")
			}
			rs.ints[d.name], rs.source[d.name] = cfgInt(cfg, d.name, d.def), "default"
		}
	}
	if mx := rs.ints["sessionDurationMax"]; rs.ints["sessionDurationDefault"] > mx {
		slog.Warn("settings: sessionDurationDefault is above sessionDurationMax; clamped to the max", "max", mx)
		notes = append(notes, "sessionDurationDefault above sessionDurationMax; clamped to "+strconv.Itoa(mx))
		rs.ints["sessionDurationDefault"] = mx
	}
	return rs, notes
}

func (rs *runtimeSettings) int(name string) int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.ints[name]
}

// applySettings pushes the effective values into the pool and the logger.
func (s *Server) applySettings() {
	if s.pool != nil {
		s.pool.SetDuration(s.settings.int("sessionDurationDefault"))
		s.pool.SetDurationMax(s.settings.int("sessionDurationMax"))
	}
	s.settings.mu.RLock()
	lvl := s.settings.level
	s.settings.mu.RUnlock()
	LogLevel.Set(levelOf(lvl))
}

func (s *Server) durationDefault() int { return s.settings.int("sessionDurationDefault") }
func (s *Server) durationMax() int     { return s.settings.int("sessionDurationMax") }
func (s *Server) forwardIdle() int     { return s.settings.int("forwardIdle") }

// SessionIdleGrace is the idle-sweep grace in seconds (read on every tick).
func (s *Server) SessionIdleGrace() int { return s.settings.int("sessionIdleGrace") }

type settingView struct {
	Name    string   `json:"name"`
	Value   any      `json:"value"`
	Default any      `json:"default"`
	Min     int      `json:"min,omitempty"`
	Max     int      `json:"max,omitempty"`
	Options []string `json:"options,omitempty"`
	Source  string   `json:"source"` // env | persisted | default
	Env     string   `json:"env"`
	Pinned  bool     `json:"pinned"` // set by environment: read-only here
}

func (s *Server) settingsView() map[string]any {
	s.settings.mu.RLock()
	defer s.settings.mu.RUnlock()
	out := make([]settingView, 0, len(settingDefs))
	for _, d := range settingDefs {
		v := settingView{Name: d.name, Env: d.env, Source: s.settings.source[d.name], Pinned: s.settings.source[d.name] == "env"}
		if d.level {
			v.Value, v.Default, v.Options = s.settings.level, "info", []string{"debug", "info", "warn", "error"}
		} else {
			v.Value, v.Default, v.Min, v.Max = s.settings.ints[d.name], d.def, d.min, d.max
		}
		out = append(out, v)
	}
	return map[string]any{"settings": out}
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSettingsSet: {"name":"forwardIdle","value":240} or
// {"name":"forwardIdle","reset":true} (drop the persisted value).
func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
		Reset bool            `json:"reset"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	d, ok := settingByName(req.Name)
	if !ok {
		http.Error(w, "unknown setting", http.StatusBadRequest)
		return
	}
	if !req.Reset && len(req.Value) == 0 {
		http.Error(w, "value required", http.StatusBadRequest)
		return
	}
	// Serialise changes so old→new and the cross-field check are exact.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.settings.mu.RLock()
	src := s.settings.source[d.name]
	s.settings.mu.RUnlock()
	if src == "env" {
		http.Error(w, "set by environment ("+d.env+")", http.StatusConflict)
		return
	}

	var oldStr, newStr string
	if d.level {
		var lv string
		if !req.Reset {
			if err := json.Unmarshal(req.Value, &lv); err != nil || config.ParseLogLevel(lv) == "" {
				http.Error(w, "logLevel must be debug, info, warn or error", http.StatusBadRequest)
				return
			}
			lv = config.ParseLogLevel(lv)
		}
		if err := s.store.UpdateSettings(func(st *store.Settings) error { st.LogLevel = lv; return nil }); err != nil {
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		s.settings.mu.Lock()
		oldStr = s.settings.level
		if req.Reset {
			s.settings.level, s.settings.source[d.name] = "info", "default"
		} else {
			s.settings.level, s.settings.source[d.name] = lv, "persisted"
		}
		newStr = s.settings.level
		s.settings.mu.Unlock()
	} else {
		var n int
		var np *int
		if req.Reset {
			n = cfgInt(s.cfg, d.name, d.def)
		} else {
			if err := json.Unmarshal(req.Value, &n); err != nil || n < d.min || n > d.max {
				http.Error(w, fmt.Sprintf("%s must be a whole number %d…%d", d.name, d.min, d.max), http.StatusBadRequest)
				return
			}
			np = &n
		}
		def, max := s.durationDefault(), s.durationMax()
		if (d.name == "sessionDurationDefault" && n > max) || (d.name == "sessionDurationMax" && n < def) {
			http.Error(w, "sessionDurationDefault must not exceed sessionDurationMax", http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateSettings(func(st *store.Settings) error { setPersistedInt(st, d.name, np); return nil }); err != nil {
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		s.settings.mu.Lock()
		oldStr = strconv.Itoa(s.settings.ints[d.name])
		s.settings.ints[d.name] = n
		s.settings.source[d.name] = map[bool]string{true: "default", false: "persisted"}[req.Reset]
		s.settings.mu.Unlock()
		newStr = strconv.Itoa(n)
	}
	s.applySettings()
	s.auditAdmin(r, "setting_change", d.name+" "+oldStr+"→"+newStr)
	writeJSON(w, http.StatusOK, s.settingsView())
}
