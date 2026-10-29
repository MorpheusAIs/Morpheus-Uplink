package pool

// Router RPC rate-limit tests. When the router's chain RPC is rate
// limited, every router call that reads or writes chain can fail with the
// RPC's 429, which the router wraps in a status 500 answer. These tests
// pin the expected handling; they FAIL on the current code until the fix
// lands:
//
//   - a transient RPC fault on open is retried on the same bid with a
//     capped exponential backoff (base 0.5s, doubling, capped at 4s per
//     step, equal jitter: each step sleeps in [d/2, d]); 5 attempts within
//     a 15s budget; no walk to other bids (each walk step is one more
//     chain round trip) and no provider cool-down;
//   - after the cap the refusal says when to retry: 1..4s, never 0;
//   - an open that may have been sent is never re-opened: it is reconciled
//     inside the request (3 session-list polls 2s apart) and the landed
//     session adopted;
//   - the 2-minute sweep leaves sessions younger than orphanAge alone;
//     only the in-request reconcile adopts a young session, and only the
//     one matching its own attempt;
//   - a transient RPC fault on rated bids gets the same backoff;
//   - one open slot per wallet: opens never overlap (concurrent txs from
//     one wallet race on the nonce: "replacement transaction
//     underpriced"); concurrent conversations on a model share one
//     session.
//
// Waits go through the pool's sleep seam (SetSleep): sleepRec records
// them and advances the injected clock, so nothing sleeps for real.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// Retry schedule: base 0.5s doubling, capped at 4s per step, equal
// jitter; 4 retries (5 attempts) within a 15s budget.
var backoffSteps = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

const (
	retryBudget    = 15 * time.Second
	reconcileGap   = 2 * time.Second // between in-request session-list polls
	reconcilePolls = 3
)

// sleepRec is the test sleep: it records each wait and advances the
// injected clock by it instead of sleeping.
type sleepRec struct {
	mu  sync.Mutex
	got []time.Duration
	c   *fakeClock
}

func (r *sleepRec) sleep(d time.Duration) {
	r.mu.Lock()
	r.got = append(r.got, d)
	r.mu.Unlock()
	if r.c != nil {
		r.c.Advance(d)
	}
}

func (r *sleepRec) sleeps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.got...)
}

// installSleep routes the pool's sleep seam to a recorder for the test.
func installSleep(t *testing.T, c *fakeClock) *sleepRec {
	r := &sleepRec{c: c}
	t.Cleanup(SetSleep(r.sleep))
	return r
}

// rpcEnv is a mock router and pool on an injected clock and sleep.
func rpcEnv(t *testing.T) (*fakeClock, *leaseRouter, *Pool, *sleepRec) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	return c, lr, p, installSleep(t, c)
}

// assertBackoff checks the first n backoff steps: each sleep in [d/2, d]
// and the total within the retry budget.
func assertBackoff(t *testing.T, got []time.Duration, n int) {
	t.Helper()
	if len(got) != n {
		t.Errorf("backoff sleeps = %v (%d), want %d steps", got, len(got), n)
	}
	var sum time.Duration
	for i, d := range got {
		sum += d
		if i >= len(backoffSteps) {
			continue
		}
		if step := backoffSteps[i]; d < step/2 || d > step {
			t.Errorf("backoff step %d slept %v, want within [%v, %v] (equal jitter)", i+1, d, step/2, step)
		}
	}
	if sum > retryBudget {
		t.Errorf("total backoff %v exceeds the %v budget", sum, retryBudget)
	}
}

// assertRetryAfterCap: the refusal after the retry cap is a
// NoAvailableError with a fixed 1..4s hint, never 0.
func assertRetryAfterCap(t *testing.T, err error) {
	t.Helper()
	var na *NoAvailableError
	if !errors.As(err, &na) || na.RetryAfter < time.Second || na.RetryAfter > 4*time.Second {
		t.Errorf("refusal after the retry cap: %v (want NoAvailableError with RetryAfter 1..4s)", err)
	}
}

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

// countLists counts session-list answers (offset 0) from now on.
func (lr *leaseRouter) countLists() func() int {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.lists = 0
	return func() int {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.lists
	}
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

func pooledIDs(p *Pool) []string {
	var out []string
	for _, s := range p.Snapshot() {
		out = append(out, s.SessionID)
	}
	return out
}

// threeBidders gives modelB three healthy providers in rated order.
func threeBidders(lr *leaseRouter) {
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv1, leaseProv2, leaseProv3}
	lr.mu.Unlock()
}

