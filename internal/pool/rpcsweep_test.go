package pool

// The sweep, closes and shared-wallet warnings.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

func pooledHas(p *Pool, id string) bool {
	for _, x := range pooledIDs(p) {
		if x == id {
			return true
		}
	}
	return false
}

func TestYoungUnheldNotAdoptedBySweep(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		name := map[bool]string{false: "adopted", true: "closed"}[pooled]
		t.Run(name, func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			if pooled {
				l, _ := mustAcquire(t, p, modelB, AcquireOpts{})
				l.Done()
				ageAll(c)
			}
			lr.addChain("0xyoung", modelB, c.Now().Add(-60*time.Second), c.Now().Add(time.Hour))
			// Every scan: the sweep, a status poll, a forced scan, a pre-open scan.
			p.ScanOrphans(context.Background())
			_ = p.RehydrateForce()
			mustAcquire(t, p, modelC, AcquireOpts{})
			if pooledHas(p, "0xyoung") || len(lr.closesSent()) != 0 {
				t.Fatalf("young unheld session touched at 60 s: pooled %v closes %v", pooledIDs(p), lr.closesSent())
			}
			c.Advance(61 * time.Second) // 121 s old
			p.ScanOrphans(context.Background())
			if pooled {
				if lr.closedCount("0xyoung") != 1 {
					t.Fatalf("closes %v, want the extra session closed after pruneGrace", lr.closesSent())
				}
			} else if !pooledHas(p, "0xyoung") {
				t.Fatalf("pooled %v, want the session adopted after pruneGrace", pooledIDs(p))
			}
		})
	}
}

