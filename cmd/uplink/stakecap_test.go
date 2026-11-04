package main

import (
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// A saved hourly cap is converted once at boot for the saved default
// session (150/h at 2 h is 300), saved under maxStMorPerSession, and the
// hourly key is dropped; the next boot keeps the saved value as is.
func TestResolveStakeCapConvertsOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hourly, def, hi := "150", "7200", "7200"
	if err := st.UpdateSettings(func(x *store.Settings) error {
		for n, v := range map[string]*string{config.LegacyMaxStMorPerHour: &hourly, "sessionDurationDefault": &def, "sessionDurationMax": &hi} {
			if err := x.SetValue(n, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	boot := func() *config.Config {
		cfg := &config.Config{SessionDurationSec: 600, SessionDurationMax: 1800}
		saved := st.GetSettings()
		config.ApplyPersisted(cfg, saved.All())
		resolveStakeCap(cfg, st, saved)
		return cfg
	}
	cfg := boot()
	if cfg.MaxStMorPerSession != "300" || cfg.StakeLimitWei.String() != "300000000000000000000" {
		t.Fatalf("converted %q %v", cfg.MaxStMorPerSession, cfg.StakeLimitWei)
	}
	all := st.GetSettings().All()
	if all["maxStMorPerSession"] != "300" {
		t.Fatalf("saved %v", all)
	}
	if _, ok := all[config.LegacyMaxStMorPerHour]; ok {
		t.Fatalf("hourly key kept: %v", all)
	}
	// A later default change does not move a saved cap.
	short := "600"
	if err := st.UpdateSettings(func(x *store.Settings) error { return x.SetValue("sessionDurationDefault", &short) }); err != nil {
		t.Fatal(err)
	}
	if cfg := boot(); cfg.MaxStMorPerSession != "300" {
		t.Fatalf("second boot %q", cfg.MaxStMorPerSession)
	}
}

// With nothing saved the cap follows the default session: 150 per hour.
func TestBootDefaultSessionSec(t *testing.T) {
	cfg := &config.Config{SessionDurationSec: 600, SessionDurationMax: 1800}
	d, hi, low := 7200, 3600, 60
	for _, c := range []struct {
		saved store.Settings
		want  int
	}{
		{store.Settings{}, 600},
		{store.Settings{SessionDurationDefault: &d}, 1800},
		{store.Settings{SessionDurationDefault: &d, SessionDurationMax: &hi}, 3600},
		{store.Settings{SessionDurationDefault: &low}, 600},
	} {
		if got := bootDefaultSessionSec(cfg, c.saved); got != c.want {
			t.Errorf("bootDefaultSessionSec(%+v) = %d, want %d", c.saved, got, c.want)
		}
	}
}
