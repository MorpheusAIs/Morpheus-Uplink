package config

import (
	"os"
	"reflect"
	"testing"
)

// setOrUnset sets key to v, or unsets it when unset is true (restored after).
func setOrUnset(t *testing.T, key, v string, unset bool) {
	t.Helper()
	t.Setenv(key, v)
	if unset {
		_ = os.Unsetenv(key)
	}
}

// setBase sets the required variables plus a router credential.
func setBase(t *testing.T) {
	t.Helper()
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:router-pass-ok")
}

func TestAdmin2FARequiredFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		val       string
		unset     bool
		want      bool
		malformed bool
	}{
		{"true", "true", false, true, false},
		{"false", "false", false, false, false},
		{"unset", "", true, false, false},
		{"empty_like_unset", "", false, false, false},
		{"blank_like_unset", "  ", false, false, false},
		{"literal_compose_default", "${ADMIN_2FA_REQUIRED:-true}", false, true, true},
		{"literal_compose_default_false", "${ADMIN_2FA_REQUIRED:-false}", false, true, true},
		{"yes_question", "yes?", false, true, true},
		{"spaces_around_false", " false ", false, false, false},
		{"zero", "0", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBase(t)
			setOrUnset(t, "MASTER_KEY_ADMIN", "", true)
			setOrUnset(t, "ADMIN_2FA_REQUIRED", tc.val, tc.unset)
			c, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if c.Admin2FARequired != tc.want {
				t.Fatalf("ADMIN_2FA_REQUIRED=%q: Admin2FARequired=%v, want %v", tc.val, c.Admin2FARequired, tc.want)
			}
			var want []string
			if tc.malformed {
				want = []string{"ADMIN_2FA_REQUIRED"}
			}
			if !reflect.DeepEqual(c.MalformedBoolVars, want) {
				t.Fatalf("MalformedBoolVars=%v, want %v", c.MalformedBoolVars, want)
			}
		})
	}
}

// MASTER_KEY_ADMIN keeps the safe direction: anything but a boolean is false.
func TestMasterKeyAdminMalformedStaysOff(t *testing.T) {
	for _, v := range []string{"${MASTER_KEY_ADMIN:-true}", "yes?", "on"} {
		setBase(t)
		setOrUnset(t, "ADMIN_2FA_REQUIRED", "", true)
		t.Setenv("MASTER_KEY_ADMIN", v)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.MasterKeyAdmin {
			t.Fatalf("MASTER_KEY_ADMIN=%q enabled master key admin", v)
		}
		if !reflect.DeepEqual(c.MalformedBoolVars, []string{"MASTER_KEY_ADMIN"}) {
			t.Fatalf("MASTER_KEY_ADMIN=%q: MalformedBoolVars=%v", v, c.MalformedBoolVars)
		}
	}
	setBase(t)
	setOrUnset(t, "ADMIN_2FA_REQUIRED", "", true)
	t.Setenv("MASTER_KEY_ADMIN", "true")
	if c, err := FromEnv(); err != nil || !c.MasterKeyAdmin || c.MalformedBoolVars != nil {
		t.Fatalf("MASTER_KEY_ADMIN=true: %+v %v", c, err)
	}
}
