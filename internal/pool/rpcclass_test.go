package pool

// Open-error classification and what each class does on the
// open path: provider fault → cool and walk; proven pre-send chain-stage
// error → same bid (backoff with the operator's RPC set, one attempt
// without); anything else → maybe sent: one open POST, reconcile, never
// re-opened.

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const fixturePath = "testdata/router-v7.18.0-errors.txt"

type fixtureCase struct {
	class       string
	postElapsed time.Duration
	text        string
}

func fixtureCases(t *testing.T) []fixtureCase {
	t.Helper()
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fixtureCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			t.Fatalf("fixture line %q: want <class><TAB><postElapsed><TAB><text>", line)
		}
		d, err := time.ParseDuration(parts[1])
		if err != nil {
			t.Fatalf("fixture line %q: %v", line, err)
		}
		out = append(out, fixtureCase{parts[0], d, parts[2]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func status500(text string) error {
	return &router.StatusError{Code: http.StatusInternalServerError, Body: routerErr(text)}
}

// Every fixture line classifies as stated (operator RPC set, window
// retry unused).
func TestClassifierRouterFixture(t *testing.T) {
	cases := fixtureCases(t)
	if len(cases) < 100 {
		t.Fatalf("fixture has %d cases, want every marker after every prefix", len(cases))
	}
	for _, c := range cases {
		if got := classifyOpenErr(status500(c.text), c.postElapsed, true).String(); got != c.class {
			t.Errorf("%s after %v: class %s, want %s", c.text, c.postElapsed, got, c.class)
		}
	}
}

// Every prefix and deny-list constant appears in the fixture (lowercase
// match), so a renamed router string fails here, and the P* lines are
// there at 2 s and at 25 s.
func TestClassifierFixtureCoversEveryPrefix(t *testing.T) {
	cases := fixtureCases(t)
	has := func(pred func(fixtureCase) bool) bool {
		for _, c := range cases {
			if pred(c) {
				return true
			}
		}
		return false
	}
	for _, p := range append(append([]string{}, preSendPrefixes...), windowPrefixes...) {
		if !has(func(c fixtureCase) bool { return strings.HasPrefix(strings.ToLower(c.text), p) }) {
			t.Errorf("prefix %q not in %s", p, fixturePath)
		}
	}
	for _, p := range windowPrefixes {
		for _, at := range []time.Duration{2 * time.Second, 25 * time.Second} {
			if !has(func(c fixtureCase) bool {
				return c.postElapsed == at && strings.HasPrefix(strings.ToLower(c.text), p)
			}) {
				t.Errorf("P* prefix %q has no line at %v", p, at)
			}
		}
	}
	for _, p := range providerStageTexts {
		if !has(func(c fixtureCase) bool { return strings.Contains(strings.ToLower(c.text), p) }) {
			t.Errorf("provider-stage text %q not in %s", p, fixturePath)
		}
	}
	for _, m := range append([]string{"429"}, rateLimitMarkers...) {
		if !has(func(c fixtureCase) bool { return strings.Contains(strings.ToLower(c.text), m) }) {
			t.Errorf("marker %q not in %s", m, fixturePath)
		}
	}
}

// TestRouterRateLimitMarkers: each router marker after each anchored
// prefix is transient; the same words inside a provider-stage text are a
// provider fault.
func TestRouterRateLimitMarkers(t *testing.T) {
	markers := []string{"429 Too Many Requests", "Too Many Requests", "rate limit", "usage limit", "quota exceeded",
		"request limit", "daily limit", "monthly limit", "exceeded its compute units per second capacity"}
	for _, m := range markers {
		for _, p := range preSendPrefixes {
			if got := classifyOpenErr(status500(p+m), time.Second, true); got != transientPreSend {
				t.Errorf("%q: %s, want transient_presend", p+m, got)
			}
		}
		for _, p := range windowPrefixes {
			if got := classifyOpenErr(status500(p+m), 2*time.Second, true); got != transientWindow {
				t.Errorf("%q: %s, want transient_window", p+m, got)
			}
		}
		for _, ps := range providerStageTexts {
			if got := classifyOpenErr(status500(ps+": "+m), time.Second, true); got != providerFault {
				t.Errorf("%q: %s, want provider_fault", ps+": "+m, got)
			}
		}
	}
}

// TestChainStageErrorNeverProviderFault: any error after a pre-send
// chain-stage prefix (no rate-limit words, a timeout, a revert, a bare
// code) is transient on the same bid, never a provider fault; a proven
// P* the same; a slow P*, or a P* without the operator's RPC, is maybe
// sent.
func TestChainStageErrorNeverProviderFault(t *testing.T) {
	tails := []string{"-32005", "429 Too Many Requests: execution reverted", "nonce 14290 too high", "0xabc429def",
		"execution reverted: 0x429a", "context deadline exceeded", "Post \"https://rpc\": dial tcp: i/o timeout", "502 Bad Gateway"}
	for _, tail := range tails {
		for _, p := range preSendPrefixes {
			for _, windowOK := range []bool{true, false} {
				if got := classifyOpenErr(status500(p+tail), 30*time.Second, windowOK); got != transientPreSend {
					t.Errorf("%q (windowOK %v): %s, want transient_presend", p+tail, windowOK, got)
				}
			}
		}
		for _, p := range windowPrefixes {
			if got := classifyOpenErr(status500(p+tail), 2*time.Second, true); got != transientWindow {
				t.Errorf("%q fast: %s, want transient_window", p+tail, got)
			}
			if got := classifyOpenErr(status500(p+tail), 25*time.Second, true); got != maybeSent {
				t.Errorf("%q slow: %s, want maybe_sent", p+tail, got)
			}
			if got := classifyOpenErr(status500(p+tail), 2*time.Second, false); got != maybeSent {
				t.Errorf("%q without the operator RPC: %s, want maybe_sent", p+tail, got)
			}
		}
	}
}

// TestChainStageTimeoutSameBid: a pre-send chain-stage timeout with the
// operator's RPC set retries the same bid after one backoff step and
// cools nothing.
func TestChainStageTimeoutSameBid(t *testing.T) {
	sameBidRetry(t, routerErr(stageBid+"context deadline exceeded"), time.Second)
}

// TestBareWordsNotTransient: unanchored rate-limit or timeout words in a
// non-provider body on the open are maybe-sent: one POST, never retried.
func TestBareWordsNotTransient(t *testing.T) {
	for _, body := range []string{"timeout", "rate limit exceeded", "429 Too Many Requests", "connection refused"} {
		t.Run(body, func(t *testing.T) {
			if got := classifyOpenErr(status500(body), time.Second, true); got != maybeSent {
				t.Fatalf("class %s, want maybe_sent", got)
			}
			_, lr, p, _ := rpcEnv(t)
			lr.setOpenFault(faultBurst(-1, routerErr(body), false))
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			assertTransient(t, err, 4*time.Second, 4*time.Second)
			if n := len(lr.bidsTried()); n != 1 {
				t.Fatalf("open POSTs = %d, want 1 (never retried)", n)
			}
			assertNoCooling(t, p, modelB)
		})
	}
}

// providerFaultWalk: body on the top bid cools that provider once and
// the open walks to bid 2.
func providerFaultWalk(t *testing.T, body string) {
	t.Helper()
	_, lr, p, _ := rpcEnv(t)
	top := bidID(modelB, leaseProv1)
	lr.setOpenFault(func(bid string) (string, bool) {
		if bid == top {
			return routerErr(body), false
		}
		return "", false
	})
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	tried := lr.bidsTried()
	if len(tried) != 2 || tried[0] != top || tried[1] != bidID(modelB, leaseProv2) {
		t.Fatalf("bids tried %v, want top then bid 2", tried)
	}
	if !p.IsCooling(modelB, leaseProv1) || p.IsCooling(modelB, leaseProv2) {
		t.Fatalf("cooling: top %v, bid 2 %v; want only the top", p.IsCooling(modelB, leaseProv1), p.IsCooling(modelB, leaseProv2))
	}
	if !res.Opened {
		t.Fatalf("result %+v, want opened on bid 2", res)
	}
}

func TestPingIOTimeoutIsProviderFault(t *testing.T)   { providerFaultWalk(t, pingIOTimeout) }
func TestPingConnRefusedIsProviderFault(t *testing.T) { providerFaultWalk(t, pingConnRefused) }

func TestProviderBodyRateLimitIsProviderFault(t *testing.T) {
	for _, msg := range []string{"rate limit exceeded", "429"} {
		t.Run(msg, func(t *testing.T) {
			providerFaultWalk(t, "provider healthcheck ping failed: provider request failed: code: 429, msg: "+msg)
		})
	}
}

// sameBidRetry: body once on the top bid, then the open succeeds on the
// same bid after one backoff step; nothing cools.
func sameBidRetry(t *testing.T, body string, take time.Duration) {
	t.Helper()
	c, lr, p, sl := rpcEnv(t)
	first := true
	lr.setOpenFault(func(string) (string, bool) {
		if first {
			first = false
			c.Advance(take)
			return body, false
		}
		return "", false
	})
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if !res.Opened {
		t.Fatalf("result %+v, want opened", res)
	}
	if n := len(lr.bidsTried()); n != 2 {
		t.Fatalf("open POSTs = %d, want 2 (one retry)", n)
	}
	assertOnlyBid(t, lr, bidID(modelB, leaseProv1))
	assertNoCooling(t, p, modelB)
	assertBackoff(t, sl.sleeps(), 1)
}

func TestJSONCode429AfterPrefixIsTransient(t *testing.T) {
	sameBidRetry(t, rl429(stageBid), time.Second)
}

func TestApproveStageIsPreSend(t *testing.T) {
	// Pre-send at any time: the approve tx locks no stake.
	if got := classifyOpenErr(status500(stageApprove+rpc429Tail), 30*time.Second, false); got != transientPreSend {
		t.Fatalf("approve 429 after 30 s: %s, want transient_presend", got)
	}
	sameBidRetry(t, rl429(stageApprove), time.Second)
}

// windowCase: a P* 429 after take: at 2 s one retry on the same bid; at
// 21 s or 25 s exactly one POST, reconcile (3 lists), router_transient 4 s.
func windowCase(t *testing.T, stage string, slow time.Duration) {
	t.Run("fast", func(t *testing.T) { sameBidRetry(t, rl429(stage), 2*time.Second) })
	t.Run("slow", func(t *testing.T) {
		c, lr, p, _ := rpcEnv(t)
		lr.setOpenFault(func(string) (string, bool) {
			c.Advance(slow)
			return rl429(stage), false
		})
		lists := lr.countLists()
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, 4*time.Second, 4*time.Second)
		if n := len(lr.bidsTried()); n != 1 {
			t.Fatalf("open POSTs = %d, want 1 (maybe sent: no reopen)", n)
		}
		// pre-open scan (1) + reconcile (≤3)
		if n := lists(); n < 2 || n > 1+reconcilePolls {
			t.Fatalf("session lists = %d, want the pre-open scan plus 1..%d reconcile polls", n, reconcilePolls)
		}
		assertNoCooling(t, p, modelB)
	})
}

func TestBuild429AfterWindowIsMaybeSent(t *testing.T)  { windowCase(t, stageBuild, 21*time.Second) }
func TestTxOptsAfterWindowIsMaybeSent(t *testing.T)    { windowCase(t, stageTxOpts, 25*time.Second) }
func TestGasPricesAfterWindowIsMaybeSent(t *testing.T) { windowCase(t, stageGas, 25*time.Second) }

// lateAdopt: the attempt's session lands after the request gave up; the
// next Acquire's pre-open scan adopts it, with no second open.
func lateAdopt(t *testing.T, c *fakeClock, lr *leaseRouter, p *Pool, opensBefore int) {
	t.Helper()
	lr.setOpenFault(nil)
	lr.set(func(lr *leaseRouter) { lr.openHang = false })
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if n := len(lr.bidsTried()); n != opensBefore {
		t.Fatalf("open POSTs = %d, want %d (the late session is adopted)", n, opensBefore)
	}
	if res.SessionID == "" {
		t.Fatal("no session")
	}
	if got := lr.openSet(); len(got) != 1 || got[0] != res.SessionID {
		t.Fatalf("on chain %v, leased %s", got, res.SessionID)
	}
}

// TestOpen5xxIsMaybeSent: a 502/503/504 on the open is maybe-sent: one
// POST, ≤3 lists, router_transient 4 s, then the late session is adopted.
func TestOpen5xxIsMaybeSent(t *testing.T) {
	for _, code := range []int{502, 503, 504} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, lr, p, _ := rpcEnv(t)
			lr.set(func(lr *leaseRouter) { lr.openStatus, lr.hideLanded = code, 99 })
			lists := lr.countLists()
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			assertTransient(t, err, 4*time.Second, 4*time.Second)
			if n := len(lr.bidsTried()); n != 1 {
				t.Fatalf("open POSTs = %d, want 1", n)
			}
			if n := lists(); n > 1+reconcilePolls {
				t.Fatalf("lists = %d, want ≤ %d", n, 1+reconcilePolls)
			}
			// The session becomes visible; the window clears on the match
			// and the next Acquire adopts it.
			lr.set(func(lr *leaseRouter) { lr.hideLeft, lr.hideLanded, lr.openStatus = 0, 0, 0 })
			c.Advance(4 * time.Second)
			lateAdopt(t, c, lr, p, 1)
		})
	}
}

