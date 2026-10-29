package api

import (
	"net/http"
	"strings"
	"testing"
)

// The router's answers to an open when the wallet lacks funds, before the
// open transaction is sent (proxy-router v7.18.0 classifyOpenSessionError
// wording).
const (
	fundsApproveETH = `{"error":"failed to send transaction: approve MOR allowance failed: insufficient ETH for gas — add ETH to your wallet to cover transaction fees. Original: insufficient funds for gas * price + value"}`
	fundsOpenMOR    = `{"error":"failed to send transaction: open session failed: insufficient MOR balance — your wallet does not have enough MOR to stake for this session length. Original: execution reverted: ERC20: transfer amount exceeds balance"}`
)

func assertWalletFunds402(t *testing.T, code int, hdr http.Header, body string) {
	t.Helper()
	if code != http.StatusPaymentRequired || !strings.Contains(body, `"type":"wallet_insufficient_funds"`) ||
		!strings.Contains(body, "The Uplink wallet needs funds (gas, MOR or allowance) to open a session") {
		t.Fatalf("got %d %s, want 402 wallet_insufficient_funds", code, body)
	}
	if v := hdr.Get("Retry-After"); v != "" {
		t.Fatalf("Retry-After %q, want none", v)
	}
	if strings.Contains(body, "0x") || strings.Contains(body, "Original") {
		t.Fatalf("body leaks router detail: %s", body)
	}
}

// A funding error before the send: one open, a clear 402 at once (not a
// 429 or 503, no Retry-After), no retry, no walk, no cool-down.
func TestWalletNeedsFunds402(t *testing.T) {
	for _, body := range []string{fundsApproveETH, fundsOpenMOR} {
		m := newFakeMarket(t, provA, provB)
		m.openErrBody = body
		e := newFailoverEnv(t, m, nil)
		sleeps := noSleep(t, e)
		code, hdr, out := chatFull(t, e, "test-model")
		assertWalletFunds402(t, code, hdr, out)
		opens, _ := m.snapshot()
		if len(opens) != 1 {
			t.Fatalf("opens %d, want exactly 1", len(opens))
		}
		if opens[0].body["rejectExisting"] != true {
			t.Fatalf("first open body %v, want rejectExisting true", opens[0].body)
		}
		if s := sleeps(); len(s) != 0 {
			t.Fatalf("backoff sleeps %v, want none", s)
		}
		for _, p := range []string{provA, provB} {
			if e.srv.pool.IsCooling(testModelID, p) {
				t.Fatalf("provider %s cooling after a funding error", p[:6])
			}
		}
	}
}

// The failover reopen hits a funding error: the client gets the same 402
// (not the first provider's error), audited wallet_funds; the failover
// open did not send rejectExisting.
func TestFailoverWalletNeedsFunds402(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.openErrBody = fundsApproveETH
		m.mu.Unlock()
		pme402(n, w)
	}
	e := newFailoverEnv(t, m, nil)
	code, hdr, out := chatFull(t, e, "test-model")
	assertWalletFunds402(t, code, hdr, out)
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "wallet_funds" {
		t.Fatalf("audit %v, want [wallet_funds]", a)
	}
	opens, _ := m.snapshot()
	if len(opens) != 2 || opens[0].body["rejectExisting"] != true || opens[1].body["rejectExisting"] != false {
		t.Fatalf("opens %+v, want the first flagged and the failover open not", opens)
	}
}
