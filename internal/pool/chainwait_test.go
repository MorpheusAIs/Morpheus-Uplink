package pool

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// The two router answers seen when the chain was slow to include the
// wallet's transactions (proxy-router v7.18.0, hash redacted).
const (
	txUnderpriced = "failed to send transaction: open session failed: failed to send transaction: replacement transaction underpriced"
	txNotMined    = "failed to send transaction: open session failed: transaction could not be confirmed on-chain — the network may be congested, try again shortly. Original: transaction not mined after all escalation attempts: last tx <hash>"
)

// Both texts stay maybe_sent at any POST time (classification is
// unchanged); chainWaitReason only names the refusal. Anything without
// the open-stage prefix, a transport error or a provider-stage text has
// no reason.
func TestChainWaitReason(t *testing.T) {
	for _, tc := range []struct {
		text, reason string
	}{
		{txUnderpriced, ReasonWalletTxPending},
		{txNotMined, ReasonChainCongested},
		{"failed to send transaction: open session failed: transaction not mined after all escalation attempts: last tx <hash>", ReasonChainCongested},
		{"FAILED TO SEND TRANSACTION: OPEN SESSION FAILED: REPLACEMENT TRANSACTION UNDERPRICED", ReasonWalletTxPending},
	} {
		for _, at := range []time.Duration{1600 * time.Millisecond, 48 * time.Second} {
			for _, windowOK := range []bool{true, false} {
				if got := classifyOpenErr(status500(tc.text), at, windowOK); got != maybeSent {
					t.Errorf("%q after %v (windowOK %v): class %s, want maybe_sent", tc.text, at, windowOK, got)
				}
			}
		}
		if got := chainWaitReason(status500(tc.text)); got != tc.reason {
			t.Errorf("%q: reason %q, want %q", tc.text, got, tc.reason)
		}
	}
	for name, err := range map[string]error{
		"nil":                 nil,
		"no prefix":           status500("replacement transaction underpriced"),
		"no prefix not mined": status500("transaction not mined after all escalation attempts"),
		"other stage":         status500("failed to send transaction: parse session receipt failed: replacement transaction underpriced"),
		"provider stage":      status500("provider healthcheck ping failed: provider request failed: replacement transaction underpriced"),
		"502 with the text":   &router.StatusError{Code: http.StatusBadGateway, Body: routerErr(txUnderpriced)},
		"503 with the text":   &router.StatusError{Code: http.StatusServiceUnavailable, Body: routerErr(txNotMined)},
		"our deadline":        context.DeadlineExceeded,
		"other open text":     status500(escalationNotSent),
		"plain error":         errors.New(txUnderpriced),
	} {
		if got := chainWaitReason(err); got != "" {
			t.Errorf("%s: reason %q, want none", name, got)
		}
	}
}

// The chain-wait refusal's Retry-After is the fixed 30 s, not clamped to
// the router_transient cap (clampTransient would cut it to 4 s).
func TestErrChainWaitRetryAfterNotClamped(t *testing.T) {
	if clampTransient(chainWaitRetryAfter) != transientCap {
		t.Fatalf("clampTransient(30 s) = %v: this test assumes it caps at %v", clampTransient(chainWaitRetryAfter), transientCap)
	}
	for _, r := range []string{ReasonWalletTxPending, ReasonChainCongested} {
		na := refusal(t, errChainWait(r))
		if na.Reason != r || na.RetryAfter != 30*time.Second {
			t.Fatalf("%s: refusal %+v, want Retry-After 30 s", r, na)
		}
		if !errors.Is(errChainWait(r), ErrNoAvailableProvider) {
			t.Fatalf("%s: not ErrNoAvailableProvider", r)
		}
	}
}

