package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// S6: settings persist beside AuthState (0640, tmp+rename), survive a
// reopen, and a refused update leaves them unchanged.
func TestS6SettingsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthState(AuthState{TwoFactorOn: true}); err != nil {
		t.Fatal(err)
	}
	n := 240
	if err := s.UpdateSettings(func(x *Settings) error { x.ForwardIdle = &n; x.LogLevel = "debug"; return nil }); err != nil {
		t.Fatal(err)
	}
	n = 1 // the stored copy is not aliased
	if err := s.UpdateSettings(func(x *Settings) error { x.LogLevel = "error"; return errors.New("no") }); err == nil {
		t.Fatal("want error")
	}
	fi, err := os.Stat(filepath.Join(dir, "uplink-state.json"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v", fi, err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.GetSettings()
	if got.ForwardIdle == nil || *got.ForwardIdle != 240 || got.LogLevel != "debug" || got.SessionIdleGrace != nil {
		t.Fatalf("got %+v", got)
	}
	if !s2.GetAuthState().TwoFactorOn {
		t.Fatal("auth state lost")
	}
}
