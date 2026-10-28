package config

import (
	"bytes"
	"log"
	"os"
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

// S5 defaults for the long-job settings.
func TestS5LongJobEnvDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	c, _ := fromEnvQuiet(t)
	got := []int{c.SessionDurationMax, c.ForwardIdleSec, c.SessionIdleGraceSec}
	want := []int{1800, 180, 300}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("defaults %v want %v", got, want)
		}
	}
	if !c.SessionRelease {
		t.Fatal("sessionRelease default is not true")
	}
}
