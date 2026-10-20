package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// A security switch set to a non-boolean is reported by name (never value)
// on /admin/2fa/status and /admin/status, so the GUI can warn.
func TestConfigWarnReported(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.MalformedBoolVars = []string{"ADMIN_2FA_REQUIRED"}
	ac := adminClient(t, ts, cfg)
	get := func(path string) map[string]any {
		t.Helper()
		resp, err := ac.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	want := []any{"ADMIN_2FA_REQUIRED"}
	if got := get("/admin/2fa/status")["configWarn"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("/admin/2fa/status configWarn=%v, want %v", got, want)
	}
	tf, _ := get("/admin/status")["twoFactor"].(map[string]any)
	if got := tf["configWarn"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("/admin/status twoFactor.configWarn=%v, want %v", got, want)
	}
	cfg.MalformedBoolVars = nil
	if got := get("/admin/2fa/status")["configWarn"]; got != nil {
		t.Fatalf("configWarn without malformed vars: %v", got)
	}
}
