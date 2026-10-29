package pool

// Mirrors of the live release-gate cases on the mock router.
// Each ends with assertNoOrphans: after the sweep, the on-chain open set
// equals the held set.

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// settle advances past orphanScanEvery + pruneGrace (the live orphan check).
func settle(c *fakeClock) { c.Advance(orphanScanEvery + pruneGrace) }

func TestGateSequentialLanes(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	// A provider-side timeout on one lane's first bid: it walks to bid 2.
	lr.setOpenFault(func(bid string) (string, bool) {
		if bid == bidID(modelC, leaseProv1) {
			return routerErr(pingIOTimeout), false
		}
		return "", false
	})
	for i, m := range eightModels() {
		l, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("lane%d", i)})
		if err != nil {
			t.Fatalf("lane %d: %v", i, err)
		}
		l.Done()
	}
	settle(c)
	assertNoOrphans(t, lr, p)
}

func TestGateOneModelFiveConversations(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	n := 0
	lr.set(func(lr *leaseRouter) {
		lr.ratedFault = func(string) string { n++; return map[bool]string{true: rated429}[n == 1] }
	})
	var wg sync.WaitGroup
	ids := make([]string, 5)
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, r, err := p.Acquire(modelB, AcquireOpts{KeyID: fmt.Sprintf("conv%d", i)})
			if err != nil {
				t.Errorf("conversation %d: %v", i, err)
				return
			}
			ids[i] = r.SessionID
			l.Done()
		}()
	}
	within(t, 10*time.Second, "5 conversations", wg.Wait)
	if len(distinct(ids)) != 1 || len(lr.bidsTried()) > 1 {
		t.Fatalf("sessions %v after %d opens, want one session and ≤ 1 open", ids, len(lr.bidsTried()))
	}
	settle(c)
	assertNoOrphans(t, lr, p)
}

func TestGateSixKeysOneModel(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	models := []string{modelB, modelB, modelB, modelB, modelC, modelC}
	lr.setOpenFault(faultBurst(1, rl429(stageBid), false))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("key%d", i)})
			if err != nil {
				t.Errorf("key %d: %v", i, err)
				return
			}
			l.Done()
		}()
	}
	within(t, 10*time.Second, "6 keys", wg.Wait)
	opens := map[string]int{}
	for _, b := range lr.bidsTried() {
		m, _, _ := lr.bidOwner(b)
		opens[m]++
	}
	// One pre-send 429 retry is allowed on top of one open per model.
	if opens[modelB]+opens[modelC] > 3 || len(lr.openSet()) != 2 {
		t.Fatalf("opens %v, on chain %v: want one session per distinct model", opens, lr.openSet())
	}
	settle(c)
	assertNoOrphans(t, lr, p)
}

func TestParallelBurst8Lanes(t *testing.T) {
	logs := captureLogs(t)
	c, lr, p, _ := rpcEnv(t)
	var mu sync.Mutex
	seen := map[string]int{}
	lr.setOpenFault(func(bid string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		seen[bid]++
		if seen[bid] > 1 {
			return "", false
		}
		m, _, _ := lr.bidOwner(bid)
		switch m {
		case modelA, modelB:
			return rl429(stageBid), false // pre-send: same-bid retry
		case modelC:
			return rl429(stageApprove), false
		case modelD:
			return routerErr(receiptWait429), true // landed: reconciled
		}
		return "", false
	})
	rated := map[string]int{}
	lr.set(func(lr *leaseRouter) {
		lr.ratedFault = func(m string) string {
			mu.Lock()
			defer mu.Unlock()
			rated[m]++
			if rated[m] == 1 && m == extraModels[0] {
				return rated429
			}
			return ""
		}
	})
	var wg sync.WaitGroup
	for i, m := range eightModels() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("lane%d", i)})
			if err != nil {
				t.Errorf("lane %d: %v", i, err)
				return
			}
			l.Done()
		}()
	}
	within(t, 10*time.Second, "8 parallel lanes", wg.Wait)
	for _, m := range eightModels() {
		assertNoCooling(t, p, m)
	}
	if w, _ := lr.writePeaks(); w > 1 {
		t.Fatalf("wallet writes in flight peaked at %d", w)
	}
	if n := logs.count("wallet_shared_suspected"); n != 0 {
		t.Fatalf("shared-wallet warning fired %d times", n)
	}
	settle(c)
	assertNoOrphans(t, lr, p)
}
