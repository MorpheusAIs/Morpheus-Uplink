package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The router's answers to an open when the chain is slow to include the
// wallet's transactions (proxy-router v7.18.0 wording, hash redacted).
const (
	openUnderpriced = `{"error":"failed to send transaction: open session failed: failed to send transaction: replacement transaction underpriced"}`
	openNotMined    = `{"error":"failed to send transaction: open session failed: transaction could not be confirmed on-chain — the network may be congested, try again shortly. Original: transaction not mined after all escalation attempts: last tx 0x0000000000000000000000000000000000000000000000000000000000000abc"}`
)

// A chain-wait open error: a typed 503 with its own type and fixed
// message, Retry-After 30 (not the 4 s router_transient cap) and the same
// retry_after in the body; nothing from the router text; one open, no
// walk, no cool-down; audited with the reason.
func TestChainWaitTyped503(t *testing.T) {
	for _, tc := range []struct {
		body, typ, msg string
	}{
		{openUnderpriced, "wallet_tx_pending", "The Uplink wallet has a transaction still pending on-chain; retry shortly"},
		{openNotMined, "chain_congested", "The Base network is congested and the session could not be confirmed yet; retry shortly"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
			m := newFakeMarket(t, a40, b40)
			m.openErrBody = tc.body
			e := newFailoverEnv(t, m, nil)
			noSleep(t, e)
			code, hdr, out := chatFull(t, e, "test-model")
			if code != http.StatusServiceUnavailable {
				t.Fatalf("got %d %s, want 503", code, out)
			}
			if v := hdr.Get("Retry-After"); v != "30" {
				t.Fatalf("Retry-After %q, want 30", v)
			}
			var got struct {
				Error struct {
					Type       string `json:"type"`
					Code       int    `json:"code"`
					Message    string `json:"message"`
					RetryAfter int    `json:"retry_after"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("body %q: %v", out, err)
			}
			if got.Error.Type != tc.typ || got.Error.Code != 503 || got.Error.Message != tc.msg || got.Error.RetryAfter != 30 {
				t.Fatalf("body %s, want type %s, code 503, retry_after 30", out, tc.typ)
			}
			for _, leak := range []string{"0x", "Original", "underpriced", "not mined", "escalation"} {
				if strings.Contains(out, leak) {
					t.Fatalf("body leaks router detail %q: %s", leak, out)
				}
			}
			opens, _ := m.snapshot()
			if len(opens) != 1 {
				t.Fatalf("opens %d, want exactly 1 (maybe sent: never re-opened, no walk)", len(opens))
			}
			for _, p := range []string{a40, b40} {
				if e.srv.pool.IsCooling(testModelID, p) {
					t.Fatalf("provider %s cooling after a chain-wait error", p[:6])
				}
			}
			var outcomes []string
			for _, ev := range e.srv.audit.Recent(1000) {
				if ev.Kind == "provider_unavailable" {
					outcomes = append(outcomes, ev.Result)
				}
			}
			if len(outcomes) != 1 || outcomes[0] != tc.typ {
				t.Fatalf("audit outcomes %v, want [%s]", outcomes, tc.typ)
			}
		})
	}
}

// Regression: a maybe-sent open with no chain-wait text stays
// no_available_provider with Retry-After 1..4 and no retry_after field.
func TestRouterTransientStaysNoAvailable(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.openErrBody = `{"error":"failed to send transaction: open session failed: failed to send transaction: already known"}`
	e := newFailoverEnv(t, m, nil)
	noSleep(t, e)
	code, hdr, out := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(out, `"type":"no_available_provider"`) || !retryAfterOK(hdr.Get("Retry-After"), 4) || strings.Contains(out, "retry_after") {
		t.Fatalf("got %d Retry-After %q %s, want 503 no_available_provider, Retry-After 1..4", code, hdr.Get("Retry-After"), out)
	}
	if _, ok := statusChainWait(t, e); ok {
		t.Fatal("status shows chainWait after a router_transient refusal")
	}
}

// statusChainWait reads /admin/status and returns its chainWait reason.
func statusChainWait(t *testing.T, e *failoverEnv) (string, bool) {
	t.Helper()
	code, body := e.admin(t, http.MethodGet, "/admin/status", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/status %d %s", code, body)
	}
	var st struct {
		ChainWait *struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		} `json:"chainWait"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("status %q: %v", body, err)
	}
	if st.ChainWait == nil {
		return "", false
	}
	return st.ChainWait.Reason, true
}

// The status page shows the reason after a chain-wait refusal (only
// reason and time) and drops it after the next successful open.
func TestStatusChainWait(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.openErrBody, m.openErrN = openUnderpriced, 1
	e := newFailoverEnv(t, m, nil)
	noSleep(t, e)
	if code, _, out := chatFull(t, e, "test-model"); code != 503 {
		t.Fatalf("first call %d %s, want 503", code, out)
	}
	if r, ok := statusChainWait(t, e); !ok || r != "wallet_tx_pending" {
		t.Fatalf("chainWait %q %v, want wallet_tx_pending", r, ok)
	}
	_, body := e.admin(t, http.MethodGet, "/admin/status", "")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &raw)
	var cw map[string]any
	_ = json.Unmarshal(raw["chainWait"], &cw)
	if len(cw) != 2 || cw["reason"] == nil || cw["at"] == nil {
		t.Fatalf("chainWait %s, want only reason and at", raw["chainWait"])
	}
	if code, _, out := chatFull(t, e, "test-model"); code != 200 {
		t.Fatalf("second call %d %s, want 200", code, out)
	}
	if _, ok := statusChainWait(t, e); ok {
		t.Fatal("chainWait still shown after a successful open")
	}
}
