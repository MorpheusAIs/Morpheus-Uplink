package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// restartEnv is a console server whose router and pool point at a fake
// router that counts every request. With hook set, the restart hook counts
// calls; without, it is left nil. Both are set before the test server
// starts, as cmd/uplink does before listening.
func restartEnv(t *testing.T, hook bool) (*settingsEnv, *atomic.Int32, chan struct{}) {
	t.Helper()
	var routerHits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routerHits.Add(1)
		http.Error(w, "unexpected router call", http.StatusTeapot)
	}))
	t.Cleanup(fake.Close)
	called := make(chan struct{}, 4)
	e := newSettingsEnvPrep(t, nil, nil, func(srv *Server) {
		srv.router = router.New(fake.URL, "admin", "routerpass")
		srv.pool = pool.New(srv.router, 600)
		if hook {
			srv.SetRestartHook(func() { called <- struct{}{} })
		}
	})
	return e, &routerHits, called
}

func hookCalls(ch chan struct{}, wait time.Duration) int {
	n := 0
	deadline := time.After(wait)
	for {
		select {
		case <-ch:
			n++
		case <-deadline:
			return n
		}
	}
}

// Every refusal leaves the process alone: no cookie, an Authorization
// header (the master key included), a cross-site request, a stale session
// without step-up, and the wrong method.
func TestRestartRefusals(t *testing.T) {
	e, hits, called := restartEnv(t, true)
	anon := &http.Client{}
	if code, _ := e.do(t, anon, "POST", "/admin/restart", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", code)
	}
	master := keymaker.MasterKey(e.cfg.APIKeySeed)
	if code, _ := e.do(t, nil, "POST", "/admin/restart", "", map[string]string{"Authorization": "Bearer " + master}); code != http.StatusForbidden {
		t.Errorf("master key with cookie: %d", code)
	}
	if code, _ := e.do(t, anon, "POST", "/admin/restart", "", map[string]string{"Authorization": "Bearer " + master}); code != http.StatusForbidden {
		t.Errorf("master key alone: %d", code)
	}
	if code, _ := e.do(t, nil, "POST", "/admin/restart", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("cross-site: %d", code)
	}
	if code, _ := e.do(t, nil, "POST", "/admin/restart", "", map[string]string{"Origin": "", "Sec-Fetch-Site": ""}); code != http.StatusForbidden {
		t.Errorf("no CSRF headers: %d", code)
	}
	if code, _ := e.do(t, nil, "GET", "/admin/restart", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", code)
	}
	e.clk.Advance(StepUpMaxAge + time.Minute)
	if code, body := e.do(t, nil, "POST", "/admin/restart", "", nil); code != http.StatusUnauthorized || strings.TrimSpace(body) != errMsgStepUp {
		t.Errorf("stale session: %d %s", code, body)
	}
	if n := hookCalls(called, 50*time.Millisecond); n != 0 {
		t.Fatalf("restart ran %d time(s) on a refused request", n)
	}
	if len(e.audits("uplink_restart")) != 0 {
		t.Fatal("refused restart audited as done")
	}
	if hits.Load() != 0 {
		t.Fatalf("router called %d time(s)", hits.Load())
	}
}

// A fresh admin session restarts once: 202, one hook call, one audit line,
// a second request while restarting is 409, and the router is never called
// (no session close, no router restart).
func TestRestartRunsOnceWithoutTouchingRouter(t *testing.T) {
	e, hits, called := restartEnv(t, true)
	code, body := e.do(t, nil, "POST", "/admin/restart", "", nil)
	if code != http.StatusAccepted || !strings.Contains(body, `"restarting":true`) {
		t.Fatalf("restart %d %s", code, body)
	}
	if code, _ := e.do(t, nil, "POST", "/admin/restart", "", nil); code != http.StatusConflict {
		t.Errorf("second restart: %d", code)
	}
	if n := hookCalls(called, 100*time.Millisecond); n != 1 {
		t.Fatalf("hook calls %d", n)
	}
	if got := e.audits("uplink_restart"); len(got) != 1 {
		t.Fatalf("audit %v", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("router called %d time(s)", hits.Load())
	}
}

// After a step-up a stale session may restart; without a hook (not wired
// by cmd/uplink) the answer is 503 and nothing happens. Each case is a
// fresh server with its hook set, or left nil, before it starts serving.
func TestRestartStepUpAndNoHook(t *testing.T) {
	stepUp := func(t *testing.T, e *settingsEnv) {
		t.Helper()
		e.clk.Advance(StepUpMaxAge + time.Minute)
		if code, body := e.do(t, nil, "POST", "/admin/step-up", `{"password":"`+e.cfg.AdminPassword+`","code":""}`, nil); code != 200 {
			t.Fatalf("step-up %d %s", code, body)
		}
	}
	t.Run("no hook", func(t *testing.T) {
		e, hits, called := restartEnv(t, false)
		stepUp(t, e)
		if code, _ := e.do(t, nil, "POST", "/admin/restart", "", nil); code != http.StatusServiceUnavailable {
			t.Fatalf("no hook: %d", code)
		}
		if n := hookCalls(called, 50*time.Millisecond); n != 0 {
			t.Fatalf("hook calls %d", n)
		}
		if got := e.audits("uplink_restart"); len(got) != 0 {
			t.Fatalf("audit %v", got)
		}
		if hits.Load() != 0 {
			t.Fatalf("router called %d time(s)", hits.Load())
		}
	})
	t.Run("hook after step-up", func(t *testing.T) {
		e, hits, called := restartEnv(t, true)
		stepUp(t, e)
		if code, _ := e.do(t, nil, "POST", "/admin/restart", "", nil); code != http.StatusAccepted {
			t.Fatalf("after step-up: %d", code)
		}
		if n := hookCalls(called, 100*time.Millisecond); n != 1 {
			t.Fatalf("hook calls %d", n)
		}
		if hits.Load() != 0 {
			t.Fatalf("router called %d time(s)", hits.Load())
		}
	})
}

// With 2FA switched on but not enrolled, the console gate refuses the
// restart like any other admin change: no restart, no audit line, no
// router call.
func TestRestartRefusedUntil2FAEnrolled(t *testing.T) {
	e, hits, called := restartEnv(t, true)
	if err := e.srv.store.UpdateAuthState(func(a *store.AuthState) error {
		a.TwoFactorOn, a.Enrolled = true, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, body := e.do(t, nil, "POST", "/admin/restart", "", nil)
	if code != http.StatusForbidden || !strings.Contains(body, "2fa enrollment required") {
		t.Fatalf("2FA not enrolled: %d %s", code, body)
	}
	if n := hookCalls(called, 50*time.Millisecond); n != 0 {
		t.Fatalf("restart ran %d time(s) without 2FA enrolment", n)
	}
	if len(e.audits("uplink_restart")) != 0 {
		t.Fatal("refused restart audited as done")
	}
	if hits.Load() != 0 {
		t.Fatalf("router called %d time(s)", hits.Load())
	}
}
