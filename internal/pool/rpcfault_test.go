package pool

// Router RPC rate-limit tests. When the router's chain RPC is rate
// limited, every router call that reads or writes chain can fail with the
// RPC's 429, which the router wraps in a status 500 answer. These tests
// pin the expected handling; they FAIL on the current code until the fix
// lands:
//
//   - a transient RPC fault on open is retried on the same bid: no walk to
//     other bids (each walk step is one more chain round trip) and no
//     provider cool-down (the provider did nothing wrong);
//   - an open whose tx landed but whose receipt wait failed leaves no
//     open on-chain session that the pool neither holds nor closes;
//   - a transient RPC fault on rated bids is retried before refusing;
//   - opens from one wallet do not overlap (concurrent txs from one
//     wallet race on the nonce: "replacement transaction underpriced");
//     concurrent conversations on a model share one session.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// extraModels are more mock models (with modelA..modelD: eight) for the
// burst test; leaseRouter resolves their bids too.
var extraModels = []string{
	"0xeeee000000000000000000000000000000000000000000000000000000000000",
	"0xffff000000000000000000000000000000000000000000000000000000000000",
	"0x1010000000000000000000000000000000000000000000000000000000000000",
	"0x2020000000000000000000000000000000000000000000000000000000000000",
}

// orphanAge: the router wallet is dedicated to this service, so an
// unheld on-chain session older than this is an orphan (adopted or
// closed); a younger one may be an open still in progress and is left
// alone, as is a held one.
const orphanAge = 2 * time.Minute

// rpcSteps are the router's open steps whose chain call failed under
// rate limiting, as the router words them.
var rpcSteps = map[string]string{
	"bid":       "failed to get bid",
	"provider":  "failed to get provider",
	"gas":       "failed to send transaction: open session failed: failed to get initial gas prices",
	"build":     "failed to send transaction: open session failed: failed to build transaction",
	"allowance": "failed to get current MOR allowance",
}

// rpc429 is the router's status-500 body when step's chain call got the
// RPC's 429 (JSON-RPC error embedded in the message; the id is made up).
func rpc429(step string) string {
	inner, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "error": map[string]any{
		"code":    429,
		"message": "Your app has exceeded its compute units per second capacity. If you have retries enabled, you can safely ignore this message. If not, check out https://docs.alchemy.com/reference/throughput",
	}})
	b, _ := json.Marshal(map[string]string{"error": rpcSteps[step] + ": 429 Too Many Requests: " + string(inner)})
	return string(b)
}

// receiptWait429 is the router's body when the open tx was sent (the
// session is on chain) but waiting for its receipt hit the RPC's 429.
const receiptWait429 = `{"error":"failed to send transaction: parse session receipt failed: failed to wait for confirmations 429 Too Many Requests"}`

// rated429 is a rated-bids answer under rate limiting. Assumed shape: the
// incident log shows this wording for other chain reads, not rated bids.
const rated429 = `{"error":"Failed to call contract: 429 Too Many Requests"}`

// faultBurst returns an openFault that answers the next n by-bid opens
// with body (n < 0: every open) and then opens normally.
func faultBurst(n int, body string, landed bool) func(string) (string, bool) {
	var mu sync.Mutex
	return func(string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if n == 0 {
			return "", false
		}
		if n > 0 {
			n--
		}
		return body, landed
	}
}

func (lr *leaseRouter) setOpenFault(f func(string) (string, bool)) {
	lr.mu.Lock()
	lr.openFault = f
	lr.mu.Unlock()
}

func (lr *leaseRouter) bidsTried() []string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return append([]string(nil), lr.byBid...)
}