func assertNoCooling(t *testing.T, p *Pool, model string) {
	t.Helper()
	for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
		if p.IsCooling(model, pv) {
			t.Errorf("model %s provider %s cooling after RPC 429s (not a provider fault)", model[:6], pv[:6])
		}
	}
}

func assertOnlyTopBid(t *testing.T, lr *leaseRouter, model string) {
	t.Helper()
	top := bidID(model, leaseProv1)
	for i, id := range lr.bidsTried() {
		if id != top {
			t.Errorf("open %d went to bid %s, want only the top bid %s (walk on an RPC fault)", i+1, id[:10], top[:10])
		}
	}
}

// Failure 1 (open amplification): one RPC 429 on the top bid must not
// walk to the next bids. Expected: one backoff step, then the open is
// retried on the same bid and succeeds there.
func TestRPC429OpenStaysOnSameBid(t *testing.T) {
	for step := range rpcSteps {
		t.Run(step, func(t *testing.T) {
			_, lr, p, sr := rpcEnv(t)
			threeBidders(lr)
			lr.setOpenFault(faultBurst(1, rpc429(step), false))
			_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
			assertOnlyTopBid(t, lr, modelB)
			if n := len(lr.bidsTried()); n != 2 {
				t.Errorf("open attempts = %d, want 2 (one fault, one retry)", n)
			}
			assertBackoff(t, sr.sleeps(), 1)
			if err != nil || !res.Opened {
				t.Fatalf("acquire after one transient fault: res %+v err %v", res, err)
			}
		})
	}
}

// Item 1, the schedule: four transient faults, then the fifth attempt
// (the last within the budget) succeeds on the same bid. Sleeps follow
// the schedule exactly (each in [d/2, d] for 0.5, 1, 2, 4s); nothing cools.
func TestRPC429RetryScheduleSameBid(t *testing.T) {
	_, lr, p, sr := rpcEnv(t)
	threeBidders(lr)
	lr.setOpenFault(faultBurst(4, rpc429("gas"), false))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := len(lr.bidsTried()); n != 5 {
		t.Errorf("open attempts = %d, want 5 (4 faults, then success)", n)
	}
	assertOnlyTopBid(t, lr, modelB)
	assertBackoff(t, sr.sleeps(), 4)
	if err != nil || !res.Opened {
		t.Errorf("acquire on the fifth attempt: res %+v err %v", res, err)
	}
	assertNoCooling(t, p, modelB)
}

// Failure 1 (open amplification): while every open hits the RPC's 429, a
// request makes exactly 5 attempts on one bid (the full schedule) instead
// of walking MaxBidAttempts bids, then refuses with Retry-After 1..4s.
func TestRPC429PersistentDoesNotWalk(t *testing.T) {
	_, lr, p, sr := rpcEnv(t)
	threeBidders(lr)
	lr.setOpenFault(faultBurst(-1, rpc429("build"), false))
	_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if err == nil {
		t.Fatal("acquire succeeded while every open fails")
	}
	if n := len(lr.bidsTried()); n != 5 {
		t.Errorf("open attempts = %d, want exactly 5 on one bid", n)
	}
	assertOnlyTopBid(t, lr, modelB)
	assertBackoff(t, sr.sleeps(), 4)
	assertRetryAfterCap(t, err)
	assertNoCooling(t, p, modelB)
}

// Failure 2 (healthy provider cooled): an RPC 429 says nothing about the
// provider, so it must not start the provider's cool-down.
func TestRPC429DoesNotCoolProvider(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	threeBidders(lr)
	lr.setOpenFault(faultBurst(1, rpc429("bid"), false))
	if _, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"}); err != nil {
		t.Logf("acquire: %v", err)
	}
	assertNoCooling(t, p, modelB)
}

