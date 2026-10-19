package adminauth

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClockedStore() (*SessionStore, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := NewSessionStore()
	s.now = clk.Now
	return s, clk
}

// A session that only sees background polls (extendIdle=false) must expire
// at SessionIdle even though it was "used" the whole time.
func TestPollOnlySessionExpiresAtIdle(t *testing.T) {
	s, clk := newClockedStore()
	raw, err := s.Create("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	step := time.Minute
	for elapsed := step; elapsed <= SessionIdle; elapsed += step {
		clk.Advance(step)
		if _, ok := s.Lookup(raw, false); !ok {
			t.Fatalf("poll lookup failed early at %v", elapsed)
		}
	}
	clk.Advance(time.Second)
	if _, ok := s.Lookup(raw, false); ok {
		t.Fatal("poll-only session survived past SessionIdle")
	}
	// Expired session is evicted; a non-poll lookup cannot revive it.
	if _, ok := s.Lookup(raw, true); ok {
		t.Fatal("expired session revived by non-poll lookup")
	}
}

// Interactive (non-poll) use extends idle, but never past SessionAbsolute.
func TestInteractiveSessionExtendsIdleUntilAbsolute(t *testing.T) {
	s, clk := newClockedStore()
	raw, err := s.Create("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	step := SessionIdle - time.Minute
	var elapsed time.Duration
	for elapsed+step <= SessionAbsolute {
		clk.Advance(step)
		elapsed += step
		if _, ok := s.Lookup(raw, true); !ok {
			t.Fatalf("interactive session expired early at %v", elapsed)
		}
	}
	clk.Advance(SessionAbsolute - elapsed + time.Second)
	if _, ok := s.Lookup(raw, true); ok {
		t.Fatal("session survived past SessionAbsolute")
	}
}
