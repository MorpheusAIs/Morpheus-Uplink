package pool

// The wallet slot: one wallet write at a time, FIFO,
// bounded waits and holds, cut opens and closes on a router that keeps
// going after a disconnect, and which paths may close.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// orphans puts n old unheld sessions of model on chain (opened 20 min ago).
func orphans(c *fakeClock, lr *leaseRouter, model string, n int) []string {
	var ids []string
	for i := range n {
		id := fmt.Sprintf("0xorph%02d%s", i, model[2:6])
		lr.addChain(id, model, c.Now().Add(-20*time.Minute), c.Now().Add(time.Hour))
		ids = append(ids, id)
	}
	return ids
}

func TestOneOpenInFlightPerWallet(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	orphans(c, lr, modelA, 6) // one is adopted, five are duplicates the sweep closes
	// Real time on each write widens every overlap window.
	lr.set(func(lr *leaseRouter) { lr.closeHook = func(string) { time.Sleep(2 * time.Millisecond) } })
	lr.setOpenFault(func(string) (string, bool) { time.Sleep(2 * time.Millisecond); return "", false })
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i, m := range eightModels() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("k%d", i)})
			if err != nil {
				errs <- err
				return
			}
			l.Done()
		}()
	}
	stop := make(chan struct{})
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		for {
			select {
			case <-stop:
				return
			default:
				p.ScanOrphans(context.Background())
			}
		}
	}()
	within(t, 10*time.Second, "8 parallel opens", wg.Wait)
	waitUntil(t, "backlog closed", func() bool { return len(lr.closesSent()) >= 5 })
	close(stop)
	<-swept
	close(errs)
	for err := range errs {
		t.Errorf("acquire: %v", err)
	}
	writes, closes := lr.writePeaks()
	if writes > 1 || closes > 1 {
		t.Fatalf("wallet writes in flight peaked at %d (closes %d), want ≤ 1", writes, closes)
	}
	if n := p.slot.maxHeld.Load(); n > 1 {
		t.Fatalf("slot held %d times at once", n)
	}
	assertNoOrphans(t, lr, p)
}