// Failure 2 (healthy provider cooled): a burst that fails one open per
// provider must not leave the model refused (all_cooling) once the RPC
// recovers: the next request is served.
func TestRPC429BurstThenNextRequestServed(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
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

// Item 3 / failure 3: the open tx landed but the receipt wait hit the
// RPC's 429 (maybe-sent). The request reconciles at once: no second
// open, and the landed session (on its own bid) is adopted and handed
// out. A later sweep finds nothing left unheld.
func TestReceiptWaitReconcileAdoptsInRequest(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	threeBidders(lr)
	lr.setOpenFault(faultBurst(1, receiptWait429, true))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := len(lr.bidsTried()); n != 1 {
		t.Errorf("opens = %d, want 1 (a maybe-sent open is never re-opened)", n)
	}
	if err != nil || res.SessionID != "0xs001" {
		t.Errorf("acquire: res %+v err %v, want the landed session 0xs001 adopted", res, err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("after the request: on-chain sessions neither pooled nor closed: %v", u)
	}
	c.Advance(orphanAge + time.Second)
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("after a rescan: on-chain sessions neither pooled nor closed: %v", u)
	}
}

// Item 3, polling: the landed session shows in the session list only
// on the third poll. The reconcile makes 3 polls 2s apart (through the
// sleep seam), adopts it, and does not open again.
func TestReceiptWaitReconcilePollsTwoSecondsApart(t *testing.T) {
	_, lr, p, sr := rpcEnv(t)
	threeBidders(lr)
	lr.mu.Lock()
	lr.hideLanded = reconcilePolls - 1
	lr.mu.Unlock()
	lr.setOpenFault(faultBurst(1, receiptWait429, true))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := len(lr.bidsTried()); n != 1 {
		t.Errorf("opens = %d, want 1 (a maybe-sent open is never re-opened)", n)
	}
	if err != nil || res.SessionID != "0xs001" {
		t.Errorf("acquire: res %+v err %v, want the landed session adopted on the third poll", res, err)
	}
	assertReconcileSleeps(t, sr.sleeps())
}

// assertReconcileSleeps: the waits between reconcile polls are 2s each,
// 2 or 3 of them (3 polls; whether the first poll waits is open).
func assertReconcileSleeps(t *testing.T, got []time.Duration) {
	t.Helper()
	if len(got) < reconcilePolls-1 || len(got) > reconcilePolls {
		t.Errorf("reconcile sleeps = %v, want %d..%d waits of %v", got, reconcilePolls-1, reconcilePolls, reconcileGap)
	}
	for i, d := range got {
		if d != reconcileGap {
			t.Errorf("reconcile wait %d = %v, want %v", i+1, d, reconcileGap)
		}
	}
}

// Item 3, not visible in time: the landed session stays out of the list
// for all 3 polls. The request refuses (Retry-After 1..4s) without a
// second open, exactly 3 polls; the young session is left alone (not
// closed) until the sweep finds it past orphanAge and adopts or closes it.
func TestReceiptWaitNotVisibleNoReopen(t *testing.T) {
	c, lr, p, sr := rpcEnv(t)
	threeBidders(lr)
	lr.mu.Lock()
	lr.hideLanded = reconcilePolls
	lr.mu.Unlock()
	lr.setOpenFault(faultBurst(1, receiptWait429, true))
	lists := lr.countLists()
	_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := len(lr.bidsTried()); n != 1 {
		t.Errorf("opens = %d, want 1 (a maybe-sent open is never re-opened)", n)
	}
	lr.mu.Lock()
	after := lr.listsAfterLand
	lr.mu.Unlock()
	if after != reconcilePolls {
		t.Errorf("session-list polls after the maybe-sent open = %d, want %d (total lists %d)", after, reconcilePolls, lists())
	}
	assertReconcileSleeps(t, sr.sleeps())
	assertRetryAfterCap(t, err)
	if n := lr.closedCount("0xs001"); n != 0 {
		t.Errorf("young landed session closed (%d closes), want left alone", n)
	}
	c.Advance(orphanAge + time.Second)
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("past %v: sessions neither pooled nor closed: %v", orphanAge, u)
	}
}