func distinct(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// unheld lists the sessions open on chain that the pool neither holds
// (pooled) nor has closed.
func unheld(lr *leaseRouter, p *Pool) []string {
	pooled := map[string]bool{}
	for _, s := range p.Snapshot() {
		pooled[s.SessionID] = true
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var out []string
	for _, id := range lr.order {
		if s := lr.sessions[id]; !s.closed && !pooled[id] {
			out = append(out, fmt.Sprintf("%s (provider %s)", id, s.prov[:6]))
		}
	}
	sort.Strings(out)
	return out
}

// threeBidders gives modelB three healthy providers in rated order.
func threeBidders(lr *leaseRouter) {
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv1, leaseProv2, leaseProv3}
	lr.mu.Unlock()
}

// Failure 1 (open amplification): one RPC 429 on the top bid must not
// walk to the next bids. Expected: the open is retried on the same bid
// and succeeds there.
func TestRPC429OpenStaysOnSameBid(t *testing.T) {
	for step := range rpcSteps {
		t.Run(step, func(t *testing.T) {
			lr, p := newLeaseRouter(t, newFakeClock())
			threeBidders(lr)
			lr.setOpenFault(faultBurst(1, rpc429(step), false))
			_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
			tried := lr.bidsTried()
			top := bidID(modelB, leaseProv1)
			for i, id := range tried {
				if id != top {
					t.Errorf("open %d went to bid %s, want only the top bid %s (walk on an RPC fault)", i+1, id[:10], top[:10])
				}
			}
			if err != nil || !res.Opened {
				t.Fatalf("acquire after one transient fault: res %+v err %v", res, err)
			}
		})
	}
}

// Failure 1 (open amplification): while every open hits the RPC's 429, a
// request must stay on one bid instead of trying up to MaxBidAttempts
// bids (each one more chain round trip into the rate limit).
func TestRPC429PersistentDoesNotWalk(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	threeBidders(lr)
	lr.setOpenFault(faultBurst(-1, rpc429("build"), false))
	_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if err == nil {
		t.Fatal("acquire succeeded while every open fails")
	}
	if d := distinct(lr.bidsTried()); len(d) != 1 {
		t.Errorf("bids tried under persistent RPC 429 = %d, want 1 (no walk); opens %d", len(d), len(lr.bidsTried()))
	}
	if n := len(lr.bidsTried()); n < 2 || n > 10 {
		t.Errorf("open attempts = %d, want a bounded retry of the same bid (2..10)", n)
	}
	// Cap reached: a clean refusal that says when to retry (never 0).
	var na *NoAvailableError
	if !errors.As(err, &na) || na.RetryAfter < time.Second {
		t.Errorf("refusal after the retry cap: %v (want NoAvailableError with RetryAfter >= 1s)", err)
	}
	for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
		if p.IsCooling(modelB, pv) {
			t.Errorf("provider %s cooling after RPC 429s", pv[:6])
		}
	}
}

// Failure 2 (healthy provider cooled): an RPC 429 says nothing about the
// provider, so it must not start the provider's cool-down.
func TestRPC429DoesNotCoolProvider(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	threeBidders(lr)
	lr.setOpenFault(faultBurst(1, rpc429("bid"), false))
	if _, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"}); err != nil {
		t.Logf("acquire: %v", err)
	}
	for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
		if p.IsCooling(modelB, pv) {
			t.Errorf("provider %s cooling after an RPC 429 (not a provider fault)", pv[:6])
		}
	}
}

// Failure 2 (healthy provider cooled): a burst that fails one open per
// provider must not leave the model refused (all_cooling) once the RPC
// recovers: the next request is served.
func TestRPC429BurstThenNextRequestServed(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	threeBidders(lr)
	lr.setOpenFault(faultBurst(MaxBidAttempts, rpc429("gas"), false))
	_, _, _ = p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	lr.setOpenFault(nil) // RPC recovered
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k2"})
	var na *NoAvailableError
	if errors.As(err, &na) {
		t.Fatalf("next request refused after the RPC recovered (retry after %v): providers cooled by an RPC fault", na.RetryAfter)
	}
	if err != nil || res.SessionID == "" {
		t.Fatalf("next request: res %+v err %v", res, err)
	}
}

// Guard (passes today, must keep passing): a provider fault still cools
// the provider and walks on, so the fix is limited to RPC faults.
func TestProviderFaultStillCools(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	threeBidders(lr)
	lr.setOpenFault(faultBurst(1, `{"error":"provider unreachable"}`, false))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if err != nil || !res.Opened {
		t.Fatalf("acquire: res %+v err %v", res, err)
	}
	if !p.IsCooling(modelB, leaseProv1) {
		t.Fatal("provider fault did not cool the provider")
	}
	if d := distinct(lr.bidsTried()); len(d) != 2 {
		t.Fatalf("bids tried = %d, want 2 (walk past the failed provider)", len(d))
	}
}