func TestSlotFIFO(t *testing.T) {
	s := &walletSlot{}
	ctx := context.Background()
	hold, _ := s.acquire(ctx, "")
	order := make(chan int, 5)
	for i := range 5 {
		go func() {
			tok, err := s.acquire(ctx, fmt.Sprintf("k%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			order <- i
			s.release(tok)
		}()
		waitUntil(t, "waiter queued", func() bool { return s.queued() == i+1 })
	}
	s.release(hold)
	for want := range 5 {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("served %d, want %d (arrival order)", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("waiter not served")
		}
	}
}

func TestSlotPerKeyCap(t *testing.T) {
	t.Run("slot", func(t *testing.T) {
		s := &walletSlot{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hold, _ := s.acquire(ctx, "")
		defer s.release(hold)
		for range maxQueuedPerKey {
			go func() { _, _ = s.acquire(ctx, "k1") }()
		}
		waitUntil(t, "two queued", func() bool { return s.queued() == maxQueuedPerKey })
		start := time.Now()
		if _, err := s.acquire(ctx, "k1"); !errors.Is(err, errSlotBusy) {
			t.Fatalf("third k1 waiter: %v, want errSlotBusy", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("the per-key cap did not answer at once")
		}
		go func() { _, _ = s.acquire(ctx, "k2") }()
		waitUntil(t, "k2 queued", func() bool { return s.queued() == maxQueuedPerKey+1 })
		go func() { _, _ = s.acquire(ctx, "") }() // background closes are exempt
		waitUntil(t, "unkeyed queued", func() bool { return s.queued() == maxQueuedPerKey+2 })
	})
	t.Run("acquire", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		hold := p.slot.tryAcquire("")
		var wg sync.WaitGroup
		for _, m := range []string{modelB, modelC} {
			wg.Add(1)
			go func() { defer wg.Done(); _, _, _ = p.Acquire(m, AcquireOpts{KeyID: "k1"}) }()
		}
		waitUntil(t, "two k1 opens queued", func() bool { return p.slot.queued() == 2 })
		_, _, err := p.Acquire(modelD, AcquireOpts{KeyID: "k1"})
		assertOpenBusy(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.Acquire(extraModels[0], AcquireOpts{KeyID: "k2"}); err != nil {
				t.Errorf("k2: %v", err)
			}
		}()
		waitUntil(t, "k2 queued", func() bool { return p.slot.queued() == 3 })
		p.slot.release(hold)
		within(t, 5*time.Second, "queued opens", wg.Wait)
		if n := len(lr.bidsTried()); n != 3 {
			t.Fatalf("open POSTs = %d, want 3 (the capped one sends none)", n)
		}
	})
}

func TestSlotWaitBoundedByContext(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		hold := p.slot.tryAcquire("")
		defer p.slot.release(hold)
		ctx, cancel := context.WithCancel(context.Background())
		res := make(chan error, 1)
		go func() { _, _, err := p.AcquireCtx(ctx, modelB, AcquireOpts{}); res <- err }()
		waitUntil(t, "queued", func() bool { return p.slot.queued() == 1 })
		cancel()
		select {
		case err := <-res:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancel did not end the wait")
		}
		if p.slot.queued() != 0 || len(lr.bidsTried()) != 0 {
			t.Fatal("waiter left queued or an open was sent")
		}
	})
	t.Run("90s cap", func(t *testing.T) {
		scaleSlot(t, 1000) // 90 s queue cap = 90 ms
		_, lr, p, _ := rpcEnv(t)
		hold := p.slot.tryAcquire("")
		defer p.slot.release(hold)
		start := time.Now()
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertOpenBusy(t, err) // no Retry-After
		if el := time.Since(start); el < realDur(openSlotWait) || el > realDur(openSlotWait)+2*time.Second {
			t.Fatalf("waited %v, want about %v", el, realDur(openSlotWait))
		}
		if len(lr.bidsTried()) != 0 {
			t.Fatal("open sent")
		}
	})
}

func TestTarpitOpenBoundedByHoldCap(t *testing.T) {
	scaleSlot(t, 100) // 60 s hold = 600 ms
	_, lr, p, _ := rpcEnv(t)
	lr.set(func(lr *leaseRouter) { lr.openHang, lr.mintDelay = true, 70*time.Second })
	first := make(chan error, 1)
	var held time.Duration
	go func() {
		start := time.Now()
		_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
		held = time.Since(start)
		first <- err
	}()
	waitUntil(t, "open sent", func() bool { return len(lr.bidsTried()) == 1 })
	second := make(chan error, 1)
	go func() { _, _, err := p.Acquire(modelC, AcquireOpts{KeyID: "k2"}); second <- err }()
	waitUntil(t, "second waiter queued", func() bool { return p.slot.queued() == 1 })
	err := <-first
	assertTransient(t, err, 4*time.Second, 4*time.Second)
	cut := realDur(slotHoldCap - reconcileReserve)
	if held < cut*9/10 || held > realDur(slotHoldCap)+250*time.Millisecond {
		t.Fatalf("hold %v: want the POST cut at about %v and the hold ≤ %v", held, cut, realDur(slotHoldCap))
	}
	<-second // the next waiter (another model) may open once the slot frees
	if n := len(lr.bidsTried()); n < 1 || n > 2 {
		t.Fatalf("open POSTs = %d, want the cut one plus at most the next waiter's", n)
	}
	time.Sleep(realDur(slotHoldCap + watchdogSlack))
	if n := p.slot.watchdogFired.Load(); n != 0 {
		t.Fatalf("watchdog fired %d times", n)
	}
}

// cutOpen runs one Acquire for model whose open the mock tarpits; the
// session mints mint later. It returns once the POST is cut.
func cutOpen(t *testing.T, lr *leaseRouter, p *Pool, model string, mint time.Duration) {
	t.Helper()
	scaleSlot(t, 100)
	lr.set(func(lr *leaseRouter) { lr.openHang, lr.mintDelay = true, mint })
	_, _, err := p.Acquire(model, AcquireOpts{})
	assertTransient(t, err, 4*time.Second, 4*time.Second)
	lr.set(func(lr *leaseRouter) { lr.openHang = false })
}

func TestCutOpenMinesLateMatchedNoWarn(t *testing.T) {
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	cutOpen(t, lr, p, modelB, 6*time.Minute)
	for range 3 { // sweeps before it mints: nothing to see
		c.Advance(orphanScanEvery)
		p.ScanOrphans(context.Background())
	}
	c.Advance(time.Minute)
	p.ScanOrphans(context.Background())
	if ids := pooledIDs(p); len(ids) != 1 {
		t.Fatalf("pooled %v, want the late session adopted", ids)
	}
	if n := logs.count("wallet_shared_suspected"); n != 0 {
		t.Fatalf("shared-wallet warning fired %d times for our own late open", n)
	}
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if res.Opened || len(lr.bidsTried()) != 1 {
		t.Fatalf("result %+v after %d POSTs, want the late session leased", res, len(lr.bidsTried()))
	}
	assertNoOrphans(t, lr, p)
}

func TestCloseTimeoutKeepsMark(t *testing.T) {
	scaleSlot(t, 100) // 30 s close call = 300 ms
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	dup := orphans(c, lr, modelB, 1)[0]
	lr.set(func(lr *leaseRouter) { lr.closeHang, lr.closeDelay = true, 3*time.Minute })
	p.ScanOrphans(context.Background())
	if got := lr.closesSent(); len(got) != 1 || got[0] != dup {
		t.Fatalf("closes %v, want the duplicate %s", got, dup)
	}
	p.mu.Lock()
	exp := p.closingIDs[strings.ToLower(dup)]
	p.mu.Unlock()
	if exp.IsZero() {
		t.Fatal("cut close left no expiring mark")
	}
	// Our pooled session goes away: the marked one would now be the
	// model's only adoptable session.
	lr.mu.Lock()
	lr.sessions[res.SessionID].closed = true
	lr.mu.Unlock()
	p.notifyClosed(res.SessionID)
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	for _, id := range pooledIDs(p) {
		if id == dup {
			t.Fatal("status poll adopted a session whose close was cut")
		}
	}
	lr.set(func(lr *leaseRouter) { lr.closeHang = false })
	_, res2 := mustAcquire(t, p, modelB, AcquireOpts{})
	if res2.SessionID == dup {
		t.Fatal("Acquire leased a session whose close was cut")
	}
	c.Advance(3*time.Minute + time.Second)
	p.ScanOrphans(context.Background())
	p.mu.Lock()
	_, marked := p.closingIDs[strings.ToLower(dup)]
	p.mu.Unlock()
	if marked {
		t.Fatal("mark not cleared once the list showed the session closed")
	}
	if logs.count("closed after its close call was cut") != 1 {
		t.Fatal("late close not logged")
	}
}

func TestOpenNeedsMinPostTime(t *testing.T) {
	for _, tc := range []struct {
		left  time.Duration
		posts int
	}{{30 * time.Second, 0}, {40 * time.Second, 1}} {
		t.Run(tc.left.String(), func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			lr.set(func(lr *leaseRouter) {
				lr.ratedFault = func(string) string { c.Advance(slotHoldCap - tc.left); return "" }
			})
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			if n := len(lr.bidsTried()); n != tc.posts {
				t.Fatalf("open POSTs = %d, want %d", n, tc.posts)
			}
			if tc.posts == 0 {
				assertTransient(t, err, 4*time.Second, 4*time.Second)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCutCloseDoesNotBlockOpens(t *testing.T) {
	scaleSlot(t, 100) // the 30 s failover close = 300 ms
	c, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	lr.set(func(lr *leaseRouter) { lr.closeHang, lr.closeDelay = true, 2*time.Minute })
	fr, err := p.Failover(modelB, res.SessionID, FailoverOpts{Reopen: true})
	if err != nil {
		t.Fatalf("failover: %v (a cut close must not block the reopen)", err)
	}
	if !fr.Opened || fr.SessionID == res.SessionID {
		t.Fatalf("failover %+v, want a reopen", fr)
	}
	lr.set(func(lr *leaseRouter) { lr.closeHang = false })
	_, _, err = p.Acquire(modelC, AcquireOpts{})
	if err != nil {
		t.Fatalf("open after a cut close: %v", err)
	}
	p.mu.Lock()
	exp := p.closingIDs[strings.ToLower(res.SessionID)]
	p.mu.Unlock()
	if exp.IsZero() {
		t.Fatal("cut close mark not held")
	}
	c.Advance(2*time.Minute + time.Second)
	p.ScanOrphans(context.Background())
	p.mu.Lock()
	_, marked := p.closingIDs[strings.ToLower(res.SessionID)]
	p.mu.Unlock()
	if marked {
		t.Fatal("mark not cleared after the late close")
	}
	assertNoOrphans(t, lr, p)
}

func TestEscalationExtraOpenTxReverts(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		lr.set(func(lr *leaseRouter) { lr.extraMint = func(string) bool { return false } }) // the extra tx reverts
		_, res := mustAcquire(t, p, modelB, AcquireOpts{})
		if !res.Opened || len(lr.openSet()) != 1 {
			t.Fatalf("result %+v, on chain %v: want one session", res, lr.openSet())
		}
		c.Advance(time.Minute)
		p.ScanOrphans(context.Background())
		ageAll(c)
		p.ScanOrphans(context.Background())
		if n := logs.count("wallet_shared_suspected"); n != 0 {
			t.Fatalf("shared-wallet warning fired %d times", n)
		}
		assertNoOrphans(t, lr, p)
	})
	t.Run("reverted receipt first", func(t *testing.T) {
		logs := captureLogs(t)
		c, lr, p, _ := rpcEnv(t)
		lr.set(func(lr *leaseRouter) { lr.hideLanded = 1 })
		const reverted = "failed to send transaction: parse session receipt failed: Transaction failed with status 0" // v7.18.0 text
		lr.setOpenFault(faultBurst(1, routerErr(reverted), true))
		if got := classifyOpenErr(status500(reverted), time.Second, true); got != maybeSent {
			t.Fatalf("reverted receipt: %s, want maybe_sent", got)
		}
		_, res := mustAcquire(t, p, modelB, AcquireOpts{})
		if res.SessionID == "" || len(lr.bidsTried()) != 1 || len(lr.openSet()) != 1 {
			t.Fatalf("result %+v, %d POSTs, on chain %v: want the one real session matched", res, len(lr.bidsTried()), lr.openSet())
		}
		c.Advance(time.Minute)
		p.ScanOrphans(context.Background())
		if n := logs.count("wallet_shared_suspected"); n != 0 {
			t.Fatalf("shared-wallet warning fired %d times", n)
		}
		assertNoOrphans(t, lr, p)
	})
}

func TestSameBidExtraFromSecondCallIsSwept(t *testing.T) {
	t.Run("matched to a pending attempt", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		// A fast P* (pending, maybe late) then success on the same bid.
		lr.setOpenFault(faultBurst(1, rl429(stageBuild), false))
		_, res := mustAcquire(t, p, modelB, AcquireOpts{})
		// The first call's session lands afterwards.
		lr.addChainOn("0xextra1", modelB, leaseProv1, c.Now().Add(time.Second), c.Now().Add(10*time.Minute))
		c.Advance(5 * time.Second)
		p.ScanOrphans(context.Background()) // known duplicate: closed now, no pruneGrace wait
		if got := lr.closesSent(); len(got) != 1 || got[0] != "0xextra1" {
			t.Fatalf("closes %v, want the extra session", got)
		}
		if got := pooledIDs(p); len(got) != 1 || got[0] != res.SessionID {
			t.Fatalf("pooled %v, want %s", got, res.SessionID)
		}
	})
	t.Run("no attempt", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		mustAcquire(t, p, modelB, AcquireOpts{})
		lr.addChainOn("0xextra2", modelB, leaseProv1, c.Now(), c.Now().Add(10*time.Minute))
		c.Advance(time.Minute)
		p.ScanOrphans(context.Background())
		if len(lr.closesSent()) != 0 {
			t.Fatal("young session closed before pruneGrace")
		}
		ageAll(c)
		p.ScanOrphans(context.Background())
		if got := lr.closesSent(); len(got) != 1 || got[0] != "0xextra2" {
			t.Fatalf("closes %v, want the extra session after pruneGrace", got)
		}
	})
}

func TestClientCancelMidOpen(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	gate := make(chan struct{})
	arr := make(chan string, 1)
	lr.set(func(lr *leaseRouter) { lr.gate, lr.arrivals = gate, arr })
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, _, err := p.AcquireCtx(ctx, modelB, AcquireOpts{}); res <- err }()
	<-arr
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(gate)
	err := <-res
	if err != nil {
		var na *NoAvailableError
		if errors.As(err, &na) {
			t.Fatalf("cancel mid-open gave a refusal %v", err)
		}
	}
	lr.mu.Lock()
	cancels := lr.gateCancels
	lr.mu.Unlock()
	if cancels != 0 {
		t.Fatal("the open POST was cancelled with the client")
	}
	if got := pooledIDs(p); len(got) != 1 {
		t.Fatalf("pooled %v, want the detached open pooled", got)
	}
	assertNoOrphans(t, lr, p)
}

