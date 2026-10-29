package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Router RPC rate limiting seen by a client. These pin the target
// behavior: a transient chain RPC fault is retried on the same bid (no
// walk, no cool-down) and, once the retry cap is reached, the client gets
// a clean 503 with a Retry-After of 1..4 seconds (never 0). A maybe-sent
// open is reconciled in the request and finishes even when the client
// goes away.

// rpc429Open is the router's status-500 answer to an open when its chain
// RPC is rate limited (JSON-RPC error embedded; the id is made up).
const rpc429Open = `{"error":"failed to send transaction: open session failed: failed to build transaction: 429 Too Many Requests: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":429,\"message\":\"Your app has exceeded its compute units per second capacity. If you have retries enabled, you can safely ignore this message. If not, check out https://docs.alchemy.com/reference/throughput\"}}"}`

// noSleep routes the pool's sleep seam to a recorder (no real waits).
func noSleep(t *testing.T, e *failoverEnv) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var got []time.Duration
	e.srv.pool.SetSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return ctx.Err()
	})
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), got...)
	}
}

// rpc429Bid is the bid-read stage (pre-send: no transaction yet) hitting
// the RPC's 429.
const rpc429Bid = `{"error":"failed to get bid: 429 Too Many Requests: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":429,\"message\":\"Your app has exceeded its compute units per second capacity.\"}}"}`

// Items 1-2: every open hits the RPC's 429. A pre-send stage is retried on
// the same bid up to the retry cap (5 attempts); a build-transaction stage
// (pre-send only inside the proof window, one RPC URL) is retried once.
// Either way the client gets 503 no_available_provider with Retry-After
// 1..4; no walk to other bids and no provider cooled.
func TestRPC429RetryCapClean503(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		opens int
	}{
		{"pre-send stage", rpc429Bid, 5},
		{"window stage", rpc429Open, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
			m := newFakeMarket(t, a40, b40, c40)
			m.openErrBody = tc.body
			m.rpcURLs = 1 // one RPC URL: a window-stage 429 may be pre-send
			e := newFailoverEnv(t, m, nil)
			noSleep(t, e)
			if n := e.srv.pool.RefreshRPCURLCount(context.Background()); n != 1 {
				t.Fatalf("RPC URL count %d, want 1", n)
			}
			code, hdr, body := chatFull(t, e, "test-model")
			if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 4) || strings.Contains(body, "0x") {
				t.Errorf("got %d Retry-After %q %s (want 503, Retry-After 1..4)", code, hdr.Get("Retry-After"), body)
			}
			opens, _ := m.snapshot()
			bids := map[string]bool{}
			for _, o := range opens {
				bids[o.bid] = true
			}
			if len(bids) != 1 || len(opens) != tc.opens {
				t.Errorf("opens %d on %d bids, want exactly %d attempts on one bid (no walk)", len(opens), len(bids), tc.opens)
			}
			for _, p := range []string{a40, b40, c40} {
				if e.srv.pool.IsCooling(testModelID, p) {
					t.Errorf("provider %s cooling after RPC 429s", p[:6])
				}
			}
		})
	}
}

// Items 2 and 5, rated bids: when the rated-bids read keeps hitting the
// RPC's 429, the refusal is still a clean 503 with Retry-After 1..4
// (today it is rated_unreadable with no Retry-After at all).
func TestRated429Clean503RetryAfter(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.ratedErrBody = `{"error":"Failed to call contract: 429 Too Many Requests"}`
	e := newFailoverEnv(t, m, nil)
	noSleep(t, e)
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
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	m := newFakeMarket(t, a40, b40)
	m.openLandBody = `{"error":"failed to send transaction: parse session receipt failed: failed to wait for confirmations 429 Too Many Requests"}`
	m.openArrived = make(chan struct{}, 16)
	m.openGate = make(chan struct{})
	e := newFailoverEnv(t, m, nil)
	noSleep(t, e)
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

// Parallel lanes through the API (the live gate's burst): 8 concurrent
// conversations on one model while the first opens hit the RPC's 429.
// Every answer is 200 or a typed 503 with Retry-After 1..4, never a 502;
// one session serves them all and no provider is cooled.
func TestParallelLanesNever502(t *testing.T) {
	for _, lanes := range []int{5, 6, 8} {
		t.Run(strconv.Itoa(lanes), func(t *testing.T) {
			a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
			m := newFakeMarket(t, a40, b40)
			m.openErrBody, m.openErrN = rpc429Bid, 2
			m.rpcURLs = 1
			e := newFailoverEnv(t, m, nil)
			noSleep(t, e)
			var wg sync.WaitGroup
			codes := make([]int, lanes)
			ras := make([]string, lanes)
			for i := range lanes {
				wg.Add(1)
				go func() {
					defer wg.Done()
					code, hdr, _ := chatFull(t, e, "test-model")
					codes[i], ras[i] = code, hdr.Get("Retry-After")
				}()
			}
			wg.Wait()
			for i, c := range codes {
				if c != 200 && !(c == 503 && retryAfterOK(ras[i], 4)) {
					t.Errorf("lane %d: %d Retry-After %q (want 200 or 503 with Retry-After 1..4)", i, c, ras[i])
				}
			}
			opens, closes := m.snapshot()
			bids := map[string]bool{}
			for _, o := range opens {
				bids[o.bid] = true
			}
			if len(bids) != 1 || len(opens) != 3 || len(closes) != 0 {
				t.Errorf("opens %d on %d bids, closes %v: want 2 retried 429s and one session", len(opens), len(bids), closes)
			}
			for _, p := range []string{a40, b40} {
				if e.srv.pool.IsCooling(testModelID, p) {
					t.Errorf("provider %s cooling", p[:6])
				}
			}
		})
	}
}