func TestYoungSessionAdoptedOnlyByMatchingAttempt(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	a := p.addAttempt(modelB, bidID(modelB, leaseProv1))
	lr.addChainOn("0xother", modelB, leaseProv2, c.Now().Add(5*time.Second), c.Now().Add(time.Hour))
	c.Advance(10 * time.Second)
	p.ScanOrphans(context.Background())
	if len(pooledIDs(p)) != 0 {
		t.Fatalf("a young session on another bid was adopted: %v", pooledIDs(p))
	}
	lr.addChainOn("0xmine", modelB, leaseProv1, c.Now(), c.Now().Add(time.Hour))
	p.ScanOrphans(context.Background())
	if !pooledHas(p, "0xmine") {
		t.Fatalf("pooled %v, want the session matching the attempt", pooledIDs(p))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.pending {
		if x == a {
			t.Fatal("matched attempt still pending")
		}
	}
}

func TestRecordlessOrphanAdoptedOrClosed(t *testing.T) {
	c, lr, p, _ := rpcEnv(t) // a fresh pool: no records of any session
	old := c.Now().Add(-30 * time.Minute)
	lr.addChain("0xa1", modelA, old, c.Now().Add(time.Hour))
	lr.addChain("0xa2", modelA, old.Add(time.Minute), c.Now().Add(2*time.Hour))
	lr.addChainOn("0xb1", modelB, leaseProv2, old, c.Now().Add(time.Hour))
	lr.addChain("0xexp", modelC, old, c.Now().Add(-time.Minute))
	p.MarkCooling(modelB, leaseProv2) // cooling does not stop adoption
	for range 5 {
		p.ScanOrphans(context.Background())
	}
	if !pooledHas(p, "0xa2") || !pooledHas(p, "0xb1") {
		t.Fatalf("pooled %v, want 0xa2 (latest end) and 0xb1", pooledIDs(p))
	}
	if lr.closedCount("0xa1") != 1 || lr.closedCount("0xexp") != 1 {
		t.Fatalf("closes %v, want the duplicate and the expired session", lr.closesSent())
	}
	assertNoOrphans(t, lr, p)
}

func TestAttemptWindowBoundaries(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	a := &attempt{model: normModel(modelB), bid: strings.ToLower(bidID(modelB, leaseProv1)), start: start}
	s := func(opened time.Time) router.Session {
		return router.Session{ModelAgentID: modelB, BidID: strings.ToUpper(bidID(modelB, leaseProv1)[2:]), OpenedAt: opened.Unix()}
	}
	for _, tc := range []struct {
		at   time.Duration
		want bool
	}{
		{-attemptSkew, true}, {-attemptSkew - time.Second, false},
		{0, true}, {attemptKeep + attemptSkew, true}, {attemptKeep + attemptSkew + time.Second, false},
	} {
		ses := s(start.Add(tc.at))
		ses.BidID = "0x" + ses.BidID
		if got := a.matches(ses); got != tc.want {
			t.Errorf("opened at %v: match %v, want %v", tc.at, got, tc.want)
		}
	}
	other := s(start)
	other.BidID = bidID(modelB, leaseProv2)
	if a.matches(other) {
		t.Error("another bid matched")
	}
	// Pruned after attemptKeep + attemptSkew; at most maxPendingAttempts kept.
	c, _, p, _ := rpcEnv(t)
	p.addAttempt(modelB, "0x01")
	c.Advance(attemptKeep + attemptSkew - time.Second)
	p.mu.Lock()
	p.pruneAttemptsLocked(c.Now())
	n := len(p.pending)
	p.mu.Unlock()
	if n != 1 {
		t.Fatal("attempt pruned early")
	}
	c.Advance(time.Second)
	for range maxPendingAttempts + 5 {
		p.addAttempt(modelB, "0x02")
	}
	p.mu.Lock()
	n = len(p.pending)
	p.mu.Unlock()
	if n != maxPendingAttempts {
		t.Fatalf("%d pending attempts, want the cap %d", n, maxPendingAttempts)
	}
}

func TestCloseAlreadyClosedIsSuccess(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(map[bool]string{false: "ClosedAt set", true: "404"}[gone], func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			lr.addChain("0xdone", modelB, c.Now().Add(-time.Hour), c.Now().Add(-time.Minute))
			var evs []ReconcileEvent
			var mu sync.Mutex
			p.SetOnRescanEvent(func(ev ReconcileEvent) { mu.Lock(); evs = append(evs, ev); mu.Unlock() })
			lr.closeFail.Store(true)
			lr.set(func(lr *leaseRouter) {
				lr.closeHook = func(id string) {
					lr.mu.Lock()
					if gone {
						delete(lr.sessions, id)
					} else {
						lr.sessions[id].closed = true
					}
					lr.mu.Unlock()
				}
			})
			p.ScanOrphans(context.Background())
			p.ScanOrphans(context.Background())
			if n := len(lr.closesSent()); n != 1 {
				t.Fatalf("closes %d, want 1 (no retry)", n)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(evs) != 1 || evs[0].Result != "already_closed" {
				t.Fatalf("events %+v, want one already_closed", evs)
			}
		})
	}
}

func TestUnheldRecheckedInsideSlot(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	dup := orphans(c, lr, modelB, 1)[0]
	// A tick records it close-due but cannot close (a cut close's mark).
	release := holdCloses(c, lr, p)
	p.ScanOrphans(context.Background())
	// Before the next tick our session goes and an Acquire leases the old one.
	lr.mu.Lock()
	lr.sessions[res.SessionID].closed = true
	lr.mu.Unlock()
	p.notifyClosed(res.SessionID)
	release()
	_, r2 := mustAcquire(t, p, modelB, AcquireOpts{})
	if r2.SessionID != dup {
		t.Fatalf("leased %s, want %s adopted", r2.SessionID, dup)
	}
	p.ScanOrphans(context.Background())
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("closed %v after it was leased", lr.closesSent())
	}
}

