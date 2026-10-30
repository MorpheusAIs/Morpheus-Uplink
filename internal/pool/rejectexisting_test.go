package pool

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// hiddenOnChain puts a live session of model on chain that the next
// session-list answers leave out (the router's visibility lag), so it
// lands after the pre-open scan; the session detail shows it.
func hiddenOnChain(c *fakeClock, lr *leaseRouter, model, prov string, ends time.Time) string {
	id := fmt.Sprintf("0x%064x", 0xf00d)
	lr.addChainOn(id, model, prov, c.Now().Add(-time.Minute), ends)
	lr.set(func(lr *leaseRouter) {
		lr.landed[id] = true
		lr.hideLeft = 1
	})
	return id
}

func assertRejects(t *testing.T, lr *leaseRouter, want ...bool) {
	t.Helper()
	got := lr.rejectsSeen()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rejectExisting flags %v, want %v", got, want)
	}
}

func TestRejectExistingOnFirstOpen(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if !res.Opened {
		t.Fatalf("result %+v, want opened", res)
	}
	assertRejects(t, lr, true)
}

// A retry after a cut open: the flag is set; the router refuses it while
// the cut open is still minting, so no second session is opened.
func TestRejectExistingOnRetryAfterCutOpen(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	cutOpen(t, lr, p, modelB, 3*time.Minute)
	waitUntil(t, "late mint recorded", func() bool { lr.mu.Lock(); defer lr.mu.Unlock(); return lr.n == 1 })
	_, _, err := p.Acquire(modelB, AcquireOpts{})
	assertTransient(t, err, time.Second, transientCap)
	assertRejects(t, lr, true, true)
	lr.mu.Lock()
	n, refused := lr.n, lr.refused
	lr.mu.Unlock()
	if n != 1 || refused != 1 {
		t.Fatalf("minted %d, refused %d: want 1 and 1 (no second session)", n, refused)
	}
	c.Advance(3*time.Minute + time.Second)
	lateAdopt(t, c, lr, p, 2)
}

// A retry after a maybe-sent open whose session the list does not show
// yet: the router names it and it is adopted (our own open: Opened).
func TestRejectExistingOnRetryAfterMaybeSent(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	lr.set(func(lr *leaseRouter) { lr.hideLanded = 2 + reconcilePolls })
	lr.setOpenFault(func(string) (string, bool) {
		return `{"error":"failed to send transaction: parse session receipt failed: failed to wait for confirmations 429 Too Many Requests"}`, true
	})
	_, _, err := p.Acquire(modelB, AcquireOpts{})
	assertTransient(t, err, time.Second, transientCap)
	lr.setOpenFault(nil)
	lr.mu.Lock()
	landed := lr.order[len(lr.order)-1]
	lr.mu.Unlock()
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	assertRejects(t, lr, true, true)
	if res.SessionID != landed || !res.Opened {
		t.Fatalf("result %+v, want %s adopted as our own open", res, landed)
	}
	lr.mu.Lock()
	n := lr.n
	lr.mu.Unlock()
	if n != 1 {
		t.Fatalf("minted %d, want 1", n)
	}
	p.mu.Lock()
	pending := len(p.pending)
	p.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d open attempts still pending after the match", pending)
	}
}

// The router names a session another opener made: it is adopted like a
// scan adoption (not Opened by us), and no session is minted.
func TestRejectExisting409AdoptsNamedSession(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	id := hiddenOnChain(c, lr, modelB, leaseProv1, c.Now().Add(time.Hour))
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	assertRejects(t, lr, true)
	if res.SessionID != id || res.Opened {
		t.Fatalf("result %+v, want %s adopted (not opened)", res, id)
	}
	lr.mu.Lock()
	n := lr.n
	lr.mu.Unlock()
	if n != 0 {
		t.Fatalf("minted %d sessions, want 0", n)
	}
	// It is pooled: the next Acquire leases it without any open.
	_, res = mustAcquire(t, p, modelB, AcquireOpts{})
	if res.SessionID != id || len(lr.bidsTried()) != 1 {
		t.Fatalf("result %+v after %d opens, want %s pooled", res, len(lr.bidsTried()), id)
	}
}

// A named session Uplink may not use (ending, or on an excluded provider)
// is not adopted: router_transient, nothing opened.
func TestRejectExisting409NotAdoptable(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov string
		ends time.Duration
		excl bool
	}{
		{"ending", leaseProv1, expiryBuffer - time.Second, false},
		{"excluded", leaseProv1, time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			if tc.excl {
				p.SetProviderExclude([]string{tc.prov})
			}
			hiddenOnChain(c, lr, modelB, tc.prov, c.Now().Add(tc.ends))
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			assertTransient(t, err, time.Second, transientCap)
			assertRejects(t, lr, true)
			if ids := pooledIDs(p); len(ids) != 0 {
				t.Fatalf("pooled %v, want nothing", ids)
			}
		})
	}
}