func TestCancelBeforeSend(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	lr.set(func(lr *leaseRouter) {
		lr.ratedFault = func(string) string { arrived <- struct{}{}; <-release; return "" }
	})
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, _, err := p.AcquireCtx(ctx, modelB, AcquireOpts{}); res <- err }()
	<-arrived
	cancel()
	err := <-res
	close(release)
	if err == nil {
		t.Fatal("open went ahead after cancel")
	}
	if n := len(lr.bidsTried()); n != 0 {
		t.Fatalf("open POSTs = %d, want 0", n)
	}
	tok := p.slot.tryAcquire("")
	if tok == nil {
		t.Fatal("slot not released")
	}
	p.slot.release(tok)
}

func TestAllSlotPathsParallel(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	orphans(c, lr, modelD, 3)
	lr.addChain("0xexpired", modelC, c.Now().Add(-time.Hour), c.Now().Add(-time.Minute))
	la, ra := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "ka"})
	lb, rb := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "kb"})
	// A retired session with one lease left (its last Done is a path).
	p.mu.Lock()
	p.sess[rb.SessionID].retired = true
	delete(p.entries, normModel(modelB))
	p.mu.Unlock()
	within(t, 10*time.Second, "every slot path at once", func() {
		var wg sync.WaitGroup
		run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
		for _, m := range eightModels() {
			run(func() {
				if l, _, err := p.Acquire(m, AcquireOpts{KeyID: "x" + m[2:6]}); err == nil {
					l.Done()
				}
			})
		}
		run(func() { _, _ = p.Failover(modelA, ra.SessionID, FailoverOpts{Reopen: true}) })
		run(func() { _ = p.CloseSession("0xorph00dddd") })
		run(func() { p.CloseAll() })
		run(func() { p.SweepIdle(0) })
		run(func() { p.ScanOrphans(context.Background()) })
		run(func() { lb.Done() })
		run(func() { la.Done(); p.Release(modelA, "ka") })
		run(func() { p.CloseAllWithin(time.Now().Add(5*time.Second), time.Second) })
		run(func() {
			p.Gate()
			p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{})
		})
		run(func() { _ = p.Rehydrate(); _ = p.RehydrateForce() })
		wg.Wait()
	})
	if n := p.slot.maxHeld.Load(); n > 1 {
		t.Fatalf("slot held %d times at once", n)
	}
	writes, _ := lr.writePeaks()
	if writes > 1 {
		t.Fatalf("wallet writes in flight peaked at %d", writes)
	}
	if n := p.slot.watchdogFired.Load(); n != 0 {
		t.Fatalf("watchdog fired %d times", n)
	}
}

