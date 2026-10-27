package store

import "strconv"

// Settings are operator settings changed in the admin console (#51). They
// live beside AuthState in the same state file and are written with it
// (tmp + rename, 0640). A nil field or an empty LogLevel means "not set":
// the environment or the built-in default applies. Environment values
// always win over these (precedence env > persisted > default).
type Settings struct {
	SessionDurationDefault *int   `json:"sessionDurationDefault,omitempty"`
	SessionDurationMax     *int   `json:"sessionDurationMax,omitempty"`
	SessionIdleGrace       *int   `json:"sessionIdleGrace,omitempty"`
	ForwardIdle            *int   `json:"forwardIdle,omitempty"`
	LogLevel               string `json:"logLevel,omitempty"`
	// Values holds every other console setting by name, in canonical
	// string form (validated by the caller). Secrets are never stored here.
	Values map[string]string `json:"values,omitempty"`
}

// legacyInt maps the first four settings to their typed fields.
func (a *Settings) legacyInt(name string) **int {
	switch name {
	case "sessionDurationDefault":
		return &a.SessionDurationDefault
	case "sessionDurationMax":
		return &a.SessionDurationMax
	case "sessionIdleGrace":
		return &a.SessionIdleGrace
	case "forwardIdle":
		return &a.ForwardIdle
	}
	return nil
}

// Value returns the persisted value of a setting by name.
func (a Settings) Value(name string) (string, bool) {
	if p := a.legacyInt(name); p != nil {
		if *p == nil {
			return "", false
		}
		return strconv.Itoa(**p), true
	}
	if name == "logLevel" {
		return a.LogLevel, a.LogLevel != ""
	}
	v, ok := a.Values[name]
	return v, ok
}

// SetValue sets (v != nil) or clears (v == nil) a setting by name. v must
// already be validated; a legacy int name needs a whole number.
func (a *Settings) SetValue(name string, v *string) error {
	if p := a.legacyInt(name); p != nil {
		if v == nil {
			*p = nil
			return nil
		}
		n, err := strconv.Atoi(*v)
		if err != nil {
			return err
		}
		*p = &n
		return nil
	}
	if name == "logLevel" {
		a.LogLevel = ""
		if v != nil {
			a.LogLevel = *v
		}
		return nil
	}
	if v == nil {
		delete(a.Values, name)
		return nil
	}
	if a.Values == nil {
		a.Values = map[string]string{}
	}
	a.Values[name] = *v
	return nil
}

// All returns every persisted setting by name.
func (a Settings) All() map[string]string {
	out := map[string]string{}
	for _, n := range []string{"sessionDurationDefault", "sessionDurationMax", "sessionIdleGrace", "forwardIdle", "logLevel"} {
		if v, ok := a.Value(n); ok {
			out[n] = v
		}
	}
	for k, v := range a.Values {
		out[k] = v
	}
	return out
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (a Settings) clone() Settings {
	return Settings{
		SessionDurationDefault: cloneInt(a.SessionDurationDefault),
		SessionDurationMax:     cloneInt(a.SessionDurationMax),
		SessionIdleGrace:       cloneInt(a.SessionIdleGrace),
		ForwardIdle:            cloneInt(a.ForwardIdle),
		LogLevel:               a.LogLevel,
		Values:                 cloneValues(a.Values),
	}
}

func cloneValues(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// GetSettings returns a copy of the persisted settings.
func (s *Store) GetSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Settings.clone()
}

// UpdateSettings runs fn under the store lock and saves only if fn returns
// nil; on a failed save the in-memory settings are left unchanged.
func (s *Store) UpdateSettings(fn func(*Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st.Settings.clone()
	if err := fn(&next); err != nil {
		return err
	}
	prev := s.st.Settings
	s.st.Settings = next
	if err := s.save(); err != nil {
		s.st.Settings = prev
		return err
	}
	return nil
}
