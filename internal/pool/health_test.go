package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const (
	leaseProv3 = "0x4444444444444444444444444444444444444444"
	leaseProv4 = "0x5555555555555555555555555555555555555555"
	leaseProv5 = "0x6666666666666666666666666666666666666666"
)

func (lr *leaseRouter) paths() (byModel int, byBid []string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.byModel, append([]string(nil), lr.byBid...)
}

func noOpens(t *testing.T, lr *leaseRouter, what string) {
	t.Helper()
	if byModel, byBid := lr.paths(); byModel != 0 || len(byBid) != 0 {
		t.Fatalf("%s: by model %d, by bid %v", what, byModel, byBid)
	}
}

// TestOpenAlwaysByBid: no exclusion, nothing cooling: the open is by bid on the top healthy
// bid; there is never an open by model.
func TestOpenAlwaysByBid(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	id, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv1 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv1)
	}
	if id == "" {
		t.Fatal("empty session id")
	}
	byModel, byBid := lr.paths()
	if len(byBid) != 1 || byBid[0] != bidID(modelB, leaseProv1) {
		t.Fatalf("byBid = %v, want [%s]", byBid, bidID(modelB, leaseProv1))
	}
	if byModel != 0 {
		t.Fatalf("byModel = %d, want 0", byModel)
	}
}

// TestOpenKeepsRouterRatedOrder: the router's rated (Score) order wins; the pool never re-sorts.
func TestOpenKeepsRouterRatedOrder(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	b1, b2 := bidID(modelB, leaseProv1), bidID(modelB, leaseProv2)
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{
		ratedRow(modelB, b1, leaseProv1, 5),
		ratedRow(modelB, b2, leaseProv2, 9),
	}
	lr.health[modelB] = []string{b1, b2}
	lr.mu.Unlock()
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
}

// TestEqualScoresKeepRouterOrder: equal scores keep the router's response order.
func TestEqualScoresKeepRouterOrder(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	b1, b2 := bidID(modelB, leaseProv1), bidID(modelB, leaseProv2)
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{
		ratedRow(modelB, b1, leaseProv1, 5),
		ratedRow(modelB, b2, leaseProv2, 5),
	}
	lr.health[modelB] = []string{b1, b2}
	lr.mu.Unlock()
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv1 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv1)
	}
}

// TestUnhealthyRatedBidsSkipped: a bid that is not healthy (degraded, or absent
// from the health set) is skipped.
func TestUnhealthyRatedBidsSkipped(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.setUnhealthy(modelB, leaseProv1)
	// a model whose health set lacks a bid is skipped: health[modelB] only has leaseProv2
	lr.mu.Lock()
	lr.health[modelB] = []string{bidID(modelB, leaseProv2)}
	lr.mu.Unlock()
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
}

// TestAllUnhealthyRefuses: no healthy bid → ErrNoAvailableProvider and zero opens of any kind.
func TestAllUnhealthyRefuses(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.setUnhealthy(modelB, leaseProv1)
	lr.setUnhealthy(modelB, leaseProv2)
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoAvailableProvider) {
		t.Fatalf("err = %v, want ErrNoAvailableProvider", err)
	}
	byModel, byBid := lr.paths()
	if byModel != 0 || len(byBid) != 0 {
		t.Fatalf("opens: byModel=%d byBid=%v, want 0 and empty", byModel, byBid)
	}
}

// TestHealthUnknownRefuses: a failing health source refuses; zero opens.
func TestHealthUnknownRefuses(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.mu.Lock()
	lr.healthErr = errors.New("x")
	lr.mu.Unlock()
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoAvailableProvider) {
		t.Fatalf("err = %v, want ErrNoAvailableProvider", err)
	}
	byModel, byBid := lr.paths()
	if byModel != 0 || len(byBid) != 0 {
		t.Fatalf("opens: byModel=%d byBid=%v, want 0 and empty", byModel, byBid)
	}
}

// TestNoHealthSourceRefuses: no health source wired refuses (health_unknown).
func TestNoHealthSourceRefuses(t *testing.T) {
	_, p := newLeaseRouter(t, newFakeClock())
	p.SetHealthyBids(nil)
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoAvailableProvider) {
		t.Fatalf("err = %v, want ErrNoAvailableProvider", err)
	}
}

// TestHealthPlusExclude: healthy {A,B} with A excluded opens B.
func TestHealthPlusExclude(t *testing.T) {
	_, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderExclude([]string{leaseProv1})
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
}