// dueBacklog: duplicates of modelD on chain, one adoptable, the rest
// close-due.
func dueBacklog(c *fakeClock, lr *leaseRouter, n int) []string { return orphans(c, lr, modelD, n+1) }

func TestForcedScanWithoutTokenNeverCloses(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 3)
	lr.addChain("0xexpired", modelC, c.Now().Add(-time.Hour), c.Now().Add(-time.Minute))
	for range 3 {
		if err := p.RehydrateForce(); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("forced scan sent %d closes", n)
	}
}

func TestStatusScanNeverCloses(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 3)
	for range 3 {
		_ = p.Rehydrate()
		c.Advance(time.Minute)
	}
	_ = p.ChainOpen()
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("status scan sent %d closes", n)
	}
}

func TestPreOpenScanNeverCloses(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 3)
	mustAcquire(t, p, modelB, AcquireOpts{})
	mustAcquire(t, p, modelD, AcquireOpts{}) // adopts one, leaves the rest
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("pre-open scan sent %d closes", n)
	}
}

func TestClosePhaseRequiresToken(t *testing.T) {
	_, _, p, _ := rpcEnv(t)
	mustPanic := func(what string, tok *slotToken) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s: close without the slot did not panic", what)
			}
		}()
		_, _ = p.closeWithToken(context.Background(), tok, "0xs999", "", "", 0)
	}
	mustPanic("nil token", nil)
	tok := p.slot.tryAcquire("")
	p.slot.release(tok)
	mustPanic("released token", tok)
	other := &slotToken{id: 999}
	held := p.slot.tryAcquire("")
	defer p.slot.release(held)
	mustPanic("foreign token", other)
}

