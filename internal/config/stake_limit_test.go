package config

import (
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

// Spec §5 test 8 (boot side): no row gives 150; a saved "" is off; an
// invalid saved value is kept, marked invalid, and noted.
func TestMaxStMorPerHourBoot(t *testing.T) {
	clearSettingEnv(t)
	c := quietFromEnv(t)
	ApplyPersisted(c, map[string]string{})
	if c.MaxStMorPerHour != "150" || c.StakeLimitInvalid || c.StakeLimitWei.String() != "150000000000000000000" {
		t.Fatalf("no row: %q %v %v", c.MaxStMorPerHour, c.StakeLimitWei, c.StakeLimitInvalid)
	}

	c = quietFromEnv(t)
	ApplyPersisted(c, map[string]string{"maxStMorPerHour": ""})
	if c.MaxStMorPerHour != "" || c.StakeLimitWei != nil || c.StakeLimitInvalid {
		t.Fatalf("empty: %q %v %v", c.MaxStMorPerHour, c.StakeLimitWei, c.StakeLimitInvalid)
	}

	c = quietFromEnv(t)
	notes := ApplyPersisted(c, map[string]string{"maxStMorPerHour": "abc"})
	if c.MaxStMorPerHour != "abc" || c.StakeLimitWei != nil || !c.StakeLimitInvalid {
		t.Fatalf("invalid: %q %v %v", c.MaxStMorPerHour, c.StakeLimitWei, c.StakeLimitInvalid)
	}
	found := false
	for _, n := range append(notes, c.SettingNotes...) {
		found = found || n == MaxStMorPerHourInvalidNote
	}
	if !found || MaxStMorPerHourInvalidNote != "Max stMOR/hr is invalid: fix the value and restart" {
		t.Fatalf("notes %v", notes)
	}
}

func TestMaxStMorPerHourRow(t *testing.T) {
	d, ok := SettingByName("maxStMorPerHour")
	if !ok || !strings.HasPrefix(d.Help, "Highest price a new session may open at, in stMOR/hr; empty means no limit.") || d.Default != "150" || d.Scope != ScopeRestart {
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
}
