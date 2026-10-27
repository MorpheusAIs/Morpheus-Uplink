package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// S1 test 4: the session lengths are the fixed constants idle 4h and
// absolute 12h.
func TestSessionLengthsAreFixedConstants(t *testing.T) {
	if SessionIdle != 4*time.Hour || SessionAbsolute != 12*time.Hour {
		t.Fatalf("constants: idle %v absolute %v, want 4h / 12h", SessionIdle, SessionAbsolute)
	}
	s := NewSessionStore()
	if s.Idle() != SessionIdle || s.Absolute() != SessionAbsolute {
		t.Fatalf("store: %v / %v", s.Idle(), s.Absolute())
	}
}

// Idle expires at 4h + 1 s and not before; absolute ends the session at
// 12h whatever the activity.
func TestSessionStoreFixedExpiry(t *testing.T) {
	s, clk := newClockedStore()
	raw, _ := s.Create("127.0.0.1")
	clk.Advance(4 * time.Hour)
	if _, ok := s.Lookup(raw, false); !ok {
		t.Fatal("expired at exactly 4h idle")
	}
	clk.Advance(time.Second)
	if _, ok := s.Lookup(raw, false); ok {
		t.Fatal("alive past 4h + 1s idle")
	}

	raw, _ = s.Create("127.0.0.1")
	for i := 0; i < 12; i++ {
		clk.Advance(time.Hour)
		if _, ok := s.Lookup(raw, true); !ok {
			t.Fatalf("interactive session ended early at %dh", i+1)
		}
	}
	clk.Advance(time.Second)
	if _, ok := s.Lookup(raw, true); ok {
		t.Fatal("session alive past 12h absolute")
	}
}

// StampStepUp sets StepUpAt; Fresh uses it, else CreatedAt.
func TestStampStepUpAndFresh(t *testing.T) {
	s, clk := newClockedStore()
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
	s, _ := newClockedStore()
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
	s, _ := newClockedStore()
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

// Cookie MaxAge is the 12h absolute constant; flags are unchanged.
func TestSetSessionCookieMaxAge(t *testing.T) {
	for _, dev := range []bool{true, false} {
		rec := httptest.NewRecorder()
		SetSessionCookie(rec, "tok", dev)
		c := (&http.Response{Header: rec.Header()}).Cookies()[0]
		if c.MaxAge != 12*3600 || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure == dev || c.Name != CookieName(dev) {
			t.Fatalf("dev=%v cookie %+v", dev, c)
		}
	}
}