func TestOrphanBacklogDelaysOpenByAtMostOneClose(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 10)
	var once sync.Once
	opened := make(chan error, 1)
	closesAtOpen := -1
	lr.set(func(lr *leaseRouter) {
		lr.closeTake = 25 * time.Second
		lr.closeHook = func(string) {
			once.Do(func() {
				go func() { _, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k"}); opened <- err }()
				waitUntil(t, "open queued mid-sweep", func() bool { return p.slot.queued() == 1 })
			})
		}
	})
	lr.setOpenFault(func(string) (string, bool) {
		closesAtOpen = len(lr.closesSent())
		return "", false
	})
	p.ScanOrphans(context.Background())
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	if closesAtOpen != 1 {
		t.Fatalf("closes before the open = %d, want ≤ 1 (the one in flight)", closesAtOpen)
	}
}

func TestClosesNeverCauseOpenBusy(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 10)
	lr.set(func(lr *leaseRouter) { lr.closeTake = 25 * time.Second })
	stop := make(chan struct{})
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		for {
			select {
			case <-stop:
				return
			default:
				p.ScanOrphans(context.Background())
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i, m := range eightModels() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("k%d", i)}); err != nil {
				errs <- err
			}
		}()
	}
	within(t, 10*time.Second, "8 opens beside the backlog", wg.Wait)
	close(stop)
	<-swept
	close(errs)
	for err := range errs {
		t.Errorf("open refused beside closes: %v", err)
	}
}

