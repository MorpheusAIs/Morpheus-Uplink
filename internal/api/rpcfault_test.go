package api

import (
	"strings"
	"testing"
)

// Router RPC rate limiting seen by a client. These pin the target
// behavior and FAIL on the current code until the fix lands: a transient
// chain RPC fault is retried on the same bid (no walk, no cool-down) and,
// once the retry cap is reached, the client gets a clean 503 with a
// Retry-After of at least 1 second.

// rpc429Open is the router's status-500 answer to an open when its chain
// RPC is rate limited (JSON-RPC error embedded; the id is made up).
const rpc429Open = `{"error":"failed to send transaction: open session failed: failed to build transaction: 429 Too Many Requests: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":429,\"message\":\"Your app has exceeded its compute units per second capacity. If you have retries enabled, you can safely ignore this message. If not, check out https://docs.alchemy.com/reference/throughput\"}}"}`

// Item (2): every open hits the RPC's 429. The request retries the same
// bid, then answers 503 no_available_provider with Retry-After >= 1; no
// walk to other bids and no provider cooled.
func TestRPC429RetryCapClean503(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	m := newFakeMarket(t, a40, b40, c40)
	m.openErrBody = rpc429Open
	e := newFailoverEnv(t, m, nil)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 600) || strings.Contains(body, "0x") {
		t.Errorf("got %d Retry-After %q %s", code, hdr.Get("Retry-After"), body)
	}
	opens, _ := m.snapshot()
	bids := map[string]bool{}
	for _, o := range opens {
		bids[o.bid] = true
	}
	if len(bids) != 1 || len(opens) < 2 {
		t.Errorf("opens %d on %d bids, want >= 2 retries of one bid (no walk)", len(opens), len(bids))
	}
	for _, p := range []string{a40, b40, c40} {
		if e.srv.pool.IsCooling(testModelID, p) {
			t.Errorf("provider %s cooling after RPC 429s", p[:6])
		}
	}
}

// Item (2), rated bids: when the rated-bids read keeps hitting the RPC's
// 429, the refusal is still a clean 503 with Retry-After >= 1 (today it
// is rated_unreadable with no Retry-After at all).
func TestRated429Clean503RetryAfter(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.ratedErrBody = `{"error":"Failed to call contract: 429 Too Many Requests"}`
	e := newFailoverEnv(t, m, nil)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 600) {
		t.Fatalf("got %d Retry-After %q %s", code, hdr.Get("Retry-After"), body)
	}
}