// Failure 3 (orphaned session): the open tx landed but the receipt wait
// hit the RPC's 429. The session our wallet opened on that bid must be
// adopted or closed, not left open and unheld, both right after the
// request and after a rescan.
func TestReceiptWaitFailureNoOrphan(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	lr.setOpenFault(faultBurst(1, receiptWait429, true))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	t.Logf("acquire: res %+v err %v", res, err)
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("after the request: on-chain sessions neither pooled nor closed: %v", u)
	}
	c.Advance(orphanAge + time.Second) // now an orphan by age
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("after a rescan: on-chain sessions neither pooled nor closed: %v", u)
	}
}

// Failure 3 (rehydrate adopted 0): an unheld on-chain session older than
// orphanAge on a provider that is not excluded is adopted or closed by a
// rescan, even when that provider is cooling.
func TestRehydrateCoolingProviderNoOrphan(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	old := c.Now().Add(-orphanAge - time.Second)
	lr.addChain("0xorphan01", modelB, old, c.Now().Add(time.Hour))
	p.MarkCooling(modelB, leaseProv1)
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Fatalf("on-chain sessions neither pooled nor closed after a rescan: %v", u)
	}
}

// Failure 3 (several orphans on one model): the rescan adopts one session
// per model; the other orphans (older than orphanAge) must be closed, not
// left open and unheld.
func TestRehydrateExtraSessionsNoOrphan(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	old := c.Now().Add(-orphanAge - time.Second)
	lr.addChain("0xorphan01", modelB, old, c.Now().Add(time.Hour))
	lr.addChain("0xorphan02", modelB, old, c.Now().Add(50*time.Minute))
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Fatalf("on-chain sessions neither pooled nor closed after a rescan: %v", u)
	}
}

// Rated bids unreadable on a transient RPC 429: retried before refusing
// with rated_unreadable.
func TestRated429RetriedBeforeRefusing(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	var mu sync.Mutex
	n := 1
	lr.mu.Lock()
	lr.ratedFault = func(string) string {
		mu.Lock()
		defer mu.Unlock()
		if n == 0 {
			return ""
		}
		n--
		return rated429
	}
	lr.mu.Unlock()
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if err != nil || !res.Opened {
		t.Fatalf("acquire after one transient rated-bids 429: res %+v err %v", res, err)
	}
}

// Failure 1, item (1): transient RPC 429s are retried on the same bid
// with a backoff (no hot loop), then the open succeeds there; no cooling.
//
// Timing-dependent: the pool has no injectable sleep (only an injectable
// clock, SetClock), so the backoff is measured in wall time and only its
// presence is asserted (each gap >= 5ms); exponential growth, the cap and
// the jitter cannot be pinned without a sleep hook in production code.
func TestRPC429RetryBackoffSameBidTiming(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	threeBidders(lr)
	var mu sync.Mutex
	var at []time.Time
	faults := faultBurst(2, rpc429("gas"), false)
	lr.setOpenFault(func(bid string) (string, bool) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return faults(bid)
	})
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if d := distinct(lr.bidsTried()); len(d) != 1 {
		t.Errorf("bids tried = %d, want 1 (retry the same bid)", len(d))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Errorf("open attempts = %d, want 3 (two transient faults, then success)", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 5*time.Millisecond {
			t.Errorf("gap before attempt %d = %v, want a backoff (>= 5ms)", i+1, gap)
		}
	}
	if err != nil || !res.Opened {
		t.Errorf("acquire: res %+v err %v", res, err)
	}
	if p.IsCooling(modelB, leaseProv1) {
		t.Error("top provider cooling after transient RPC 429s")
	}
}