// closeDuring runs one sweep close of a duplicate of modelC while f runs
// inside the close call; the kept session is dropped first so the closing
// one would otherwise be adoptable.
func closeDuring(t *testing.T, f func(c *fakeClock, lr *leaseRouter, p *Pool, closing string)) {
	c, lr, p, _ := rpcEnv(t)
	old := c.Now().Add(-30 * time.Minute)
	lr.addChain("0xkeep", modelC, old, c.Now().Add(2*time.Hour))
	lr.addChain("0xdup", modelC, old, c.Now().Add(time.Hour))
	ran := false
	lr.set(func(lr *leaseRouter) {
		lr.closeHook = func(id string) {
			if ran {
				return
			}
			ran = true
			lr.mu.Lock()
			lr.sessions["0xkeep"].closed = true
			lr.mu.Unlock()
			p.notifyClosed("0xkeep")
			f(c, lr, p, id)
		}
	})
	p.ScanOrphans(context.Background())
	if !ran {
		t.Fatal("no close")
	}
}

func TestPollAdoptDuringClose(t *testing.T) {
	closeDuring(t, func(_ *fakeClock, _ *leaseRouter, p *Pool, closing string) {
		_ = p.RehydrateForce()
		_ = p.Rehydrate()
		if pooledHas(p, closing) {
			t.Fatal("status poll adopted a session being closed")
		}
	})
}

func TestLeaseDuringClose(t *testing.T) {
	got := make(chan AcquireResult, 1)
	closeDuring(t, func(_ *fakeClock, _ *leaseRouter, p *Pool, _ string) {
		go func() {
			_, r, err := p.Acquire(modelC, AcquireOpts{})
			if err != nil {
				t.Error(err)
			}
			got <- r
		}()
		time.Sleep(10 * time.Millisecond)
	})
	if r := <-got; r.SessionID == "0xdup" || r.SessionID == "" {
		t.Fatalf("Acquire during the close got %q", r.SessionID)
	}
}

func TestCloseMarkClearedOnFailure(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	lr.addChain("0xexp", modelB, c.Now().Add(-time.Hour), c.Now().Add(-time.Minute))
	lr.closeFail.Store(true)
	p.ScanOrphans(context.Background())
	p.mu.Lock()
	n := len(p.closingIDs)
	p.mu.Unlock()
	if n != 0 {
		t.Fatal("failed close left its mark")
	}
	lr.closeFail.Store(false)
	p.ScanOrphans(context.Background())
	if lr.closedCount("0xexp") != 2 || len(lr.openSet()) != 0 {
		t.Fatalf("closes %v, want a second try that closes it", lr.closesSent())
	}
}

func TestPickOneTieBreak(t *testing.T) {
	ss := []router.Session{
		{ID: "0xC", EndsAt: 100, OpenedAt: 10},
		{ID: "0xb", EndsAt: 200, OpenedAt: 10},
		{ID: "0xa", EndsAt: 200, OpenedAt: 10},
		{ID: "0xd", EndsAt: 200, OpenedAt: 5},
	}
	if got := pickOne(ss).ID; got != "0xa" {
		t.Fatalf("pickOne = %s, want 0xa (latest end, later open, lowest id)", got)
	}
	ss = append(ss, router.Session{ID: "0xz", EndsAt: 200, OpenedAt: 11})
	if got := pickOne(ss).ID; got != "0xz" {
		t.Fatalf("pickOne = %s, want 0xz (later open)", got)
	}
}

func TestSharedWalletTriggers(t *testing.T) {
	t.Run("a: young session with no attempt", func(t *testing.T) {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		lr.addChain("0xforeign", modelB, c.Now().Add(-30*time.Second), c.Now().Add(time.Hour))
		p.ScanOrphans(context.Background())
		if logs.count("trigger=a") != 1 {
			t.Fatalf("logs: %s", logs)
		}
	})
	t.Run("b: our pooled session closed elsewhere", func(t *testing.T) {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		l, res := mustAcquire(t, p, modelB, AcquireOpts{})
		l.Done()
		ageAll(c)
		lr.mu.Lock()
		lr.sessions[res.SessionID].closed = true
		lr.mu.Unlock()
		p.ScanOrphans(context.Background())
		if logs.count("trigger=b") != 1 {
			t.Fatalf("logs: %s", logs)
		}
	})
	t.Run("c: many foreign adoptions", func(t *testing.T) {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		for _, m := range []string{modelA, modelB, modelC, modelD} {
			lr.addChain("0xf"+m[2:6], m, c.Now().Add(-30*time.Minute), c.Now().Add(time.Hour))
		}
		p.ScanOrphans(context.Background())
		if logs.count("trigger=c") != 1 {
			t.Fatalf("logs: %s", logs)
		}
	})
}

