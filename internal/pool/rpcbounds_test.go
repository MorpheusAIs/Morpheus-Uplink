package pool

// Bounds: stake and router calls per operation,
// the backoff schedule and budget, rated-bids retries, Retry-After.

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// TestStakeBoundPerRequest: random answers on every open POST over 1,000
// seeds. Per request, at most one open POST is not proven pre-send, and
// one operation makes at most 13 router calls.
func TestStakeBoundPerRequest(t *testing.T) {
	seeds := 1000
	if testing.Short() {
		seeds = 100
	}
	for seed := range seeds {
		c, lr, p, _ := rpcEnv(t)
		threeBidders(lr, modelB)
		rng := rand.New(rand.NewPCG(uint64(seed), 7))
		var mu sync.Mutex
		notProven, fastP := 0, 0
		lr.setOpenFault(func(string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			switch rng.IntN(7) {
			case 0: // provider fault: nothing sent
				return routerErr(pingIOTimeout), false
			case 1: // pre-send 429
				return rl429(stageBid), false
			case 2: // fast P*: pre-send by the 18 s rule once per operation
				fastP++
				if fastP > maxWindowRetries {
					notProven++
				}
				return rl429(stageBuild), false
			case 3: // slow P*: maybe sent
				c.Advance(25 * time.Second)
				notProven++
				return rl429(stageGas), false
			case 4: // receipt wait: sent and landed
				notProven++
				return routerErr(receiptWait429), true
			case 5: // unknown text
				notProven++
				return routerErr("session created on-chain (tx mined) but not yet visible after retries: x"), false
			default: // success
				notProven++
				return "", false
			}
		})
		lr.calls.Store(0)
		_, _, _ = p.Acquire(modelB, AcquireOpts{})
		calls := lr.calls.Load()
		if notProven > 1 {
			t.Fatalf("seed %d: %d open POSTs not proven pre-send, want ≤ 1 (bids %v)", seed, notProven, lr.bidsTried())
		}
		if calls > 13 {
			t.Fatalf("seed %d: %d router calls in one operation, want ≤ 13", seed, calls)
		}
	}
}

// TestBackoffDelaysAndBudget: a persistent pre-send 429 gets 4 retries
// within the equal-jitter bands, then router_transient with the next
// step (4 s); with slow answers no retry starts after 15 s.
func TestBackoffDelaysAndBudget(t *testing.T) {
	t.Run("schedule", func(t *testing.T) {
		_, lr, p, sl := rpcEnv(t)
		lr.setOpenFault(faultBurst(-1, rl429(stageBid), false))
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, 4*time.Second, 4*time.Second)
		assertBackoff(t, sl.sleeps(), transientMaxRetries)
		if n := len(lr.bidsTried()); n != 1+transientMaxRetries {
			t.Fatalf("open POSTs = %d, want %d", n, 1+transientMaxRetries)
		}
		assertOnlyBid(t, lr, bidID(modelB, leaseProv1))
		assertNoCooling(t, p, modelB)
	})
	t.Run("budget", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		var starts []time.Time
		lr.setOpenFault(func(string) (string, bool) {
			starts = append(starts, c.Now())
			c.Advance(4 * time.Second)
			return rl429(stageBid), false
		})
		start := c.Now()
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, time.Second, 4*time.Second)
		if len(starts) < 2 {
			t.Fatalf("POSTs = %d, want retries", len(starts))
		}
		for i, s := range starts {
			if s.Sub(start) > transientBudget {
				t.Fatalf("POST %d started %v after the operation start, past the %v budget", i+1, s.Sub(start), transientBudget)
			}
		}
		if len(starts) > 1+transientMaxRetries {
			t.Fatalf("POSTs = %d, want ≤ %d", len(starts), 1+transientMaxRetries)
		}
	})
}

// TestRatedBidsTransientRetry: two 429s on rated bids, then ok: opened
// after two backoff steps, nothing cools.
func TestRatedBidsTransientRetry(t *testing.T) {
	_, lr, p, sl := rpcEnv(t)
	n := 0
	lr.set(func(lr *leaseRouter) {
		lr.ratedFault = func(string) string {
			n++
			if n <= 2 {
				return rated429
			}
			return ""
		}
	})
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if !res.Opened {
		t.Fatalf("result %+v, want opened", res)
	}
	assertBackoff(t, sl.sleeps(), 2)
	assertNoCooling(t, p, modelB)
	t.Run("persistent", func(t *testing.T) {
		_, lr, p, sl := rpcEnv(t)
		lr.set(func(lr *leaseRouter) { lr.ratedFault = func(string) string { return rated429 } })
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, 4*time.Second, 4*time.Second)
		assertBackoff(t, sl.sleeps(), transientMaxRetries)
		if len(lr.bidsTried()) != 0 {
			t.Fatal("opened without rated bids")
		}
	})
}

// TestRetryAfterRouterTransientRange: every router_transient refusal
// carries 1 ≤ Retry-After ≤ 4 s, whatever step it was built from.
func TestRetryAfterRouterTransientRange(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, 0, 1, 250 * time.Millisecond, time.Second, 1500 * time.Millisecond,
		2 * time.Second, 4 * time.Second, transientCap, 10 * time.Second, time.Hour} {
		na := refusal(t, errTransient(d))
		if na.Reason != ReasonRouterTransient || na.RetryAfter < time.Second || na.RetryAfter > 4*time.Second || na.RetryAfter%time.Second != 0 {
			t.Errorf("errTransient(%v) = %+v, want router_transient with whole seconds in [1 s, 4 s]", d, na)
		}
	}
	for n := range 8 {
		if ra := refusal(t, errTransient(backoffStep(n))).RetryAfter; ra < time.Second || ra > 4*time.Second {
			t.Errorf("step %d: Retry-After %v", n, ra)
		}
	}
	// The constructors on the open path: retries spent, reconcile with no
	// match (not found), the slot deadline.
	t.Run("not found", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		lr.setOpenFault(faultBurst(1, routerErr(receiptWait429), false))
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, 4*time.Second, 4*time.Second)
	})
	t.Run("slot deadline", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		lr.set(func(lr *leaseRouter) {
			lr.ratedFault = func(string) string { c.Advance(slotHoldCap); return "" }
		})
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, 4*time.Second, 4*time.Second)
	})
	if na := refusal(t, errOpenBusyRefusal()); na.RetryAfter != 0 || na.Reason != ReasonOpenBusy {
		t.Errorf("open_busy = %+v, want no Retry-After", na)
	}
	_ = context.Background
}

func TestClampRetryAfterZeroIsOne(t *testing.T) {
	if got := clampRetryAfter(0); got != time.Second {
		t.Fatalf("clampRetryAfter(0) = %v, want 1s", got)
	}
	if got := clampRetryAfter(-time.Second); got != time.Second {
		t.Fatalf("clampRetryAfter(-1s) = %v, want 1s", got)
	}
	if got := clampRetryAfter(time.Hour); got != MaxRetryAfter {
		t.Fatalf("clampRetryAfter(1h) = %v, want %v", got, MaxRetryAfter)
	}
}
