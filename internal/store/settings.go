package store

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
	}
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
