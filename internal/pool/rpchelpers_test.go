package pool

// Shared helpers for the router RPC rate-limit tests (rpc*_test.go). The
// mock is leaseRouter (lease_test.go): it keeps running after the client
// gives up, like the real router. Waits go through the pool's sleep seam
// (SetSleep): sleepRec records them and advances the injected clock, so
// nothing sleeps for real. Slot wall-clock bounds can be shrunk with
// scaleSlot.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// extraModels are more mock models (with modelA..modelD: eight) for the
// burst tests; leaseRouter resolves their bids too.
var extraModels = []string{
	"0xeeee000000000000000000000000000000000000000000000000000000000000",
	"0xffff000000000000000000000000000000000000000000000000000000000000",
	"0x1010000000000000000000000000000000000000000000000000000000000000",
	"0x2020000000000000000000000000000000000000000000000000000000000000",
}

func eightModels() []string { return append([]string{modelA, modelB, modelC, modelD}, extraModels...) }

// routerErr is a router status-500 body.
func routerErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// RPC 429 texts in the router's v7.18.0 wording (see
// testdata/router-v7.18.0-errors.txt).
const (
	rpc429Tail        = `429 Too Many Requests: {"jsonrpc":"2.0","id":7,"error":{"code":429,"message":"Your app has exceeded its compute units per second capacity."}}`
	stageBid          = "failed to get bid: "
	stageApprove      = "failed to send transaction: approve MOR allowance failed: "
	stageTxOpts       = "failed to get transactOpts: "
	stageGas          = "failed to send transaction: open session failed: failed to get initial gas prices: "
	stageNonce        = "failed to send transaction: open session failed: failed to get nonce: "
	stageBuild        = "failed to send transaction: open session failed: failed to build transaction: "
	pingIOTimeout     = "provider healthcheck ping failed: provider request failed: code: 500, msg: <nil>, error: failed to connect to provider: dial tcp <provider>:3333: i/o timeout"
	pingConnRefused   = "provider healthcheck ping failed: provider request failed: code: 500, msg: <nil>, error: failed to connect to provider: dial tcp <provider>:3333: connect: connection refused"
	receiptWait429    = "failed to send transaction: parse session receipt failed: failed to wait for confirmations 429 Too Many Requests"
	rated429          = `{"error":"Failed to call contract: 429 Too Many Requests"}`
	escalationNotSent = "failed to send transaction: open session failed: failed to send transaction: already known"
)

// rl429 is a 500 body: stage followed by the RPC's 429.
func rl429(stage string) string { return routerErr(stage + rpc429Tail) }

// sleepRec is the test sleep: it records each wait and advances the
// injected clock by it instead of sleeping.
type sleepRec struct {
	mu  sync.Mutex
	got []time.Duration
	c   *fakeClock
}

func (r *sleepRec) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.got = append(r.got, d)
	r.mu.Unlock()
	if r.c != nil {
		r.c.Advance(d)
	}
	return nil
}

func (r *sleepRec) sleeps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.got...)
}

func (r *sleepRec) reset() {
	r.mu.Lock()
	r.got = nil
	r.mu.Unlock()
}

// installSleep routes the pool's sleep seam to a recorder for the test.
func installSleep(p *Pool, c *fakeClock) *sleepRec {
	r := &sleepRec{c: c}
	p.SetSleep(r.sleep)
	return r
}

// rpcEnv is a mock router and pool on an injected clock and sleep. The
// pool has run longer than pruneGrace (shared-wallet checks are on) and
// knows the router has one RPC URL.
func rpcEnv(t *testing.T) (*fakeClock, *leaseRouter, *Pool, *sleepRec) {
	t.Helper()
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.mu.Lock()
	p.started = c.Now().Add(-time.Hour)
	p.mu.Unlock()
	if n := p.RefreshRPCURLCount(context.Background()); n != 1 {
		t.Fatalf("rpc url count = %d, want 1", n)
	}
	return c, lr, p, installSleep(p, c)
}

// scaleSlot runs the slot's wall-clock bounds k times faster for the test
// (a 60 s hold cap becomes 60 s / k of real time).
func scaleSlot(t *testing.T, k int64) {
	prev := callTimeScale.Swap(k)
	t.Cleanup(func() { callTimeScale.Store(prev) })
}

