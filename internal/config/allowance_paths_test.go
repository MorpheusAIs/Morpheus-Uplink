package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Josh SHOULD B: /ctl and /status are never under DATA_DIR.
func TestAllowancePathsNotUnderDataDir(t *testing.T) {
	data := t.TempDir()
	cases := []struct {
		status, ctl string
		bad         bool
	}{
		{"/status/allowance-status.json", "/ctl", false},
		{filepath.Join(data, "allowance-status.json"), "/ctl", true},
		{"/status/allowance-status.json", filepath.Join(data, "ctl"), true},
		{"/status/allowance-status.json", data, true},
		{"/status/x.json", filepath.Join(data, "..", filepath.Base(data)+"-ctl"), false},
		{"", "", false},
	}
	for _, tc := range cases {
		c := &Config{DataDir: data, AllowanceStatusFile: tc.status, AllowanceCtlDir: tc.ctl}
		err := checkAllowancePaths(c)
		if (err != nil) != tc.bad {
			t.Fatalf("status=%q ctl=%q: err=%v want bad=%v", tc.status, tc.ctl, err, tc.bad)
		}
	}
}

// Fallback D: an allowance file recorded as source=ceiling is reported as
// such (GUI shows "ceiling, no budget").
func TestAllowanceSourceCeiling(t *testing.T) {
	p := filepath.Join(t.TempDir(), "router.mor-allowance.json")
	if err := os.WriteFile(p, []byte(`{"wei":"100000000000000000000000000","source":"ceiling"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	c := &Config{MORAllowanceFile: p}
	if err := loadMORAllowance(c); err != nil {
		t.Fatal(err)
	}
	if c.MORAllowanceSource != "ceiling" || c.MORAllowanceWei != "100000000000000000000000000" {
		t.Fatalf("got %q %q", c.MORAllowanceSource, c.MORAllowanceWei)
	}
}