// TestHealthyOnlyExcludedRefusesNoAvailable: A healthy but excluded, B unhealthy →
// no_available (not no_eligible: B is not excluded).
func TestHealthyOnlyExcludedRefusesNoAvailable(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.setUnhealthy(modelB, leaseProv2)
	p.SetProviderExclude([]string{leaseProv1})
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoAvailableProvider) {
		t.Fatalf("err = %v, want ErrNoAvailableProvider", err)
	}
	if errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("err = %v, should NOT be ErrNoEligibleProvider", err)
	}
}

// TestFullyExcludedModelNoEligible: every rated provider excluded → ErrNoEligibleProvider,
// whatever the health (even when the source fails).
func TestFullyExcludedModelNoEligible(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderExclude([]string{leaseProv1, leaseProv2})
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("err = %v, want ErrNoEligibleProvider", err)
	}
	lr.mu.Lock()
	lr.healthErr = errors.New("x")
	lr.mu.Unlock()
	_, _, err = p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("err = %v, want ErrNoEligibleProvider (health error ignored when all excluded)", err)
	}
}

// TestEmptyRatedBidsRefusesNoAvailable: an empty rated list refuses with
// ErrNoAvailableProvider, with or without exclusions; the refusal consumes a
// pending stream cut.
func TestEmptyRatedBidsRefusesNoAvailable(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.mu.Lock()
	lr.bidders[modelB] = []string{}
	lr.mu.Unlock()
	p.MarkStreamCut(modelB, leaseProv1)
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if nae := (*NoAvailableError)(nil); !errors.As(err, &nae) {
		t.Fatalf("err = %v, want *NoAvailableError", err)
	}
	if p.pendingCut(modelB) != "" {
		t.Fatalf("pending cut not cleared, = %q", p.pendingCut(modelB))
	}
	p.SetProviderExclude([]string{leaseProv1})
	p.MarkStreamCut(modelB, leaseProv1)
	_, _, err = p.openAvoidingCooling(modelB, 600)
	if nae := (*NoAvailableError)(nil); !errors.As(err, &nae) {
		t.Fatalf("err = %v, want *NoAvailableError", err)
	}
	if p.pendingCut(modelB) != "" {
		t.Fatalf("pending cut not cleared, = %q", p.pendingCut(modelB))
	}
}

// TestRouterUnreachableLogsReason: a closed router port refuses with
// ErrNoAvailableProvider and logs reason router_unreachable.
func TestRouterUnreachableLogsReason(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	p := New(router.New("http://127.0.0.1:1", "u", "p"), 600)
	p.SetHealthyBids(func(string) (map[string]struct{}, error) {
		return map[string]struct{}{"0xabc": {}}, nil
	})
	_, _, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoAvailableProvider) {
		t.Fatalf("err = %v, want ErrNoAvailableProvider", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reason=router_unreachable") {
		t.Fatalf("log missing router_unreachable: %s", out)
	}
	if strings.Contains(out, "rated_unreadable") {
		t.Fatalf("log has rated_unreadable: %s", out)
	}
}

// TestUnusableRatedRowsIgnored: rated rows without a usable bid id or provider are
// dropped; the valid row is opened.
func TestUnusableRatedRowsIgnored(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	b2 := bidID(modelB, leaseProv2)
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{
		ratedRow(modelB, b2, leaseProv2, 5),
		ratedRow(modelB, zeroHash, leaseProv1, 4),
		ratedRow(modelB, "0xabc", "0xabc", 3),
	}
	lr.mu.Unlock()
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
}

// TestBidWalkCapsAtThree: five healthy providers that all fail: three tries,
// three cooled, a refusal with Retry-After.
func TestBidWalkCapsAtThree(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(time.Minute)
	fake := func(i int) string { return "0x" + strings.Repeat(string(rune('a'+i)), 64) }
	bids := []string{fake(0), fake(1), fake(2), fake(3), fake(4)}
	provs := []string{leaseProv1, leaseProv2, leaseProv3, leaseProv4, leaseProv5}
	rows := make([]map[string]any, 0, 5)
	for i := range bids {
		rows = append(rows, ratedRow(modelB, bids[i], provs[i], float64(10-i)))
	}
	lr.mu.Lock()
	lr.rows[modelB] = rows
	lr.health[modelB] = bids
	lr.mu.Unlock()
	_, _, err := p.openAvoidingCooling(modelB, 600)
	var nae *NoAvailableError
	if !errors.As(err, &nae) || nae.RetryAfter <= 0 {
		t.Fatalf("err = %v, want *NoAvailableError with RetryAfter>0", err)
	}
	byModel, byBid := lr.paths()
	if byModel != 0 || len(byBid) != 3 {
		t.Fatalf("opens: byModel=%d byBid=%v, want 0 and len 3", byModel, byBid)
	}
	for _, pr := range provs[:3] {
		if !p.IsCooling(modelB, pr) {
			t.Fatalf("%s not cooling, want cooling", pr)
		}
	}
	for _, pr := range provs[3:] {
		if p.IsCooling(modelB, pr) {
			t.Fatalf("%s cooling, want not cooling", pr)
		}
	}
}

