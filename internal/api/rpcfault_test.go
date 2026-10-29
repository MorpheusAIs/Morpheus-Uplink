package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
)

// Router RPC rate limiting seen by a client. These pin the target
// behavior and FAIL on the current code until the fix lands: a transient
// chain RPC fault is retried on the same bid (5 attempts with backoff, no
// walk, no cool-down) and, once the retry cap is reached, the client gets
// a clean 503 with a Retry-After of 1..4 seconds (never 0). A maybe-sent
// open is reconciled in the request and finishes even when the client
// goes away.

// rpc429Open is the router's status-500 answer to an open when its chain
// RPC is rate limited (JSON-RPC error embedded; the id is made up).
const rpc429Open = `{"error":"failed to send transaction: open session failed: failed to build transaction: 429 Too Many Requests: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":429,\"message\":\"Your app has exceeded its compute units per second capacity. If you have retries enabled, you can safely ignore this message. If not, check out https://docs.alchemy.com/reference/throughput\"}}"}`

// noSleep routes the pool's sleep seam to a recorder (no real waits).
func noSleep(t *testing.T) func() []time.Duration {
	var mu sync.Mutex
	var got []time.Duration
	t.Cleanup(pool.SetSleep(func(d time.Duration) {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
	}))
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), got...)
	}
}

// Items 1-2: every open hits the RPC's 429. The request makes exactly 5
// attempts on one bid, then answers 503 no_available_provider with
// Retry-After 1..4; no walk to other bids and no provider cooled.
func TestRPC429RetryCapClean503(t *testing.T) {
	noSleep(t)
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	m := newFakeMarket(t, a40, b40, c40)
	m.openErrBody = rpc429Open
	e := newFailoverEnv(t, m, nil)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 4) || strings.Contains(body, "0x") {
		t.Errorf("got %d Retry-After %q %s (want 503, Retry-After 1..4)", code, hdr.Get("Retry-After"), body)
	}
	opens, _ := m.snapshot()
	bids := map[string]bool{}
	for _, o := range opens {
		bids[o.bid] = true
	}
	if len(bids) != 1 || len(opens) != 5 {
		t.Errorf("opens %d on %d bids, want exactly 5 attempts on one bid (no walk)", len(opens), len(bids))
	}
	for _, p := range []string{a40, b40, c40} {
		if e.srv.pool.IsCooling(testModelID, p) {
			t.Errorf("provider %s cooling after RPC 429s", p[:6])
		}
	}
}

// Items 2 and 5, rated bids: when the rated-bids read keeps hitting the
// RPC's 429, the refusal is still a clean 503 with Retry-After 1..4
// (today it is rated_unreadable with no Retry-After at all).
func TestRated429Clean503RetryAfter(t *testing.T) {
	noSleep(t)
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.ratedErrBody = `{"error":"Failed to call contract: 429 Too Many Requests"}`
	e := newFailoverEnv(t, m, nil)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 4) {
		t.Fatalf("got %d Retry-After %q %s (want 503, Retry-After 1..4)", code, hdr.Get("Retry-After"), body)
	}
}

// (d) The client cancels while its open is at the router; the tx lands
// but the receipt wait fails (maybe-sent). The open and the reconcile
// still finish: the landed session is adopted into the pool, there is no
// second open, and nothing is closed.
func TestOpenAndReconcileOutliveClientCancel(t *testing.T) {
	noSleep(t)
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	m := newFakeMarket(t, a40, b40)
	m.openLandBody = `{"error":"failed to send transaction: parse session receipt failed: failed to wait for confirmations 429 Too Many Requests"}`
	m.openArrived = make(chan struct{}, 16)
	m.openGate = make(chan struct{})
	e := newFailoverEnv(t, m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-m.openArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("open never reached the market")
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // let the cancel reach the server
	close(m.openGate)
	pooled := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !pooled; time.Sleep(20 * time.Millisecond) {
		for _, s := range e.srv.pool.Snapshot() {
			pooled = pooled || strings.HasPrefix(s.SessionID, "0xsess01")
		}
	}
	if !pooled {
		t.Errorf("landed session not adopted after the client cancelled; pool: %s", e.pooled())
	}
	time.Sleep(200 * time.Millisecond)
	opens, closes := m.snapshot()
	if len(opens) != 1 {
		t.Errorf("opens = %d, want 1 (maybe-sent: never re-opened)", len(opens))
	}
	if len(closes) != 0 {
		t.Errorf("closes = %v, want none", closes)
	}
}
