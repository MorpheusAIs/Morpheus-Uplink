package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Gary SHOULD-3: config warnings are written at slog Warn, so a process
// running at log level warn still shows them (std log goes at Info).
func TestConfigWarningsAtWarnLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	setRequired(t)
	t.Setenv("ROUTER_AUTH", "admin:secret")
	t.Setenv("UPLINK_FORWARD_IDLE_SECONDS", "bogus")
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "UPLINK_FORWARD_IDLE_SECONDS") || strings.Contains(out, "bogus") {
		t.Fatalf("warning not at warn level (or value leaked): %q", out)
	}
}