// TestBidWalkOneTryPerProvider: one try per provider: a failed provider's second
// bid is not tried.
func TestBidWalkOneTryPerProvider(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	fake1 := "0x" + strings.Repeat("a", 64)
	fake2 := "0x" + strings.Repeat("b", 64)
	real := bidID(modelB, leaseProv2)
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{
		ratedRow(modelB, fake1, leaseProv1, 10),
		ratedRow(modelB, fake2, leaseProv1, 9),
		ratedRow(modelB, real, leaseProv2, 8),
	}
	lr.health[modelB] = []string{fake1, fake2, real}
	lr.mu.Unlock()
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
	_, byBid := lr.paths()
	if len(byBid) != 2 {
		t.Fatalf("byBid = %v, want exactly 2 entries", byBid)
	}
}

// TestCooldownKeptWithHealth: a cooling provider is skipped among the healthy
// bids; once its cooldown ends it is opened again, by bid.
func TestCooldownKeptWithHealth(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(50 * time.Millisecond) // cooling runs on the wall clock
	p.MarkCooling(modelB, leaseProv1)
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
	time.Sleep(80 * time.Millisecond)
	_, prov, err = p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv1 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv1)
	}
	byModel, _ := lr.paths()
	if byModel != 0 {
		t.Fatalf("byModel = %d, want 0", byModel)
	}
}

// TestRetryAfterIgnoresUnhealthyAndExcluded: Retry-After counts only healthy,
// non-excluded providers that are cooling.
func TestRetryAfterIgnoresUnhealthyAndExcluded(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.setUnhealthy(modelB, leaseProv1)
	p.SetProviderCooldown(time.Minute)
	p.MarkCooling(modelB, leaseProv1)
	p.MarkCooling(modelB, leaseProv3)
	p.SetProviderCooldown(9 * time.Minute)
	p.MarkCooling(modelB, leaseProv2)
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv1, leaseProv2, leaseProv3}
	lr.mu.Unlock()
	p.SetProviderExclude([]string{leaseProv3})
	_, _, err := p.openAvoidingCooling(modelB, 600)
	var nae *NoAvailableError
	if !errors.As(err, &nae) {
		t.Fatalf("err = %v, want *NoAvailableError", err)
	}
	if nae.RetryAfter <= 8*time.Minute {
		t.Fatalf("RetryAfter = %v, want > 8m", nae.RetryAfter)
	}
}

// TestFailoverReopenUsesFilteredList: the failover reopen walks only healthy,
// non-avoided bids.
func TestFailoverReopenUsesFilteredList(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv1, leaseProv2, leaseProv3}
	lr.mu.Unlock()
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	lr.setUnhealthy(modelB, leaseProv2)
	p.SetProviderCooldown(0)
	res, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil {
		t.Fatalf("failover: %v", err)
	}
	if res.Provider != leaseProv3 {
		t.Fatalf("provider = %s, want %s", res.Provider, leaseProv3)
	}
	byModel, _ := lr.paths()
	if byModel != 0 {
		t.Fatalf("byModel = %d, want 0", byModel)
	}
	res.Lease.Done()
	l.Done()
}

// TestFailoverNoHealthyAlternateKeepsSession: no healthy alternate →
// ErrNoAlternate and the session is kept.
func TestFailoverNoHealthyAlternateKeepsSession(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	lr.setUnhealthy(modelB, leaseProv2)
	_, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("err = %v, want ErrNoAlternate", err)
	}
	lr.mu.Lock()
	if lr.sessions[r.SessionID].closed {
		t.Fatal("session closed, want kept open")
	}
	lr.mu.Unlock()
	l.Done()
}

// TestFailoverHealthUnknownKeepsSession: health source failing at failover →
// ErrNoAlternate and the session is kept.
func TestFailoverHealthUnknownKeepsSession(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	lr.mu.Lock()
	lr.healthErr = errors.New("x")
	lr.mu.Unlock()
	_, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("err = %v, want ErrNoAlternate", err)
	}
	lr.mu.Lock()
	if lr.sessions[r.SessionID].closed {
		t.Fatal("session closed, want kept open")
	}
	lr.mu.Unlock()
	l.Done()
}