// TestOpenDeadlineIsMaybeSent: the open POST hits Uplink's own deadline
// (the router keeps going and mints later): one POST, ≤3 lists,
// router_transient 4 s; the next Acquire adopts the late session.
func TestOpenDeadlineIsMaybeSent(t *testing.T) {
	scaleSlot(t, 200)
	c, lr, p, _ := rpcEnv(t)
	lr.set(func(lr *leaseRouter) { lr.openHang, lr.mintDelay = true, 3*time.Minute })
	lists := lr.countLists()
	_, _, err := p.Acquire(modelB, AcquireOpts{})
	assertTransient(t, err, 4*time.Second, 4*time.Second)
	if n := len(lr.bidsTried()); n != 1 {
		t.Fatalf("open POSTs = %d, want 1", n)
	}
	if n := lists(); n > 1+reconcilePolls {
		t.Fatalf("lists = %d, want ≤ %d", n, 1+reconcilePolls)
	}
	if !p.routerBusy() {
		t.Fatal("router-busy window not set by the cut open")
	}
	waitUntil(t, "late mint recorded", func() bool { lr.mu.Lock(); defer lr.mu.Unlock(); return lr.n == 1 })
	c.Advance(3*time.Minute + time.Second)
	lateAdopt(t, c, lr, p, 1)
	if p.routerBusy() {
		t.Fatal("router-busy window still set after the cut open was matched")
	}
}