// Item (3): one wallet has at most one open in flight. 5 conversations
// on one model and 6 on another, all at once: never two opens at the
// router together, and each model's conversations share one session.
func TestConcurrentConversationsShareSessions(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.mu.Lock()
	lr.openHold = 100 * time.Millisecond
	lr.mu.Unlock()
	var wg sync.WaitGroup
	sessions := map[string]map[string]bool{modelA: {}, modelB: {}}
	var mu sync.Mutex
	for model, n := range map[string]int{modelA: 5, modelB: 6} {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, res, err := p.Acquire(model, AcquireOpts{KeyID: fmt.Sprintf("%s-k%d", model[:6], i)})
				if err != nil {
					t.Errorf("acquire %s #%d: %v", model[:6], i, err)
					return
				}
				mu.Lock()
				sessions[model][res.SessionID] = true
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	lr.mu.Lock()
	peak, opens := lr.maxInflight, len(lr.byBid)
	lr.mu.Unlock()
	if peak > 1 {
		t.Errorf("%d opens in flight at once from one wallet, want 1", peak)
	}
	if opens != 2 {
		t.Errorf("opens = %d, want 2 (one per model, shared by its conversations)", opens)
	}
	for m, ids := range sessions {
		if len(ids) != 1 {
			t.Errorf("model %s: conversations got %d sessions, want 1 shared", m[:6], len(ids))
		}
	}
}

// Burst across 8 models while the RPC is rate limited (the first 8 opens
// fault): opens stay one at a time, every model is served on its top bid,
// and no provider is cooled.
func TestBurstEightModelsUnderRPC429(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	models := append([]string{modelA, modelB, modelC, modelD}, extraModels...)
	lr.mu.Lock()
	for _, m := range models {
		lr.bidders[m] = []string{leaseProv1, leaseProv2, leaseProv3}
	}
	lr.openHold = 10 * time.Millisecond
	lr.mu.Unlock()
	lr.setOpenFault(faultBurst(len(models), rpc429("bid"), false))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("k%d", i)}); err != nil {
				t.Errorf("model %s not served: %v", m[:6], err)
			}
		}()
	}
	wg.Wait()
	lr.mu.Lock()
	peak := lr.maxInflight
	lr.mu.Unlock()
	if peak > 1 {
		t.Errorf("%d opens in flight at once from one wallet, want 1", peak)
	}
	if n := len(p.Snapshot()); n != len(models) {
		t.Errorf("pooled sessions = %d, want %d (every model served)", n, len(models))
	}
	top := map[string]bool{}
	for _, m := range models {
		top[bidID(m, leaseProv1)] = true
		for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
			if p.IsCooling(m, pv) {
				t.Errorf("model %s provider %s cooling after RPC 429s", m[:6], pv[:6])
			}
		}
	}
	for _, id := range lr.bidsTried() {
		if !top[id] {
			t.Errorf("open on bid %s: walked off a top bid on an RPC fault", id[:10])
		}
	}
}

// Orphan age boundary, young side: an unheld session opened just under
// orphanAge ago is left alone by a rescan (neither adopted nor closed);
// once the injected clock passes orphanAge it is adopted or closed.
func TestOrphanAgeBoundaryYoungLeftAlone(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	lr.addChain("0xyoung01", modelB, c.Now().Add(-orphanAge+time.Second), c.Now().Add(time.Hour))
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if n := lr.closedCount("0xyoung01"); n != 0 {
		t.Errorf("session younger than %v closed (%d closes), want left alone", orphanAge, n)
	}
	for _, s := range p.Snapshot() {
		if s.SessionID == "0xyoung01" {
			t.Errorf("session younger than %v adopted, want left alone", orphanAge)
		}
	}
	c.Advance(2 * time.Second) // now just over orphanAge
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("past %v: sessions neither pooled nor closed: %v", orphanAge, u)
	}
}

// Orphan age boundary, old side: unheld sessions opened just over
// orphanAge ago, on two models, are each adopted or closed by one rescan.
func TestOrphanAgeBoundaryOldHandled(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	old := c.Now().Add(-orphanAge - time.Second)
	lr.addChain("0xold0001", modelA, old, c.Now().Add(time.Hour))
	lr.addChain("0xold0002", modelB, old, c.Now().Add(time.Hour))
	lr.addChain("0xold0003", modelB, old, c.Now().Add(40*time.Minute))
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("orphans older than %v neither pooled nor closed: %v", orphanAge, u)
	}
}

// Guard: a held session (leased by a conversation) older than orphanAge
// is left alone by a rescan: not closed and still pooled.
func TestOrphanAgeHeldLeftAlone(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	_, res := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	c.Advance(orphanAge + time.Minute)
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if n := lr.closedCount(res.SessionID); n != 0 {
		t.Errorf("held session closed (%d closes)", n)
	}
	pooled := false
	for _, s := range p.Snapshot() {
		pooled = pooled || s.SessionID == res.SessionID
	}
	if !pooled {
		t.Error("held session dropped from the pool")
	}
}