// TestMidStreamCutGuardrailUsesFilteredList: the mid-stream cut guardrail uses the
// healthy list; once an alternate is healthy the cut retires the session,
// records the cut and the next open lands elsewhere and clears it.
func TestMidStreamCutGuardrailUsesFilteredList(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	lr.setUnhealthy(modelB, leaseProv2)
	_, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: false})
	if !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("err = %v, want ErrNoAlternate", err)
	}
	lr.mu.Lock()
	if lr.sessions[r.SessionID].closed {
		t.Fatal("session closed, want kept open")
	}
	lr.mu.Unlock()
	// make leaseProv2 healthy again
	lr.mu.Lock()
	delete(lr.unhealthy, bidID(modelB, leaseProv2))
	lr.mu.Unlock()
	res, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: false})
	if err != nil {
		t.Fatalf("failover: %v", err)
	}
	if res.SessionID != "" || res.FailedProvider != leaseProv1 {
		t.Fatalf("cut result = %+v", res)
	}
	// The cut session is retired; its close lands once this request ends.
	l.Done()
	waitUntil(t, "cut session closed", func() bool { return lr.closedCount(r.SessionID) == 1 })
	if p.pendingCut(modelB) != leaseProv1 {
		t.Fatalf("pending cut = %q, want %s", p.pendingCut(modelB), leaseProv1)
	}
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv2 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv2)
	}
	if p.pendingCut(modelB) != "" {
		t.Fatalf("pending cut not cleared = %q", p.pendingCut(modelB))
	}
}

// TestBidMatchCaseInsensitive: bid ids and providers match case-insensitively
// (upper-hex bid id, mixed-case exclusion).
func TestBidMatchCaseInsensitive(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lower := bidID(modelB, leaseProv1)
	upper := "0x" + strings.ToUpper(lower[2:])
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{
		ratedRow(modelB, upper, leaseProv1, 10),
		ratedRow(modelB, bidID(modelB, leaseProv2), leaseProv2, 9),
	}
	lr.mu.Unlock()
	p.SetHealthyBids(func(modelID string) (map[string]struct{}, error) {
		if modelID == modelB {
			return map[string]struct{}{lower: {}}, nil
		}
		return nil, nil
	})
	p.SetProviderExclude([]string{mixedCase(leaseProv2)})
	_, prov, err := p.openAvoidingCooling(modelB, 600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if prov != leaseProv1 {
		t.Fatalf("provider = %s, want %s", prov, leaseProv1)
	}
	_, byBid := lr.paths()
	if len(byBid) != 1 || byBid[0] != lower {
		t.Fatalf("byBid = %v, want [%s]", byBid, lower)
	}
}

// TestLeasedSessionKeptWhenProviderTurnsUnhealthy: a pooled session keeps being
// reused after its provider turns unhealthy (health gates new opens only).
func TestLeasedSessionKeptWhenProviderTurnsUnhealthy(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	lr.setUnhealthy(modelB, leaseProv1)
	l2, r2 := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	if r2.SessionID != r.SessionID {
		t.Fatalf("session id = %s, want %s (reuse)", r2.SessionID, r.SessionID)
	}
	_, byBid := lr.paths()
	if len(byBid) != 1 {
		t.Fatalf("byBid = %v, want len 1", byBid)
	}
	lr.mu.Lock()
	if lr.sessions[r.SessionID].closed {
		t.Fatal("session closed, want kept open")
	}
	lr.mu.Unlock()
	l2.Done()
	l.Done()
}

// TestReconcileStillClosesExcluded: the boot reconcile is unchanged by
// health: a session on an excluded provider is closed even while the
// health source fails (and it is never re-dated or adopted).
func TestReconcileStillClosesExcluded(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	lr.addChain("0xc-excluded", modelA, c.Now().Add(-time.Minute), c.Now().Add(2*time.Hour))
	lr.mu.Lock()
	lr.healthErr = errors.New("health feed down")
	lr.mu.Unlock()
	p.SetProviderExclude([]string{mixedCase(leaseProv1)})
	p.Gate()
	p.StartupReconcile(context.Background(), func() (map[string]bool, bool) { return map[string]bool{modelA: true}, true }, ReconcileConfig{})
	waitUntil(t, "excluded session closed", func() bool { return lr.closedCount("0xc-excluded") == 1 })
	noOpens(t, lr, "reconcile")
}

// TestRehydrateAdoptsRegardlessOfHealth: an open on-chain session is
// adopted and reused even when its provider's bid is not healthy (health
// gates new opens only), with no new open.
func TestRehydrateAdoptsRegardlessOfHealth(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	lr.addChain("0xc-live", modelA, c.Now().Add(-time.Minute), c.Now().Add(2*time.Hour))
	lr.setUnhealthy(modelA, leaseProv1)
	lr.setUnhealthy(modelA, leaseProv2)
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	defer l.Done()
	if r.SessionID != "0xc-live" || r.Opened {
		t.Fatalf("got %+v, want the adopted session reused", r)
	}
	noOpens(t, lr, "rehydrate")
}