// End to end in the pool: a chain-wait open answer is still maybe sent
// (one POST, reconcile polls, the attempt stays pending), the refusal is
// typed with Retry-After 30 s, nothing cools, and LastChainWait shows it
// until the next successful open.
func TestChainWaitRefusal(t *testing.T) {
	for _, tc := range []struct {
		text, reason string
	}{{txUnderpriced, ReasonWalletTxPending}, {txNotMined, ReasonChainCongested}} {
		t.Run(tc.reason, func(t *testing.T) {
			_, lr, p, _ := rpcEnv(t)
			lists := lr.countLists()
			lr.setOpenFault(faultBurst(1, routerErr(tc.text), false))
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			na := refusal(t, err)
			if na.Reason != tc.reason || na.RetryAfter != chainWaitRetryAfter || !errors.Is(err, ErrNoAvailableProvider) {
				t.Fatalf("refusal %+v, want %s with Retry-After 30 s", na, tc.reason)
			}
			if n := len(lr.bidsTried()); n != 1 {
				t.Fatalf("open POSTs = %d, want 1 (maybe sent: never re-opened)", n)
			}
			if n := lists(); n < reconcilePolls || n > 1+reconcilePolls {
				t.Fatalf("lists = %d, want the reconcile polls", n)
			}
			p.mu.Lock()
			pend := len(p.pending)
			stuck := pend == 1 && p.pending[0].stuck
			p.mu.Unlock()
			if pend != 1 || !stuck {
				t.Fatalf("pending %d (stuck %v), want the attempt kept and stuck", pend, stuck)
			}
			assertNoCooling(t, p, modelB)
			if cw, ok := p.LastChainWait(); !ok || cw.Reason != tc.reason {
				t.Fatalf("LastChainWait %+v %v, want %s", cw, ok, tc.reason)
			}
			lr.setOpenFault(nil)
			l, _ := mustAcquire(t, p, modelB, AcquireOpts{})
			l.Done()
			if cw, ok := p.LastChainWait(); ok {
				t.Fatalf("LastChainWait %+v after a successful open, want none", cw)
			}
		})
	}
}

// LastChainWait is shown for chainWaitShowFor, then dropped.
func TestLastChainWaitExpires(t *testing.T) {
	c, _, p, _ := rpcEnv(t)
	p.setChainWait(ReasonChainCongested)
	c.Advance(chainWaitShowFor - time.Second)
	if _, ok := p.LastChainWait(); !ok {
		t.Fatal("dropped before 5 min")
	}
	c.Advance(time.Second)
	if cw, ok := p.LastChainWait(); ok {
		t.Fatalf("still shown after 5 min: %+v", cw)
	}
}

// wallet_tx_pending marks every pending attempt stuck; chain_congested
// marks only its own.
func TestMarkStuck(t *testing.T) {
	_, _, p, _ := rpcEnv(t)
	a1 := p.addAttempt(modelB, bidID(modelB, leaseProv1))
	a2 := p.addAttempt(modelC, bidID(modelC, leaseProv1))
	p.markStuck(a2, ReasonChainCongested)
	if a1.stuck || !a2.stuck {
		t.Fatalf("chain_congested: stuck %v %v, want only its own", a1.stuck, a2.stuck)
	}
	a3 := p.addAttempt(modelD, bidID(modelD, leaseProv1))
	p.markStuck(a3, ReasonWalletTxPending)
	if !a1.stuck || !a2.stuck || !a3.stuck {
		t.Fatalf("wallet_tx_pending: stuck %v %v %v, want all", a1.stuck, a2.stuck, a3.stuck)
	}
}

