package router

import "testing"

func TestActualDurationSec(t *testing.T) {
	s := Session{OpenedAt: 1000, ClosedAt: 1060}
	if got := s.ActualDurationSec(); got != 60 {
		t.Fatalf("got %d want 60", got)
	}
	if (Session{OpenedAt: 1000, ClosedAt: 0}).ActualDurationSec() != 0 {
		t.Fatal("open session should report 0")
	}
	if (Session{OpenedAt: 0, ClosedAt: 50}).ActualDurationSec() != 0 {
		t.Fatal("missing open should report 0")
	}
}

func TestDurationFromOpen(t *testing.T) {
	if got := durationFromOpen(1 << 62); got < 1 {
		t.Fatalf("expected at least 1s, got %d", got)
	}
}