// Failover and replacement opens never send the flag.
func TestRejectExistingOffForReplacements(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		l, res := mustAcquire(t, p, modelB, AcquireOpts{})
		l.Done()
		fr, err := p.Failover(modelB, res.SessionID, FailoverOpts{Reopen: true})
		if err != nil || !fr.Opened {
			t.Fatalf("failover %+v, %v", fr, err)
		}
		assertRejects(t, lr, true, false)
	})
	t.Run("failover_after_replacement_gone", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		l, res := mustAcquire(t, p, modelB, AcquireOpts{})
		l.Done()
		fr, err := p.Failover(modelB, res.SessionID, FailoverOpts{Reopen: true})
		if err != nil {
			t.Fatal(err)
		}
		fr.Lease.Done()
		p.Invalidate(modelB) // the replacement is gone
		if _, err := p.Failover(modelB, res.SessionID, FailoverOpts{Reopen: true}); err != nil {
			t.Fatal(err)
		}
		got := lr.rejectsSeen()
		if len(got) != 3 || got[1] || got[2] {
			t.Fatalf("rejectExisting flags %v, want [true false false]", got)
		}
	})
	t.Run("near_expiry", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		l, res := mustAcquire(t, p, modelB, AcquireOpts{})
		l.Done()
		c.Advance(time.Duration(p.DurationSec())*time.Second - expiryBuffer + time.Second)
		_, r2 := mustAcquire(t, p, modelB, AcquireOpts{})
		if !r2.Opened || r2.SessionID == res.SessionID {
			t.Fatalf("result %+v, want a replacement opened", r2)
		}
		assertRejects(t, lr, true, false)
	})
	t.Run("release_then_reopen", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
		l.Done()
		if out := p.Release(modelB, "k1"); !out.Released {
			t.Fatalf("release %+v", out)
		}
		_, r2 := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
		if !r2.Opened || r2.SessionID == res.SessionID {
			t.Fatalf("result %+v, want a new session", r2)
		}
		assertRejects(t, lr, true, false)
	})
}

// A funding error before the send fails fast: one POST, no retry, no
// cool-down, no other bid.
func TestWalletFundsFailFast(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"approve_eth", "failed to send transaction: approve MOR allowance failed: insufficient ETH for gas — add ETH to your wallet to cover transaction fees. Original: insufficient funds for gas * price + value"},
		{"open_mor", "failed to send transaction: open session failed: insufficient MOR balance — your wallet does not have enough MOR to stake for this session length. Original: execution reverted: ERC20: transfer amount exceeds balance"},
		{"open_allowance", "failed to send transaction: open session failed: failed to build transaction: execution reverted: ERC20: insufficient allowance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, lr, p, sl := rpcEnv(t)
			lr.setOpenFault(func(string) (string, bool) { return `{"error":"` + tc.body + `"}`, false })
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			if !errors.Is(err, ErrWalletNeedsFunds) {
				t.Fatalf("err = %v, want ErrWalletNeedsFunds", err)
			}
			var na *NoAvailableError
			if errors.As(err, &na) {
				t.Fatalf("err = %+v, want no refusal with a Retry-After", na)
			}
			if n := len(lr.bidsTried()); n != 1 {
				t.Fatalf("open POSTs = %d, want 1 (no retry, no bid walk)", n)
			}
			if got := sl.sleeps(); len(got) != 0 {
				t.Fatalf("backoff sleeps %v, want none", got)
			}
			if p.IsCooling(modelB, leaseProv1) || p.IsCooling(modelB, leaseProv2) {
				t.Fatal("a provider was cooled")
			}
			p.mu.Lock()
			pending := len(p.pending)
			p.mu.Unlock()
			if pending != 0 {
				t.Fatalf("%d open attempts pending, want 0", pending)
			}
		})
	}
	// Past the 18 s rule the open stage may have been sent: reconciled,
	// not a funding refusal.
	t.Run("open_late_is_maybe_sent", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		lr.set(func(lr *leaseRouter) { lr.openTake = 25 * time.Second })
		lr.setOpenFault(func(string) (string, bool) {
			return `{"error":"failed to send transaction: open session failed: insufficient ETH for gas — add ETH to your wallet to cover transaction fees. Original: insufficient funds for gas * price + value"}`, false
		})
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		if errors.Is(err, ErrWalletNeedsFunds) {
			t.Fatal("a late open-stage funding error failed fast")
		}
		assertTransient(t, err, time.Second, transientCap)
	})
}
