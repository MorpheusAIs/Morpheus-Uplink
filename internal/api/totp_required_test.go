package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdmin2FARequiredAllowsEnrollPath(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	cfg.Admin2FARequired = true
	ac := adminClient(t, ts, cfg)

	// Status (requireAdmin) must 403 until enrolled.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/status", nil)
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status before enroll: %d want 403", resp.StatusCode)
	}

	// 2FA status (requireSession) must work.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/2fa/status", nil)
	resp, err = ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("2fa status: %d", resp.StatusCode)
	}
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st["need2fa"] != true || st["enrolled"] != false {
		t.Fatalf("2fa status: %#v", st)
	}

	// Enroll start must work with CSRF.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/admin/2fa/enroll/start", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	withCSRF(req, cfg)
	resp, err = ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll start: %d", resp.StatusCode)
	}
}