// Item 4: a young unheld session (under orphanAge) on another bid does
// not match this request's attempt, so neither the sweep nor the
// in-request reconcile adopts or closes it. Today the rescan before an
// open adopts it.
func TestYoungUnmatchedSessionNotAdopted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(string) (string, bool)
	}{
		{"open_ok", nil},
		{"maybe_sent_not_landed", faultBurst(-1, `{"error":"EOF"}`, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			threeBidders(lr)
			lr.addChainOn("0xyoung0001", modelB, leaseProv2, c.Now().Add(-30*time.Second), c.Now().Add(time.Hour))
			lr.setOpenFault(tc.fault)
			_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
			t.Logf("acquire: res %+v err %v", res, err)
			if res.SessionID == "0xyoung0001" || slices.Contains(pooledIDs(p), "0xyoung0001") {
				t.Errorf("young unmatched session adopted, want left alone")
			}
			if n := lr.closedCount("0xyoung0001"); n != 0 {
				t.Errorf("young unmatched session closed (%d closes), want left alone", n)
			}
			if n := len(lr.bidsTried()); n != 1 {
				t.Errorf("opens = %d, want 1", n)
			}
		})
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

// Rated bids unreadable on a transient RPC 429: retried (one backoff
// step) before refusing with rated_unreadable.
func TestRated429RetriedBeforeRefusing(t *testing.T) {
	_, lr, p, sr := rpcEnv(t)
	calls := ratedFaults(lr, 1)
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if err != nil || !res.Opened {
		t.Fatalf("acquire after one transient rated-bids 429: res %+v err %v (rated reads %d)", res, err, calls())
	}
	assertBackoff(t, sr.sleeps(), 1)
}

// Item 5: rated bids keep hitting the RPC's 429: the same schedule (5
// reads, 4 backoff steps within the budget), then a refusal with
// Retry-After 1..4s; no open.
func TestRated429PersistentBackoffThenRefuse(t *testing.T) {
	_, lr, p, sr := rpcEnv(t)
	calls := ratedFaults(lr, -1)
	_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := calls(); n != 5 {
		t.Errorf("rated-bids reads = %d, want 5", n)
	}
	assertBackoff(t, sr.sleeps(), 4)
	assertRetryAfterCap(t, err)
	if n := len(lr.bidsTried()); n != 0 {
		t.Errorf("opens = %d with rated bids unreadable, want 0", n)
	}
}

// ratedFaults answers the next n rated-bids reads (n < 0: all) with
// rated429 and returns a counter of reads answered with it.
func ratedFaults(lr *leaseRouter, n int) func() int {
	var mu sync.Mutex
	hits := 0
	lr.mu.Lock()
	lr.ratedFault = func(string) string {
		mu.Lock()
		defer mu.Unlock()
		if n == 0 {
			return ""
		}
		if n > 0 {
			n--
		}
		hits++
		return rated429
	}
	lr.mu.Unlock()
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// gateOpens makes every by-bid open report its bid on the returned
// arrivals channel and wait until release is called.
func gateOpens(t *testing.T, lr *leaseRouter) (arrivals chan string, release func()) {
	arrivals = make(chan string, 256)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	lr.mu.Lock()
	lr.arrivals, lr.gate = arrivals, gate
	lr.mu.Unlock()
	return arrivals, release
}

func awaitArrival(t *testing.T, arrivals chan string, within time.Duration) string {
	t.Helper()
	select {
	case id := <-arrivals:
		return id
	case <-time.After(within):
		t.Fatalf("no open reached the router within %v", within)
		return ""
	}
}

// noSecondOpen reports a second open arriving while the first is held
// and returns its bid ("" when none came).
func noSecondOpen(t *testing.T, arrivals chan string) string {
	t.Helper()
	select {
	case id := <-arrivals:
		t.Errorf("a second open (bid %s) reached the router while one was in flight: one open slot per wallet", id[:10])
		return id
	case <-time.After(300 * time.Millisecond):
		return ""
	}
}

// Item 6: one open slot per wallet. 5 conversations on one model and 6
// on another, all at once, while the first open is held at the router:
// no second open arrives until it is released; then each model's
// conversations share one session (2 opens in all, peak 1 in flight).
func TestConcurrentConversationsShareSessions(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	arrivals, release := gateOpens(t, lr)
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
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
	awaitArrival(t, arrivals, 5*time.Second)
	noSecondOpen(t, arrivals)
	release()
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
// fault): opens stay one at a time (held first open: no second arrives),
// every model is served on its top bid, and no provider is cooled.
func TestBurstEightModelsUnderRPC429(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	models := append([]string{modelA, modelB, modelC, modelD}, extraModels...)
	lr.mu.Lock()
	for _, m := range models {
		lr.bidders[m] = []string{leaseProv1, leaseProv2, leaseProv3}
	}
	lr.mu.Unlock()
	arrivals, release := gateOpens(t, lr)
	lr.setOpenFault(faultBurst(len(models), rpc429("bid"), false))
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.Acquire(m, AcquireOpts{KeyID: fmt.Sprintf("k%d", i)}); err != nil {
				t.Errorf("model %s not served: %v", m[:6], err)
			}
		}()
	}
	awaitArrival(t, arrivals, 5*time.Second)
	noSecondOpen(t, arrivals)
	release()
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
		assertNoCooling(t, p, m)
	}
	for _, id := range lr.bidsTried() {
		if !top[id] {
			t.Errorf("open on bid %s: walked off a top bid on an RPC fault", id[:10])
		}
	}
}

// Orphan age boundary, young side (item 4: a deliberate change from the
// current sweep): an unheld session opened just under orphanAge ago is
// left alone by a rescan (neither adopted nor closed); once the injected
// clock passes orphanAge it is adopted or closed.
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
