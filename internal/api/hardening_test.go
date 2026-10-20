package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

func TestKeysMaskedByDefault(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/keys", nil)
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		MasterKey string `json:"masterKey"`
		Masked    bool   `json:"masked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Masked {
		t.Fatal("expected masked=true")
	}
	if body.MasterKey == "" || body.MasterKey == cfg.APIKeySeed {
		t.Fatalf("master key should be masked, got %q", body.MasterKey)
	}
	// reveal=1 returns full
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/keys?reveal=1", nil)
	resp2, err := ac.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var body2 struct {
		MasterKey string `json:"masterKey"`
		Masked    bool   `json:"masked"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&body2)
	if body2.Masked || body2.MasterKey == body.MasterKey {
		t.Fatalf("reveal should return full key; masked=%v", body2.Masked)
	}
}

func TestLoginLockoutBackoff(t *testing.T) {
	ts, _, _ := newTestServer(t)
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader(`{"password":"wrong-password-here"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("iter %d: want 401/429 got %d", i, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader(`{"password":"wrong-password-here"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected lockout after 5 failures, status=%d retry=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header")
	}
}

func TestBasicAuthRejectedOnAdmin(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/status", nil)
	req.SetBasicAuth("admin", cfg.AdminPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Basic on /admin: status=%d, want 401", resp.StatusCode)
	}
}

func TestValidateAdminPassword(t *testing.T) {
	if err := config.ValidateAdminPassword("short", ""); err == nil {
		t.Fatal("expected short refuse")
	}
	if err := config.ValidateAdminPassword("passwordpassword", ""); err == nil {
		t.Fatal("expected weak refuse")
	}
	if err := config.ValidateAdminPassword("good-admin-pass", "admin:good-admin-pass"); err == nil {
		t.Fatal("expected cookie equality refuse")
	}
	if err := config.ValidateAdminPassword("good-admin-pass-ok", ""); err != nil {
		t.Fatal(err)
	}
}