func TestClockSkewWarn(t *testing.T) {
	for _, tc := range []struct {
		ahead time.Duration
		warn  int
	}{{attemptSkew + time.Second, 1}, {attemptSkew - time.Second, 0}} {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		lr.addChain("0xahead", modelB, c.Now().Add(tc.ahead), c.Now().Add(time.Hour))
		p.ScanOrphans(context.Background())
		if n := logs.count("box clock behind chain"); n != tc.warn {
			t.Errorf("chain %v ahead: %d warnings, want %d", tc.ahead, n, tc.warn)
		}
	}
}

func TestSharedWalletWarnRateLimited(t *testing.T) {
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	lr.addChain("0xf1", modelB, c.Now().Add(-10*time.Second), c.Now().Add(time.Hour))
	p.ScanOrphans(context.Background())
	c.Advance(time.Minute)
	lr.addChain("0xf2", modelC, c.Now().Add(-10*time.Second), c.Now().Add(time.Hour))
	p.ScanOrphans(context.Background())
	if n := logs.count("wallet_shared_suspected"); n != 1 {
		t.Fatalf("%d warnings 1 min apart, want 1", n)
	}
	c.Advance(sharedWalletWarnEvery)
	lr.addChain("0xf3", modelD, c.Now().Add(-10*time.Second), c.Now().Add(time.Hour))
	p.ScanOrphans(context.Background())
	if n := logs.count("wallet_shared_suspected"); n != 2 {
		t.Fatalf("%d warnings after the window, want 2", n)
	}
	if logs.count("suppressed=") == 0 {
		t.Fatal("suppressed count not reported")
	}
}

func TestKnownDuplicateClosedBySweepNotRequest(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	l, _ := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	p.addAttempt(modelB, bidID(modelB, leaseProv1))
	lr.addChain("0xdup", modelB, c.Now(), c.Now().Add(time.Hour))
	mustAcquire(t, p, modelC, AcquireOpts{}) // its pre-open scan matches 0xdup
	p.mu.Lock()
	known := p.knownDup["0xdup"]
	p.mu.Unlock()
	if !known || len(lr.closesSent()) != 0 {
		t.Fatalf("known=%v closes=%v: the request path must only mark it", known, lr.closesSent())
	}
	p.ScanOrphans(context.Background()) // young, but a known duplicate
	if lr.closedCount("0xdup") != 1 {
		t.Fatalf("closes %v, want the sweep to close the known duplicate", lr.closesSent())
	}
}

// busyTicks runs up to n sweep ticks (tick is the RunSweep body) while
// opens hold the wallet slot continuously: the slot is held when each
// tick starts and is handed over only if the tick queues for it, then
// taken back. It returns the tick on which done reported true (0: never).
func busyTicks(t *testing.T, p *Pool, n int, tick func(), done func() bool) int {
	t.Helper()
	hold := p.slot.tryAcquire("open")
	if hold == nil {
		t.Fatal("slot not free at the start")
	}
	defer func() {
		if hold != nil {
			p.slot.release(hold)
		}
	}()
	for i := 1; i <= n; i++ {
		fin := make(chan struct{})
		go func() { tick(); close(fin) }()
		select {
		case <-fin:
		case <-waitQueued(p):
			p.slot.release(hold)
			hold = nil
			<-fin
			hold = p.slot.tryAcquire("open")
			if hold == nil {
				t.Fatal("slot still held after the tick")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("tick %d neither returned nor queued", i)
		}
		if done() {
			return i
		}
	}
	return 0
}

// waitQueued is closed once a waiter is queued on the slot.
func waitQueued(p *Pool) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for p.slot.queued() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if p.slot.queued() > 0 {
			close(ch)
		}
	}()
	return ch
}