// The incident: an open is cut (our deadline) and its tx mines about
// 31 min later; meanwhile a later open gets "replacement transaction
// underpriced". The cut attempt is kept for stuckKeep, so the late
// session matches it: adopted as ours, no wallet_shared_suspected, no
// extra open. Control: without the underpriced answer the cut attempt is
// pruned after attemptKeep and the alarm fires as before.
func TestStuckOpenMinesLate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		underpriced bool
		alarms      int
	}{{"wallet_tx_pending keeps it", true, 0}, {"control: alarm as before", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			scaleSlot(t, 200)
			c, lr, p, _ := rpcEnv(t)
			logs := captureLogs(t)
			lr.set(func(lr *leaseRouter) { lr.openHang, lr.mintDelay = true, 31*time.Minute })
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			assertTransient(t, err, 4*time.Second, 4*time.Second) // a cut stays router_transient
			waitUntil(t, "late mint recorded", func() bool { lr.mu.Lock(); defer lr.mu.Unlock(); return lr.n == 1 })
			// Take the minted session off chain until it mines (the mock's
			// existing-session guard does not model visibility).
			lr.mu.Lock()
			id := lr.order[0]
			late := *lr.sessions[id]
			delete(lr.sessions, id)
			lr.order = lr.order[:0]
			lr.openHang = false
			lr.mu.Unlock()
			t0 := c.Now()
			if tc.underpriced {
				c.Advance(2 * time.Minute)
				lr.setOpenFault(faultBurst(1, routerErr(txUnderpriced), false))
				_, _, err = p.Acquire(modelB, AcquireOpts{})
				if na := refusal(t, err); na.Reason != ReasonWalletTxPending || na.RetryAfter != chainWaitRetryAfter {
					t.Fatalf("refusal %+v, want wallet_tx_pending 30 s; logs: %s", *na, logs)
				}
				lr.setOpenFault(nil)
			}
			opens := len(lr.bidsTried())
			// The tx mines about 31 min after the cut open; the next sweep
			// runs within its interval.
			mined := t0.Add(31 * time.Minute)
			c.Advance(mined.Add(85 * time.Second).Sub(c.Now()))
			lr.addChainOn(id, late.model, late.prov, mined, mined.Add(time.Duration(late.durationOpened)*time.Second))
			p.ScanOrphans(context.Background())
			if n := logs.count("wallet_shared_suspected"); n != tc.alarms {
				t.Fatalf("wallet_shared_suspected %d, want %d; logs: %s", n, tc.alarms, logs)
			}
			if tc.underpriced {
				if got := pooledIDs(p); len(got) != 1 {
					t.Fatalf("pooled %v, want the late session adopted", got)
				}
				p.mu.Lock()
				ours := p.opened[id]
				p.mu.Unlock()
				if !ours {
					t.Fatal("late session not recorded as ours")
				}
				if n := len(lr.bidsTried()); n != opens {
					t.Fatalf("open POSTs %d, want %d (no new open)", n, opens)
				}
			}
		})
	}
}

// A stuck attempt is pruned after stuckKeep + attemptSkew, and only
// matches the same model and bid inside [start − skew, start + stuckKeep + skew].
func TestStuckAttemptBounds(t *testing.T) {
	c, _, p, _ := rpcEnv(t)
	bid := bidID(modelB, leaseProv1)
	a := p.addAttempt(modelB, bid)
	p.markStuck(a, ReasonWalletTxPending)
	p.mu.Lock()
	a.start = a.start.Truncate(time.Second) // chain times are whole seconds
	start := a.start
	p.mu.Unlock()
	sess := func(model, b string, at time.Time) router.Session {
		return router.Session{ModelAgentID: model, BidID: b, OpenedAt: at.Unix()}
	}
	p.mu.Lock()
	for _, tc := range []struct {
		s    router.Session
		want bool
	}{
		{sess(modelB, bid, start.Add(-attemptSkew)), true},
		{sess(modelB, bid, start.Add(-attemptSkew-time.Second)), false},
		{sess(modelB, bid, start.Add(stuckKeep+attemptSkew)), true},
		{sess(modelB, bid, start.Add(stuckKeep+attemptSkew+time.Second)), false},
		{sess(modelC, bid, start.Add(time.Minute)), false},
		{sess(modelB, bidID(modelB, leaseProv2), start.Add(time.Minute)), false},
	} {
		if got := a.matches(tc.s); got != tc.want {
			t.Errorf("session opened %v after start (model %s): match %v, want %v",
				time.Unix(tc.s.OpenedAt, 0).Sub(start), tc.s.ModelAgentID[:6], got, tc.want)
		}
	}
	p.mu.Unlock()
	if stuckKeep != time.Hour {
		t.Fatalf("stuckKeep %v, want 1 h", stuckKeep)
	}
	c.Advance(stuckKeep + attemptSkew - time.Second)
	p.mu.Lock()
	p.pruneAttemptsLocked(p.now())
	n := len(p.pending)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("pruned before stuckKeep + skew")
	}
	c.Advance(time.Second)
	p.mu.Lock()
	p.pruneAttemptsLocked(p.now())
	n = len(p.pending)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending %d after stuckKeep + skew, want 0", n)
	}
	// A non-stuck attempt keeps the 13 min + 30 s window.
	b := p.addAttempt(modelB, bid)
	c.Advance(attemptKeep + attemptSkew)
	p.mu.Lock()
	p.pruneAttemptsLocked(p.now())
	gone := len(p.pending) == 0
	p.mu.Unlock()
	if !gone || b.stuck {
		t.Fatal("a non-stuck attempt outlived attemptKeep + skew")
	}
}
