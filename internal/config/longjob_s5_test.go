package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func fromEnvQuiet(t *testing.T) (*Config, string) {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	c, err := FromEnv()
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return c, buf.String()
}

// S5-29: UPLINK_INFLIGHT_MAX unset, 0, malformed or out of range → 32,
// with a warning that never carries the value.
func TestS5InflightMaxEnv(t *testing.T) {
	for v, want := range map[string]int{"": 32, "0": 32, "banana": 32, "99999": 32, "-1": 32, "8": 8, "1024": 1024} {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "admin:secret")
		t.Setenv("UPLINK_INFLIGHT_MAX", v)
		c, logs := fromEnvQuiet(t)
		if c.InflightMax != want {
			t.Errorf("%q: got %d want %d", v, c.InflightMax, want)
		}
		if v != "" && want == 32 && !strings.Contains(logs, "UPLINK_INFLIGHT_MAX") {
			t.Errorf("%q: no warning", v)
		}
		if v == "banana" && strings.Contains(logs, "banana") {
			t.Errorf("warning carries the value")
		}
	}
}

// S5 env defaults and bounds for the long-job settings.
func TestS5LongJobEnvDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	c, _ := fromEnvQuiet(t)
	got := []int{c.SessionDurationMax, c.ForwardIdleSec, c.SessionIdleGraceSec, c.LongPerKeyModel, c.LongPerKey, c.LongSessionsMax, c.OpensPerKeyModelHour}
	want := []int{1800, 180, 300, 1, 2, 3, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("defaults %v want %v", got, want)
		}
	}
	for _, kv := range [][2]string{{"UPLINK_SESSION_DURATION_MAX", "100"}, {"UPLINK_FORWARD_IDLE_SECONDS", "5"}, {"UPLINK_SESSION_IDLE_GRACE_SECONDS", "99999"}} {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "admin:secret")
		t.Setenv(kv[0], kv[1])
		c, logs := fromEnvQuiet(t)
		if c.SessionDurationMax != 1800 || c.ForwardIdleSec != 180 || c.SessionIdleGraceSec != 300 || !strings.Contains(logs, kv[0]) {
			t.Fatalf("%s=%s not rejected to default: %+v", kv[0], kv[1], c)
		}
		t.Setenv(kv[0], "")
	}
}