// TestReceiptWait429Reconciled: the open tx landed but the receipt wait
// hit a 429; the session shows on reconcile poll 2 and is leased; no
// second open.
func TestReceiptWait429Reconciled(t *testing.T) {
	_, lr, p, sl := rpcEnv(t)
	lr.set(func(lr *leaseRouter) { lr.hideLanded = 1 }) // landed during the POST; hidden from reconcile poll 1
	lr.setOpenFault(faultBurst(1, routerErr(receiptWait429), true))
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if n := len(lr.bidsTried()); n != 1 {
		t.Fatalf("open POSTs = %d, want 1", n)
	}
	if got := lr.openSet(); len(got) != 1 || got[0] != res.SessionID {
		t.Fatalf("on chain %v, leased %q", got, res.SessionID)
	}
	if s := sl.sleeps(); len(s) != 1 || s[0] != reconcileGap {
		t.Fatalf("sleeps %v, want one %v gap (match on poll 2)", s, reconcileGap)
	}
	assertNoCooling(t, p, modelB)
}

// TestWindowRetryAtMostOnce: after a mock "already known" escalation, a
// fast P* gets one retry; a second fast P* in the same operation is
// maybe-sent; the late session is matched to a pending attempt.
func TestWindowRetryAtMostOnce(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	logs := captureLogs(t)
	lr.setOpenFault(faultSeq(rl429(stageNonce), rl429(stageBuild)))
	_, _, err := p.Acquire(modelB, AcquireOpts{})
	assertTransient(t, err, 4*time.Second, 4*time.Second)
	if n := len(lr.bidsTried()); n != 2 {
		t.Fatalf("open POSTs = %d, want 2 (one window retry, then maybe sent)", n)
	}
	// The first attempt's tx was in fact sent and mines late: it matches a
	// pending attempt, no shared-wallet warning.
	m, pv, _ := lr.bidOwner(bidID(modelB, leaseProv1))
	lr.addChainOn("0xlate0001", m, pv, c.Now().Add(-5*time.Second), c.Now().Add(time.Hour))
	c.Advance(10 * time.Second)
	p.ScanOrphans(context.Background())
	if got := pooledIDs(p); len(got) != 1 || got[0] != "0xlate0001" {
		t.Fatalf("pooled %v, want the late session adopted as ours", got)
	}
	if logs.count("wallet_shared_suspected") != 0 {
		t.Fatalf("shared-wallet warning for our own late session:\n%s", logs)
	}
}

