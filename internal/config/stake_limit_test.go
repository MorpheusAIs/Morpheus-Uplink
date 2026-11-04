package config

import (
	"slices"
	"strings"
	"testing"
)

// Spec §5 test 7.
func TestParseMorLimit(t *testing.T) {
	ok := map[string]string{
		"":         "<nil>",
		"0.5":      "500000000000000000",
		"150":      "150000000000000000000",
		"100000":   "100000000000000000000000",
		"1.000001": "1000001000000000000",
	}
	for in, want := range ok {
		got, err := ParseMorLimit(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseMorLimit(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "0.000000", "-1", "1e3", "abc", "1/3", "0x10", "1_0", "1.0000001", "100000.000001", "100001", " 1 2", ".5", "5."} {
		if got, err := ParseMorLimit(in); err == nil {
			t.Errorf("ParseMorLimit(%q) accepted: %v", in, got)
		}
	}
}

// Boot with no saved cap: 150 per hour of the default session, not saved.
// A saved per-session value is kept as applied; "" is off; an invalid value
// is kept, marked invalid, and noted.
func TestMaxStMorPerSessionBoot(t *testing.T) {
	clearSettingEnv(t)
	for sec, want := range map[int]string{600: "25", 7200: "300", 1800: "75"} {
		c := quietFromEnv(t)
		ApplyPersisted(c, map[string]string{})
		if save := ResolveStakeCap(c, map[string]string{}, sec); save != nil {
			t.Fatalf("%d s: default saved %q", sec, *save)
		}
		if c.MaxStMorPerSession != want || c.StakeLimitInvalid {
			t.Fatalf("%d s: default %q %v, want %s", sec, c.MaxStMorPerSession, c.StakeLimitInvalid, want)
		}
	}

	vals := map[string]string{"maxStMorPerSession": "4"}
	c := quietFromEnv(t)
	ApplyPersisted(c, vals)
	if save := ResolveStakeCap(c, vals, 7200); save != nil || c.MaxStMorPerSession != "4" || c.StakeLimitWei.String() != "4000000000000000000" {
		t.Fatalf("saved: %v %q %v", save, c.MaxStMorPerSession, c.StakeLimitWei)
	}

	vals = map[string]string{"maxStMorPerSession": ""}
	c = quietFromEnv(t)
	ApplyPersisted(c, vals)
	ResolveStakeCap(c, vals, 600)
	if c.MaxStMorPerSession != "" || c.StakeLimitWei != nil || c.StakeLimitInvalid {
		t.Fatalf("empty: %q %v %v", c.MaxStMorPerSession, c.StakeLimitWei, c.StakeLimitInvalid)
	}

	vals = map[string]string{"maxStMorPerSession": "abc"}
	c = quietFromEnv(t)
	notes := ApplyPersisted(c, vals)
	ResolveStakeCap(c, vals, 600)
	if c.MaxStMorPerSession != "abc" || c.StakeLimitWei != nil || !c.StakeLimitInvalid {
		t.Fatalf("invalid: %q %v %v", c.MaxStMorPerSession, c.StakeLimitWei, c.StakeLimitInvalid)
	}
	found := false
	for _, n := range append(notes, c.SettingNotes...) {
		found = found || n == MaxStMorPerSessionInvalidNote
	}
	if !found || MaxStMorPerSessionInvalidNote != "Max stMOR per session is invalid: fix the value and restart" {
		t.Fatalf("notes %v", notes)
	}
}

// A saved hourly cap is converted once at boot: hourly × default hours,
// rounded up to 6 decimals; "" stays off; an invalid value is kept, marked
// invalid, and noted.
func TestMaxStMorPerHourConverted(t *testing.T) {
	clearSettingEnv(t)
	for _, c := range []struct {
		hourly string
		sec    int
		want   string
	}{
		{"150", 600, "25"},
		{"150", 7200, "300"},
		{"2", 7200, "4"},
		{"1", 1000, "0.277778"},
		{"0.000001", 600, "0.000001"},
		{"100000", 86400, "100000"},
		{"", 600, ""},
	} {
		vals := map[string]string{LegacyMaxStMorPerHour: c.hourly}
		cfg := quietFromEnv(t)
		ApplyPersisted(cfg, vals)
		save := ResolveStakeCap(cfg, vals, c.sec)
		if save == nil || *save != c.want || cfg.MaxStMorPerSession != c.want || cfg.StakeLimitInvalid {
			t.Fatalf("%s/h at %d s: save %v cap %q invalid %v; want %q", c.hourly, c.sec, save, cfg.MaxStMorPerSession, cfg.StakeLimitInvalid, c.want)
		}
	}
	vals := map[string]string{LegacyMaxStMorPerHour: "abc"}
	cfg := quietFromEnv(t)
	ApplyPersisted(cfg, vals)
	save := ResolveStakeCap(cfg, vals, 600)
	if save == nil || *save != "abc" || !cfg.StakeLimitInvalid || !slices.Contains(cfg.SettingNotes, MaxStMorPerSessionInvalidNote) {
		t.Fatalf("invalid hourly: %v %v %v", save, cfg.StakeLimitInvalid, cfg.SettingNotes)
	}
	// A saved per-session value wins over a leftover hourly one.
	vals = map[string]string{LegacyMaxStMorPerHour: "150", "maxStMorPerSession": "3"}
	cfg = quietFromEnv(t)
	ApplyPersisted(cfg, vals)
	if save := ResolveStakeCap(cfg, vals, 7200); save != nil || cfg.MaxStMorPerSession != "3" {
		t.Fatalf("both: %v %q", save, cfg.MaxStMorPerSession)
	}
}

func TestMaxStMorPerSessionRow(t *testing.T) {
	d, ok := SettingByName("maxStMorPerSession")
	if !ok || !strings.HasPrefix(d.Help, "The most stMOR one new session may stake, whatever its length;") || d.Default != "25" || d.Scope != ScopeRestart {
		t.Fatalf("row %+v", d)
	}
	if v, err := d.Validate(" 12.5 "); err != nil || v != "12.5" {
		t.Fatalf("validate: %q %v", v, err)
	}
	if v, err := d.Validate(""); err != nil || v != "" {
		t.Fatalf("validate empty: %q %v", v, err)
	}
	if _, err := d.Validate("0"); err == nil {
		t.Fatal("0 accepted")
	}
	if _, ok := SettingByName(LegacyMaxStMorPerHour); ok {
		t.Fatal("hourly row still listed")
	}
}
