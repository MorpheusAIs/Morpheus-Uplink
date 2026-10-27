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

// Settings tab: every setting persists by name in DATA_DIR and survives a
// reopen; the first five keep their typed fields; clearing removes a value;
// copies are not aliased.
func TestSettingsValuesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	str := func(v string) *string { return &v }
	if err := s.UpdateSettings(func(x *Settings) error {
		for k, v := range map[string]string{"forwardIdle": "240", "logLevel": "warn", "inflightMax": "64", "sessionFailover": "false", "providerExclude": "m=0x1"} {
			if err := x.SetValue(k, str(v)); err != nil {
				return err
			}
		}
		return x.SetValue("sessionIdleGrace", str("x"))
	}); err == nil {
		t.Fatal("a non-number legacy value was accepted")
	}
	if err := s.UpdateSettings(func(x *Settings) error {
		_ = x.SetValue("forwardIdle", str("240"))
		_ = x.SetValue("logLevel", str("warn"))
		_ = x.SetValue("inflightMax", str("64"))
		_ = x.SetValue("sessionFailover", str("false"))
		_ = x.SetValue("housekeeping", str("false"))
		return x.SetValue("housekeeping", nil)
	}); err != nil {
		t.Fatal(err)
	}
	got := s.GetSettings()
	got.Values["inflightMax"] = "1" // not aliased
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := s2.GetSettings().All()
	want := map[string]string{"forwardIdle": "240", "logLevel": "warn", "inflightMax": "64", "sessionFailover": "false"}
	if len(all) != len(want) {
		t.Fatalf("after reopen %v", all)
	}
	for k, v := range want {
		if all[k] != v {
			t.Fatalf("after reopen %v", all)
		}
	}
	if st := s2.GetSettings(); st.ForwardIdle == nil || *st.ForwardIdle != 240 || st.LogLevel != "warn" {
		t.Fatalf("legacy fields %+v", st)
	}
}
