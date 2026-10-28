package config

import "testing"

// S6: logLevel parsing (case and the "warning" alias; anything else is "").
func TestS6ParseLogLevel(t *testing.T) {
	for v, want := range map[string]string{"": "", "debug": "debug", "WARNING": "warn", "warn": "warn", "error": "error", "info": "info", "loud": ""} {
		if got := ParseLogLevel(v); got != want {
			t.Errorf("%q: got %q want %q", v, got, want)
		}
	}
}