// No backlog when the slot gets busy: an orphan that appears while opens
// keep the slot busy is still closed within sweepMaxSkips ticks.
func TestOrphanDuringBusySlotClosedWithinThreeTicks(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	ctx := context.Background()
	p.ScanOrphans(ctx) // nothing due: no backlog
	lr.addChain("0xorphan", modelC, c.Now().Add(-30*time.Minute), c.Now().Add(-time.Minute))
	tick := func() { p.sweepRetired(ctx, 0, false); p.ScanOrphans(ctx) }
	if got := busyTicks(t, p, sweepMaxSkips, tick, func() bool { return lr.closedCount("0xorphan") == 1 }); got == 0 {
		t.Fatalf("orphan not closed within %d ticks under continuous slot use (closes %v)", sweepMaxSkips, lr.closesSent())
	}
}

// No backlog when the slot gets busy: a session released under steady
// opens (retired, nothing in flight: unheld) is closed within
// sweepMaxSkips ticks.
func TestReleaseUnderSteadyOpensClosedWithinThreeTicks(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	ctx := context.Background()
	l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	ageAll(c)
	p.ScanOrphans(ctx) // nothing due: no backlog
	tick := func() { p.sweepRetired(ctx, 0, false); p.ScanOrphans(ctx) }
	if out := p.Release(modelB, "k1"); out.Reason != "closing" {
		t.Fatalf("release %+v, want closing", out)
	}
	if got := busyTicks(t, p, sweepMaxSkips, tick, func() bool { return lr.closedCount(res.SessionID) == 1 }); got == 0 {
		t.Fatalf("released session not closed within %d ticks under steady opens (closes %v)", sweepMaxSkips, lr.closesSent())
	}
}

// The retired close is not try-only: under continuous slot use it queues
// after sweepMaxSkips busy tries and closes the released session.
func TestSweepRetiredQueuesAfterBusyTries(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	ctx := context.Background()
	l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	ageAll(c)
	if out := p.Release(modelB, "k1"); out.Reason != "closing" {
		t.Fatalf("release %+v, want closing", out)
	}
	tick := func() { p.sweepRetired(ctx, 0, false) }
	got := busyTicks(t, p, sweepMaxSkips, tick, func() bool { return lr.closedCount(res.SessionID) == 1 })
	if got != sweepMaxSkips {
		t.Fatalf("closed on try %d (closes %v), want try %d", got, lr.closesSent(), sweepMaxSkips)
	}
}

// Continuous slot use with a close-due session already orphanAlertAge old
// (counted without the slot): orphan_close_starved fires and the session
// is closed on that tick, before sweepMaxSkips.
func TestStarvedCloseAlertsAndClosesWithinBound(t *testing.T) {
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	ctx := context.Background()
	l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	ageAll(c)
	if out := p.Release(modelB, "k1"); out.Reason != "closing" {
		t.Fatalf("release %+v, want closing", out)
	}
	c.Advance(orphanAlertAge + time.Second) // e.g. the host slept through the ticks
	tick := func() { p.sweepRetired(ctx, 0, false); p.ScanOrphans(ctx) }
	got := busyTicks(t, p, sweepMaxSkips, tick, func() bool { return lr.closedCount(res.SessionID) == 1 })
	if got != 1 {
		t.Fatalf("closed on tick %d (closes %v), want tick 1", got, lr.closesSent())
	}
	if logs.count("orphan_close_starved") == 0 {
		t.Fatalf("orphan_close_starved not logged:\n%s", logs.String())
	}
}
