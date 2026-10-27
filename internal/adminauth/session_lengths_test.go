package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newClockedStoreWith(idle, absolute time.Duration) (*SessionStore, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := NewSessionStoreWith(idle, absolute)
	s.SetClock(clk.Now)
	return s, clk
}

// Defaults are idle 4h and absolute 12h; zero or negative values fall back
// to them, and idle is capped at absolute.
func TestSessionStoreLengths(t *testing.T) {
	cases := []struct {
		idle, abs         time.Duration
		wantIdle, wantAbs time.Duration
	}{
		{0, 0, 4 * time.Hour, 12 * time.Hour},
		{-time.Second, -time.Second, 4 * time.Hour, 12 * time.Hour},
		{2 * time.Hour, 6 * time.Hour, 2 * time.Hour, 6 * time.Hour},
		{10 * time.Hour, 8 * time.Hour, 8 * time.Hour, 8 * time.Hour},
	}
	for _, c := range cases {
		s := NewSessionStoreWith(c.idle, c.abs)
		if s.Idle() != c.wantIdle || s.Absolute() != c.wantAbs {
			t.Errorf("NewSessionStoreWith(%v, %v) = %v/%v, want %v/%v", c.idle, c.abs, s.Idle(), s.Absolute(), c.wantIdle, c.wantAbs)
		}
	}
	if s := NewSessionStore(); s.Idle() != SessionIdle || s.Absolute() != SessionAbsolute {
		t.Fatalf("NewSessionStore: %v/%v", s.Idle(), s.Absolute())
	}
	if SessionIdle != 4*time.Hour || SessionAbsolute != 12*time.Hour {
		t.Fatalf("defaults: idle %v absolute %v", SessionIdle, SessionAbsolute)
	}
}

// A configured idle expires at idle + 1 s and not before; a configured
// absolute ends the session whatever the activity.
func TestSessionStoreConfiguredExpiry(t *testing.T) {
	s, clk := newClockedStoreWith(2*time.Hour, 6*time.Hour)
	raw, _ := s.Create("127.0.0.1")
	clk.Advance(2 * time.Hour)
	if _, ok := s.Lookup(raw, false); !ok {
		t.Fatal("expired at exactly idle")
	}
	clk.Advance(time.Second)
	if _, ok := s.Lookup(raw, false); ok {
		t.Fatal("alive past idle + 1s")
	}

	raw, _ = s.Create("127.0.0.1")
	for i := 0; i < 6; i++ {
		clk.Advance(time.Hour)
		if _, ok := s.Lookup(raw, true); !ok {
			t.Fatalf("interactive session ended early at %dh", i+1)
		}
	}
	clk.Advance(time.Second)
	if _, ok := s.Lookup(raw, true); ok {
		t.Fatal("session alive past absolute")
	}
}

// StampStepUp sets StepUpAt; Fresh uses it, else CreatedAt.
func TestStampStepUpAndFresh(t *testing.T) {
	s, clk := newClockedStoreWith(0, 0)
	raw, _ := s.Create("127.0.0.1")
	sess, _ := s.Lookup(raw, false)
	if !sess.StepUpAt.IsZero() || !sess.Fresh(clk.Now(), 30*time.Minute) {
		t.Fatal("a new session must be fresh via CreatedAt")
	}
	clk.Advance(31 * time.Minute)
	sess, _ = s.Lookup(raw, true)
	if sess.Fresh(clk.Now(), 30*time.Minute) {
		t.Fatal("fresh 31 min after login without step-up")
	}
	at, ok := s.StampStepUp(raw)
	if !ok || !at.Equal(clk.Now()) {
		t.Fatalf("StampStepUp: %v %v", at, ok)
	}
	sess, _ = s.Lookup(raw, true)
	if !sess.StepUpAt.Equal(at) || !sess.Fresh(clk.Now(), 30*time.Minute) {
		t.Fatal("not fresh right after step-up")
	}
	clk.Advance(30*time.Minute + time.Second)
	if sess.Fresh(clk.Now(), 30*time.Minute) {
		t.Fatal("fresh 30m01s after step-up")
	}
	if _, ok := s.StampStepUp("00"); ok {
		t.Fatal("stamp on a malformed token")
	}
	if _, ok := s.StampStepUp("0000000000000000000000000000000000000000000000000000000000000000"); ok {
		t.Fatal("stamp on an unknown token")
	}
}

// ObserveIP reports each change once and does not end the session.
func TestObserveIP(t *testing.T) {
	s, _ := newClockedStoreWith(0, 0)
	raw, _ := s.Create("192.0.2.1")
	if _, changed := s.ObserveIP(raw, "192.0.2.1"); changed {
		t.Fatal("same address reported as a change")
	}
	prev, changed := s.ObserveIP(raw, "192.0.2.2")
	if !changed || prev != "192.0.2.1" {
		t.Fatalf("change: %q %v", prev, changed)
	}
	if _, changed := s.ObserveIP(raw, "192.0.2.2"); changed {
		t.Fatal("the same change reported twice")
	}
	if _, ok := s.Lookup(raw, true); !ok {
		t.Fatal("session ended on an address change")
	}
}

// RevokeAllExcept keeps one session.
func TestRevokeAllExcept(t *testing.T) {
	s, _ := newClockedStoreWith(0, 0)
	keep, _ := s.Create("127.0.0.1")
	other, _ := s.Create("127.0.0.1")
	if n := s.RevokeAllExcept(keep); n != 1 {
		t.Fatalf("revoked %d, want 1", n)
	}
	if _, ok := s.Lookup(keep, false); !ok {
		t.Fatal("kept session revoked")
	}
	if _, ok := s.Lookup(other, false); ok {
		t.Fatal("other session alive")
	}
}

// Cookie MaxAge follows the absolute length; flags are unchanged.
func TestSetSessionCookieMaxAge(t *testing.T) {
	for _, dev := range []bool{true, false} {
		for _, abs := range []time.Duration{6 * time.Hour, 12 * time.Hour, 0} {
			rec := httptest.NewRecorder()
			SetSessionCookie(rec, "tok", dev, abs)
			c := (&http.Response{Header: rec.Header()}).Cookies()[0]
			want := int(abs.Seconds())
			if abs == 0 {
				want = int(SessionAbsolute.Seconds())
			}
			if c.MaxAge != want || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure == dev || c.Name != CookieName(dev) {
				t.Fatalf("dev=%v abs=%v cookie %+v", dev, abs, c)
			}
		}
	}
}