func TestBootReconcileVsAcquireNoDeadlock(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	for i := range 3 {
		lr.addChain(fmt.Sprintf("0xexp%d", i), modelA, c.Now().Add(-time.Hour), c.Now().Add(-time.Minute))
	}
	lr.set(func(lr *leaseRouter) { lr.closeTake = 5 * time.Second })
	p.Gate()
	within(t, 10*time.Second, "boot reconcile and 4 waiting opens", func() {
		var wg sync.WaitGroup
		for _, m := range []string{modelA, modelB, modelC, modelD} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := p.Acquire(m, AcquireOpts{}); err != nil {
					t.Errorf("acquire %s: %v", m[:6], err)
				}
			}()
		}
		p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{})
		wg.Wait()
		for range 5 {
			p.ScanOrphans(context.Background())
		}
	})
	for i := range 3 {
		if lr.closedCount(fmt.Sprintf("0xexp%d", i)) != 1 {
			t.Fatalf("boot close %d not done: %v", i, lr.closesSent())
		}
	}
}

func TestNoRescanNestedInSlot(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, 1)
	lists := lr.countLists()
	atClose := -1
	lr.set(func(lr *leaseRouter) { lr.closeHook = func(string) { atClose = lists() } })
	p.ScanOrphans(context.Background())
	if atClose < 0 {
		t.Fatal("no close")
	}
	if n := lists() - atClose; n != 0 {
		t.Fatalf("%d list calls during the close, want 0", n)
	}
}

func TestReleaseNeverClosesOnRequestPath(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	if out := p.Release(modelB, "k1"); out.Closed || out.Reason != "closing" || !out.Released {
		t.Fatalf("release %+v, want released, closed:false, reason closing", out)
	}
	// A retired session's last lease.
	l2, res2 := mustAcquire(t, p, modelC, AcquireOpts{KeyID: "k2"})
	p.mu.Lock()
	p.sess[res2.SessionID].retired = true
	p.mu.Unlock()
	l2.Done()
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("request path sent %d closes", n)
	}
	p.sweepRetired(context.Background(), 0, false)
	p.sweepRetired(context.Background(), 0, false)
	if lr.closedCount(res.SessionID) != 1 || lr.closedCount(res2.SessionID) != 1 {
		t.Fatalf("sweep closes %v, want both released sessions", lr.closesSent())
	}
}

