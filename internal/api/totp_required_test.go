package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// With the 2FA switch on and nothing enrolled (after a reset), only the
// enroll path works.
func TestTwoFactorOnNotEnrolledAllowsEnrollPath(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	enrolled := adminClient(t, ts, cfg)
	_, codes := enroll2FA(t, enrolled, ts, cfg)
	if code, b := postJSON(t, enrolled, ts, cfg, "/admin/2fa/reset", `{"password":"`+cfg.AdminPassword+`","code":"`+codes[0]+`"}`); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, b)
	}
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
	if st["need2fa"] != true || st["enrolled"] != false || st["on"] != true {
		t.Fatalf("2fa status: %#v", st)
	}

	// Enroll start must work with CSRF.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/admin/2fa/enroll/start", strings.NewReader(`{"password":"`+cfg.AdminPassword+`"}`))
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

func TestRequireSessionPollDoesNotExtendIdle(t *testing.T) {
	cfg := &config.Config{
		RouterURL:          "http://127.0.0.1:9",
		RouterUser:         "uplink",
		RouterPass:         "x",
		AdminPassword:      "adminpass-ok-xx",
		APIKeySeed:         "poll-idle-seed",
		DataDir:            t.TempDir(),
		ActiveModelsURL:    "http://127.0.0.1:9/models",
		SessionDurationSec: 600,
		DevHTTP:            true,
		WebPublicURL:       "http://127.0.0.1",
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := wiredServer(cfg, st, rc, cfg.SessionDurationSec)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WebPublicURL = u.Scheme + "://" + u.Host

	raw, err := srv.sessions.Create("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	before, ok := srv.sessions.Lookup(raw, false)
	if !ok {
		t.Fatal("session missing")
	}
	time.Sleep(25 * time.Millisecond)

	h := srv.requireSession(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	makeReq := func(poll bool) *http.Request {
		rr := httptest.NewRecorder()
		adminauth.SetSessionCookie(rr, raw, true)
		req := httptest.NewRequest(http.MethodGet, "/admin/2fa/status", nil)
		for _, c := range rr.Result().Cookies() {
			req.AddCookie(c)
		}
		if poll {
			req.Header.Set("X-Uplink-Poll", "1")
		}
		req.Header.Set("Origin", cfg.WebPublicURL)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		return req
	}

	h.ServeHTTP(httptest.NewRecorder(), makeReq(true))
	after, ok := srv.sessions.Lookup(raw, false)
	if !ok {
		t.Fatal("session missing after poll")
	}
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Fatalf("poll extended idle: before=%v after=%v", before.LastSeen, after.LastSeen)
	}

	time.Sleep(25 * time.Millisecond)
	h.ServeHTTP(httptest.NewRecorder(), makeReq(false))
	after2, ok := srv.sessions.Lookup(raw, false)
	if !ok {
		t.Fatal("session missing after non-poll")
	}
	if !after2.LastSeen.After(before.LastSeen) {
		t.Fatalf("non-poll should extend idle: before=%v after=%v", before.LastSeen, after2.LastSeen)
	}
}
