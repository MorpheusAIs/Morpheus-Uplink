package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

// A2 (DETAIL §14.4 #1, #10): Retry-After is the shortest remaining
// cool-down among eligible providers, digits only; a configured-excluded
// provider's cool-down is ignored even when it is the shortest.
func TestA2RetryAfterShortestEligible(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	m := newFakeMarket(t, a40, b40, c40)
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.SetProviderExclude([]string{a40})
	e.srv.pool.SetProviderCooldown(30 * time.Second)
	e.srv.pool.MarkCooling(testModelID, a40) // excluded: ignored
	e.srv.pool.SetProviderCooldown(300 * time.Second)
	e.srv.pool.MarkCooling(testModelID, b40)
	e.srv.pool.SetProviderCooldown(500 * time.Second)
	e.srv.pool.MarkCooling(testModelID, c40)
	code, hdr, body := chatFull(t, e, "test-model")
	ra := hdr.Get("Retry-After")
	n, _ := strconv.Atoi(ra)
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(ra, 600) || n < 290 || n > 300 {
		t.Fatalf("got %d Retry-After %q %s", code, ra, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("opened %+v", opens)
	}

	// A cool-down longer than 600 s is capped at 600.
	m2 := newFakeMarket(t, a40)
	e2 := newFailoverEnv(t, m2, nil)
	e2.srv.pool.SetProviderCooldown(900 * time.Second)
	e2.srv.pool.MarkCooling(testModelID, a40)
	if _, hdr, _ := chatFull(t, e2, "test-model"); hdr.Get("Retry-After") != "600" {
		t.Fatalf("900 s cool-down: Retry-After %q, want 600", hdr.Get("Retry-After"))
	}
}

// A2 (DETAIL §14.4 #2): the one provider of a model just cut a stream: 503
// with no Retry-After (no cool-down with cool-down 0), nothing opened, and
// the refusal consumed the cut, so the next request opens normally.
func TestA2CutOnlyRefusedAndCleared(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ProviderCooldownSec = 0 })
	e.srv.pool.MarkStreamCut(testModelID, a40)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || hdr.Get("Retry-After") != "" || strings.Contains(body, "0x") {
		t.Fatalf("cut only: %d %q %s", code, hdr.Get("Retry-After"), body)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("cut only opened %+v", opens)
	}
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("after the cut was consumed: %d %s", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 1 || opens[0].bid != "" {
		t.Fatalf("want one normal open, got %+v", opens)
	}
}

// A2 (DETAIL §14.4 #2, walk exhausted): with an avoided provider, a by-bid
// walk where every open fails (3 tried) is 503 no_available_provider with
// Retry-After (the tried providers are now cooling), never a plain open.
func TestA2BidWalkExhausted(t *testing.T) {
	a40, b40, c40, d40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40), "0x"+strings.Repeat("d", 40)
	m := newFakeMarket(t, a40, b40, c40, d40)
	m.openFail = map[string]bool{b40: true, c40: true, d40: true}
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.MarkCooling(testModelID, a40)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, "no_available_provider") || !retryAfterOK(hdr.Get("Retry-After"), 600) || strings.Contains(body, "0x") {
		t.Fatalf("got %d %q %s", code, hdr.Get("Retry-After"), body)
	}
	opens, _ := m.snapshot()
	if len(opens) != 3 {
		t.Fatalf("want 3 by-bid attempts, got %+v", opens)
	}
	for _, o := range opens {
		if o.bid == "" || o.provider == a40 {
			t.Fatalf("open %+v: plain or on the cooling provider", o)
		}
	}
}

// A2 (DETAIL §14.4 #12): refusals are audited as provider_unavailable on
// the ops stream, one line per resolved model per minute (the repeats are
// counted), labelled by the catalog, never by the request's model string.
func TestA2ProviderUnavailableAudit(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.MarkCooling(testModelID, a40)
	for i := 0; i < 3; i++ {
		if code, _, body := chatFull(t, e, testModelID); code != 503 {
			t.Fatalf("refusal %d: %d %s", i, code, body)
		}
	}
	var lines []AuditEvent
	for _, ev := range e.srv.audit.Recent(1000) {
		if ev.Kind == "provider_unavailable" {
			lines = append(lines, ev)
		}
	}
	if len(lines) != 1 || lines[0].Result != "no_available" || lines[0].Stream != AuditStreamOps {
		t.Fatalf("audit lines %+v", lines)
	}
	if strings.Contains(lines[0].Message, testModelID) || !strings.Contains(lines[0].Message, e.srv.modelLabel(testModelID)) {
		t.Fatalf("audit message %q", lines[0].Message)
	}
	if !opsKinds["provider_unavailable"] {
		t.Fatal("provider_unavailable is not an ops kind")
	}
}
