package pool

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A2 (DETAIL §14.4 #11): Retry-After is capped at 600 s before it is
// rounded up to whole seconds, is at least 1 s, and is absent (0) when no
// cool-down is known.
func TestA2ClampRetryAfter(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, time.Second}, // never a zero Retry-After
		{-time.Second, time.Second},
		{200 * time.Millisecond, time.Second},
		{time.Second, time.Second},
		{1500 * time.Millisecond, 2 * time.Second},
		{599100 * time.Millisecond, 600 * time.Second},
		{600400 * time.Millisecond, 600 * time.Second}, // capped before the ceil
		{900 * time.Second, 600 * time.Second},
	} {
		if got := clampRetryAfter(c.in); got != c.want {
			t.Errorf("clampRetryAfter(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A2 (DETAIL §14.4 #2): an exhausted bid walk is a typed error found with
// errors.Is, keeps the tried count and the cause, and carries no ids; the
// sentinel value is matched by NoAvailableError.
func TestA2TypedErrors(t *testing.T) {
	cause := errors.New("open failed")
	var err error = &walkError{tried: 3, last: cause}
	if !errors.Is(err, errBidWalkExhausted) || !errors.Is(err, cause) {
		t.Fatalf("walk error does not match: %v", err)
	}
	if got := err.Error(); got != "open on another provider (3 tried): open failed" {
		t.Fatalf("walk error text %q", got)
	}
	na := error(&NoAvailableError{RetryAfter: 5 * time.Second})
	wrapped := fmt.Errorf("x: %w", na)
	if !errors.Is(wrapped, ErrNoAvailableProvider) || errors.Is(wrapped, ErrNoEligibleProvider) {
		t.Fatalf("NoAvailableError matching: %v", wrapped)
	}
	var got *NoAvailableError
	if !errors.As(wrapped, &got) || got.RetryAfter != 5*time.Second || na.Error() != ErrNoAvailableProvider.Error() {
		t.Fatalf("NoAvailableError value: %+v %q", got, na.Error())
	}
}

// A2 (DETAIL §14.4 #5, retired branch): a failover on a session that was
// already failed over, whose replacement is gone, reopens only through the
// fail-fast open: with every provider cooling nothing is opened and the
// caller gets ErrNoAvailableProvider (audited no_available by the API).
func TestA2RetiredBranchNoAvailable(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetProviderCooldown(10 * time.Minute)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	fr, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k1", DurationSec: 600, Reopen: true})
	if err != nil || fr.Provider != leaseProv2 {
		t.Fatalf("first failover: %+v %v", fr, err)
	}
	fr.Lease.Done()
	p.MarkCooling(modelA, leaseProv2)
	c.Advance(2 * time.Hour) // the replacement has expired
	before, _ := lr.snapshot()
	fr2, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k1", DurationSec: 600, Reopen: true})
	if !errors.Is(err, ErrNoAvailableProvider) || !fr2.Replaced || fr2.SessionID != "" {
		t.Fatalf("retired branch: %+v %v", fr2, err)
	}
	var na *NoAvailableError
	if !errors.As(err, &na) || na.RetryAfter < time.Second || na.RetryAfter > MaxRetryAfter {
		t.Fatalf("retry after: %+v", na)
	}
	if after, _ := lr.snapshot(); len(after) != len(before) {
		t.Fatalf("retired branch opened %d sessions", len(after)-len(before))
	}
}
