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

// securityBool: unset/empty/blank is false, booleans parse, anything else
// falls back to the safe value and is reported by name.
func TestMasterKeyAdminParsing(t *testing.T) {
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
		{"spaces_around_true", " true ", false, true, false},
		{"zero", "0", false, false, false},
		{"literal_compose_default", "${MASTER_KEY_ADMIN:-false}", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBase(t)
			setOrUnset(t, "MASTER_KEY_ADMIN", tc.val, tc.unset)
			c, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if c.MasterKeyAdmin != tc.want {
				t.Fatalf("MASTER_KEY_ADMIN=%q: MasterKeyAdmin=%v, want %v", tc.val, c.MasterKeyAdmin, tc.want)
			}
			var want []string
			if tc.malformed {
				want = []string{"MASTER_KEY_ADMIN"}
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
	t.Setenv("MASTER_KEY_ADMIN", "true")
	if c, err := FromEnv(); err != nil || !c.MasterKeyAdmin || c.MalformedBoolVars != nil {
		t.Fatalf("MASTER_KEY_ADMIN=true: %+v %v", c, err)
	}
}