// Backoff bands: step n sleeps within [d/2, d), d = min(0.5 s << n, 4 s).
var backoffSteps = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// assertBackoff checks the first n backoff steps: each within its equal
// jitter band and the total within the retry budget.
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
		if step := backoffSteps[i]; d < step/2 || d >= step {
			t.Errorf("backoff step %d slept %v, want within [%v, %v)", i+1, d, step/2, step)
		}
	}
	if sum > transientBudget {
		t.Errorf("total backoff %v exceeds the %v budget", sum, transientBudget)
	}
}

// refusal unpacks a *NoAvailableError.
func refusal(t *testing.T, err error) *NoAvailableError {
	t.Helper()
	var na *NoAvailableError
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want a NoAvailableError", err)
	}
	return na
}

// assertTransient: a router_transient refusal with Retry-After in [lo, hi].
func assertTransient(t *testing.T, err error, lo, hi time.Duration) {
	t.Helper()
	na := refusal(t, err)
	if na.Reason != ReasonRouterTransient || na.RetryAfter < lo || na.RetryAfter > hi {
		t.Fatalf("refusal = %+v, want router_transient with Retry-After in [%v, %v]", na, lo, hi)
	}
}

// assertOpenBusy: an open_busy refusal, no Retry-After.
func assertOpenBusy(t *testing.T, err error) {
	t.Helper()
	na := refusal(t, err)
	if na.Reason != ReasonOpenBusy || na.RetryAfter != 0 {
		t.Fatalf("refusal = %+v, want open_busy with no Retry-After", na)
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

// faultSeq answers the by-bid opens with bodies in order ("" opens
// normally), then opens normally.
func faultSeq(bodies ...string) func(string) (string, bool) {
	var mu sync.Mutex
	return func(string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return "", false
		}
		b := bodies[0]
		bodies = bodies[1:]
		return b, false
	}
}

func (lr *leaseRouter) setOpenFault(f func(string) (string, bool)) {
	lr.mu.Lock()
	lr.openFault = f
	lr.mu.Unlock()
}

func (lr *leaseRouter) set(f func(lr *leaseRouter)) {
	lr.mu.Lock()
	f(lr)
	lr.mu.Unlock()
}

func (lr *leaseRouter) bidsTried() []string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return append([]string(nil), lr.byBid...)
}

func (lr *leaseRouter) closesSent() []string {
	_, cl := lr.snapshot()
	return cl
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

func pooledIDs(p *Pool) []string {
	var out []string
	for _, s := range p.Snapshot() {
		out = append(out, s.SessionID)
	}
	sort.Strings(out)
	return out
}

// threeBidders gives model three healthy providers in rated order.
func threeBidders(lr *leaseRouter, model string) {
	lr.mu.Lock()
	lr.bidders[model] = []string{leaseProv1, leaseProv2, leaseProv3}
	lr.mu.Unlock()
}

func assertNoCooling(t *testing.T, p *Pool, model string) {
	t.Helper()
	for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
		if p.IsCooling(model, pv) {
			t.Errorf("model %s provider %s cooling (not a provider fault)", model[:6], pv[:6])
		}
	}
}

func assertOnlyBid(t *testing.T, lr *leaseRouter, bid string) {
	t.Helper()
	for i, id := range lr.bidsTried() {
		if id != bid {
			t.Errorf("open %d went to bid %s, want only %s", i+1, short(id), short(bid))
		}
	}
}

// assertNoOrphans: after the sweep, the mock's on-chain open set equals
// the pool's held set.
func assertNoOrphans(t *testing.T, lr *leaseRouter, p *Pool) {
	t.Helper()
	ctx := context.Background()
	for range 20 {
		p.ScanOrphans(ctx)
	}
	if got, want := lr.openSet(), pooledIDs(p); !slices.Equal(got, want) {
		t.Fatalf("on-chain open %v, held %v: orphans", got, want)
	}
}

// ageAll makes every mock session older than pruneGrace on the fake clock.
func ageAll(c *fakeClock) { c.Advance(pruneGrace + time.Second) }

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) count(sub string) int { return strings.Count(s.String(), sub) }

// captureLogs sends slog and log output to a buffer for the test.
func captureLogs(t *testing.T) *syncBuf {
	buf := &syncBuf{}
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// within fails the test when f does not return within d (deadlock guard).
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v (deadlock?)", what, d)
	}
}

var _ = fmt.Sprintf
