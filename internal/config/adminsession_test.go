package config

import (
	"strings"
	"testing"
	"time"
)

// S1 test 4 (and the defaults of test 1): every ADMIN_SESSION_* value is
// kept or moved in the shorter direction, never refused.
func TestParseAdminSession(t *testing.T) {
	cases := []struct {
		name        string
		idle, abs   string
		wantIdle    time.Duration
		wantAbs     time.Duration
		wantWarning string // substring; "" = no warning
	}{
		{"unset", "", "", 4 * time.Hour, 12 * time.Hour, ""},
		{"blank", "  ", " ", 4 * time.Hour, 12 * time.Hour, ""},
		{"idle 2h kept", "2h", "", 2 * time.Hour, 12 * time.Hour, ""},
		{"idle garbage", "banana", "", 30 * time.Minute, 12 * time.Hour, "ADMIN_SESSION_IDLE is not a positive duration"},
		{"idle zero", "0", "", 30 * time.Minute, 12 * time.Hour, "ADMIN_SESSION_IDLE is not a positive duration"},
		{"idle negative", "-1h", "", 30 * time.Minute, 12 * time.Hour, "ADMIN_SESSION_IDLE is not a positive duration"},
		{"absolute 48h clamped", "", "48h", 4 * time.Hour, 12 * time.Hour, "ADMIN_SESSION_ABSOLUTE is above 12h0m0s"},
		{"absolute garbage", "", "banana", 4 * time.Hour, 8 * time.Hour, "ADMIN_SESSION_ABSOLUTE is not a positive duration"},
		{"absolute 2h kept", "", "2h", 2 * time.Hour, 2 * time.Hour, "under the documented range"},
		{"absolute 8h", "", "8h", 4 * time.Hour, 8 * time.Hour, ""},
		{"idle 10h absolute 8h", "10h", "8h", 8 * time.Hour, 8 * time.Hour, "is above ADMIN_SESSION_ABSOLUTE"},
		{"idle 12h absolute default", "12h", "", 12 * time.Hour, 12 * time.Hour, ""},
		{"both garbage", "qq-idle", "qq-abs", 30 * time.Minute, 8 * time.Hour, "ADMIN_SESSION_IDLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idle, abs, warns := ParseAdminSession(tc.idle, tc.abs)
			if idle != tc.wantIdle || abs != tc.wantAbs {
				t.Fatalf("got idle=%v abs=%v, want idle=%v abs=%v", idle, abs, tc.wantIdle, tc.wantAbs)
			}
			joined := strings.Join(warns, "\n")
			if tc.wantWarning == "" && len(warns) != 0 {
				t.Fatalf("unexpected warnings: %q", warns)
			}
			if tc.wantWarning != "" && !strings.Contains(joined, tc.wantWarning) {
				t.Fatalf("warnings %q do not contain %q", warns, tc.wantWarning)
			}
			if idle > abs {
				t.Fatalf("idle %v above absolute %v", idle, abs)
			}
			// A warning never echoes a malformed raw value.
			for _, raw := range []string{"banana", "qq-idle", "qq-abs"} {
				if raw == tc.idle || raw == tc.abs {
					if strings.Contains(joined, raw) {
						t.Fatalf("warning echoes the raw value %q: %q", raw, warns)
					}
				}
			}
		})
	}
}

// FromEnv picks the values up from the environment and never fails on them.
func TestFromEnvAdminSession(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "a-long-enough-test-value")
	t.Setenv("API_KEY_SEED", "test-seed")
	t.Setenv("ROUTER_AUTH", "admin:router-test-value")
	t.Setenv("ADMIN_SESSION_IDLE", "banana")
	t.Setenv("ADMIN_SESSION_ABSOLUTE", "48h")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv refused bad ADMIN_SESSION_* values: %v", err)
	}
	if c.AdminSessionIdle != 30*time.Minute || c.AdminSessionAbsolute != 12*time.Hour {
		t.Fatalf("got idle=%v abs=%v", c.AdminSessionIdle, c.AdminSessionAbsolute)
	}
	if len(c.AdminSessionWarnings) != 2 {
		t.Fatalf("want 2 warnings, got %q", c.AdminSessionWarnings)
	}
}