// TestBlankRPCOneAttempt: without the operator's RPC (the router uses its
// own RPC list and retries), a pre-send chain-stage error of any kind is
// one open POST on the top bid, no backoff, no cooling, no walk, and an
// immediate router_transient with Retry-After 1..4 s. A fast P* is maybe
// sent (one POST, reconcile). With the operator's RPC set, the same
// pre-send error backs off on the same bid.
func TestBlankRPCOneAttempt(t *testing.T) {
	for _, body := range []string{rl429(stageBid), rl429(stageApprove), routerErr(stageBid + "context deadline exceeded")} {
		t.Run(body, func(t *testing.T) {
			_, lr, p, sl := rpcEnv(t)
			p.SetProvidedRPC(false)
			lr.setOpenFault(faultBurst(-1, body, false))
			_, _, err := p.Acquire(modelB, AcquireOpts{})
			assertTransient(t, err, time.Second, 4*time.Second)
			if n := len(lr.bidsTried()); n != 1 {
				t.Fatalf("open POSTs = %d, want 1", n)
			}
			assertOnlyBid(t, lr, bidID(modelB, leaseProv1))
			assertNoCooling(t, p, modelB)
			if s := sl.sleeps(); len(s) != 0 {
				t.Fatalf("sleeps %v, want none", s)
			}
		})
	}
	t.Run("fast window stage", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		p.SetProvidedRPC(false)
		lr.setOpenFault(faultSeq(rl429(stageGas)))
		_, _, err := p.Acquire(modelB, AcquireOpts{})
		assertTransient(t, err, time.Second, 4*time.Second)
		if n := len(lr.bidsTried()); n != 1 {
			t.Fatalf("open POSTs = %d, want 1 (maybe sent)", n)
		}
		assertNoCooling(t, p, modelB)
	})
	t.Run("provided", func(t *testing.T) {
		_, lr, p, _ := rpcEnv(t)
		lr.setOpenFault(faultSeq(rl429(stageGas)))
		if _, res := mustAcquire(t, p, modelB, AcquireOpts{}); !res.Opened {
			t.Fatalf("result %+v, want opened", res)
		}
		if n := len(lr.bidsTried()); n != 2 {
			t.Fatalf("open POSTs = %d, want 2 (one window retry)", n)
		}
	})
}

// TestProvidedRPCRead: only the presence is kept.
func TestProvidedRPCRead(t *testing.T) {
	p := New(nil, 600)
	if p.ProvidedRPC() {
		t.Fatal("default true, want false")
	}
	p.SetProvidedRPC(true)
	if !p.ProvidedRPC() {
		t.Fatal("not recorded")
	}
}