func TestCloseBoundedReturnsOnlyAfterCall(t *testing.T) {
	scaleSlot(t, 100)
	c, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{})
	l.Done()
	lr.set(func(lr *leaseRouter) { lr.closeHang, lr.closeDelay = true, time.Minute })
	runtime.GC()
	before := runtime.NumGoroutine()
	start := time.Now()
	if err := p.closeBounded(res.SessionID); err == nil {
		t.Fatal("cut close reported success")
	}
	el := time.Since(start)
	if el < realDur(FailoverCloseTimeout)*9/10 || el > realDur(FailoverCloseTimeout)+300*time.Millisecond {
		t.Fatalf("closeBounded returned after %v, want when the %v call ends", el, realDur(FailoverCloseTimeout))
	}
	if tok := p.slot.tryAcquire(""); tok == nil {
		t.Fatal("slot still held")
	} else {
		p.slot.release(tok)
	}
	waitUntil(t, "no close goroutine left", func() bool { return runtime.NumGoroutine() <= before+2 })

	lr.mu.Lock()
	lr.sessions[res.SessionID].closeAt = c.Now().Unix()
	lr.mu.Unlock()
	_, _ = mustAcquire(t, p, modelC, AcquireOpts{})
	start = time.Now()
	r := p.CloseAllWithin(time.Now().Add(2*time.Second), time.Minute)
	if el := time.Since(start); el > realDur(closeCallTimeout)*2+500*time.Millisecond {
		t.Fatalf("CloseAllWithin took %v", el)
	}
	if r.Closed != 0 {
		t.Fatalf("result %+v: a hung close counted as closed", r)
	}
	waitUntil(t, "no close goroutine left", func() bool { return runtime.NumGoroutine() <= before+2 })
}

func TestSweepNotStarvedUnderSteadyOpens(t *testing.T) {
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	dueBacklog(c, lr, orphanAlertCount)
	// The first tick records the backlog but closes nothing (a cut close's
	// mark); the mark then clears.
	release := holdCloses(c, lr, p)
	p.ScanOrphans(context.Background())
	release()
	if len(lr.closesSent()) != 0 {
		t.Fatal("closed during the window")
	}
	hold := p.slot.tryAcquire("open") // opens keep the slot busy
	for range sweepMaxSkips - 1 {
		p.ScanOrphans(context.Background())
	}
	if len(lr.closesSent()) != 0 {
		t.Fatal("closed without the slot")
	}
	done := make(chan struct{})
	go func() { p.ScanOrphans(context.Background()); close(done) }()
	waitUntil(t, "sweep queued on the third skip", func() bool { return p.slot.queued() == 1 })
	p.slot.release(hold)
	<-done
	if n := len(lr.closesSent()); n != 1 {
		t.Fatalf("closes %d, want 1 per queued hold", n)
	}
	if n := logs.count("orphan_close_starved"); n != 1 {
		t.Fatalf("orphan_close_starved logged %d times, want 1 per window", n)
	}
	c.Advance(sharedWalletWarnEvery)
	hold = p.slot.tryAcquire("open")
	go func() {
		waitUntil(t, "sweep queued", func() bool { return p.slot.queued() == 1 })
		p.slot.release(hold)
	}()
	p.ScanOrphans(context.Background())
	if n := logs.count("orphan_close_starved"); n != 2 {
		t.Fatalf("orphan_close_starved logged %d times after the window, want 2", n)
	}
}

// A failover's slot waits follow the request context: a client that goes
// away while opens hold the slot stops waiting, and nothing is closed or
// opened for it.
func TestFailoverSlotWaitFollowsRequestContext(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	threeBidders(lr, modelB)
	l, res := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	opens := len(lr.bidsTried())
	hold := p.slot.tryAcquire("open")
	defer p.slot.release(hold)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var err error
	within(t, 2*time.Second, "failover with a cancelled request", func() {
		_, err = p.FailoverCtx(ctx, modelB, res.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	})
	// The guardrail read could not run: the session is kept (no
	// alternate known), with the context's error in the message.
	if !errors.Is(err, ErrNoAlternate) || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("err = %v, want no alternate (context deadline exceeded)", err)
	}
	if n := len(lr.closesSent()); n != 0 {
		t.Fatalf("%d closes sent for a request that went away", n)
	}
	if n := len(lr.bidsTried()) - opens; n != 0 {
		t.Fatalf("%d opens sent for a request that went away", n)
	}
}
