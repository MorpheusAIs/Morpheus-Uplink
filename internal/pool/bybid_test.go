package pool

import (
	"errors"
	"testing"
	"time"
)

const leaseProv3 = "0x4444444444444444444444444444444444444444"

func (lr *leaseRouter) paths() (byModel int, byBid []string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.byModel, append([]string(nil), lr.byBid...)
}

// An excluded provider that does not bid on the model leaves the plain
// open by model in place: the router rates and walks the providers.
func TestExcludedNonBidderOpensByModel(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderExclude([]string{leaseProv3}) // bids on neither model
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	byModel, byBid := lr.paths()
	if byModel != 1 || len(byBid) != 0 {
		t.Fatalf("by model %d, by bid %v", byModel, byBid)
	}
	lr.mu.Lock()
	prov := lr.sessions[r.SessionID].prov
	lr.mu.Unlock()
	if prov != leaseProv1 {
		t.Fatalf("opened on %s", prov)
	}
}

// An excluded provider that bids on the model is walked around by bid, in
// the router's rated order, with the real Bid.Id (the outer ID is a zero
// hash in the router's answer).
func TestExcludedBidderOpensByBidWithoutIt(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.bidders[modelA] = []string{leaseProv3, leaseProv1, leaseProv2}
	p.SetProviderExclude([]string{leaseProv3})
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	byModel, byBid := lr.paths()
	if byModel != 0 || len(byBid) != 1 || byBid[0] != bidID(modelA, leaseProv1) {
		t.Fatalf("by model %d, by bid %v", byModel, byBid)
	}
	lr.mu.Lock()
	prov := lr.sessions[r.SessionID].prov
	lr.mu.Unlock()
	if prov != leaseProv1 {
		t.Fatalf("opened on %s", prov)
	}
}

// A cooling provider that bids on the model sends the open by bid; once
// its cool-down ends the open is by model again.
func TestCooldownExpiryBackToOpenByModel(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(50 * time.Millisecond)
	p.MarkCooling(modelB, leaseProv1)
	if _, prov, err := p.openAvoidingCooling(modelB, 600); err != nil || prov != leaseProv2 {
		t.Fatalf("cooling open: %s %v", prov, err)
	}
	if byModel, byBid := lr.paths(); byModel != 0 || len(byBid) != 1 || byBid[0] != bidID(modelB, leaseProv2) {
		t.Fatalf("cooling: by model %d, by bid %v", byModel, byBid)
	}
	time.Sleep(80 * time.Millisecond)
	if _, prov, err := p.openAvoidingCooling(modelB, 600); err != nil || prov != leaseProv1 {
		t.Fatalf("after cool-down: %s %v", prov, err)
	}
	if byModel, byBid := lr.paths(); byModel != 1 || len(byBid) != 1 {
		t.Fatalf("after cool-down: by model %d, by bid %v", byModel, byBid)
	}
}

// A failover whose failed provider no longer bids on the model, with no
// other avoided provider bidding, reopens by model; one that still bids
// reopens by bid without it.
func TestFailoverReopenFollowsBidders(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(0) // only the provider that just failed is avoided
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv2} // leaseProv1 withdrew its bid
	lr.mu.Unlock()
	res, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil || res.Provider != leaseProv2 {
		t.Fatalf("failover %+v %v", res, err)
	}
	res.Lease.Done()
	if byModel, byBid := lr.paths(); byModel != 2 || len(byBid) != 0 {
		t.Fatalf("withdrawn bidder: by model %d, by bid %v", byModel, byBid)
	}

	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv2, leaseProv1}
	lr.mu.Unlock()
	res2, err := p.Failover(modelB, res.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil || res2.Provider != leaseProv1 {
		t.Fatalf("failover 2 %+v %v", res2, err)
	}
	res2.Lease.Done()
	if byModel, byBid := lr.paths(); byModel != 2 || len(byBid) != 1 || byBid[0] != bidID(modelB, leaseProv1) {
		t.Fatalf("bidding failed provider: by model %d, by bid %v", byModel, byBid)
	}
}

func ratedRow(id, prov string) map[string]any {
	return map[string]any{"ID": zeroHash, "Bid": map[string]any{"Id": id, "Provider": prov}, "Score": float64(1)}
}

func noOpens(t *testing.T, lr *leaseRouter, what string) {
	t.Helper()
	if byModel, byBid := lr.paths(); byModel != 0 || len(byBid) != 0 {
		t.Fatalf("%s: by model %d, by bid %v", what, byModel, byBid)
	}
}

// With a provider excluded and no rated bid for the model, the open is
// refused with ErrNoEligibleProvider; it is never an open by model, which
// could land on the excluded provider.
func TestEmptyRatedBidsWithExclusionRefuses(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.bidders[modelB] = []string{}
	p.SetProviderExclude([]string{leaseProv1})
	id, prov, err := p.openAvoidingCooling(modelB, 600)
	if !errors.Is(err, ErrNoEligibleProvider) || id != "" || prov != "" {
		t.Fatalf("open %q on %q: %v", id, prov, err)
	}
	noOpens(t, lr, "empty rated bids")
}

// With only a cooling or stream-cut provider avoided and no rated bid, the
// open is refused as unavailable; the refusal consumes the cut.
func TestEmptyRatedBidsWhileCoolingOrCutRefuses(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	lr.bidders[modelB] = []string{}
	p.MarkCooling(modelB, leaseProv1)
	var na *NoAvailableError
	if _, _, err := p.openAvoidingCooling(modelB, 600); !errors.As(err, &na) {
		t.Fatalf("cooling: %v", err)
	}
	lr2, p2 := newLeaseRouter(t, newFakeClock())
	lr2.bidders[modelB] = []string{}
	p2.MarkStreamCut(modelB, leaseProv1)
	if _, _, err := p2.openAvoidingCooling(modelB, 600); !errors.As(err, &na) {
		t.Fatalf("cut: %v", err)
	}
	if c := p2.pendingCut(modelB); c != "" {
		t.Fatalf("cut kept: %s", c)
	}
	noOpens(t, lr, "cooling")
	noOpens(t, lr2, "cut")
}

// A rated-bids row the pool cannot use (no valid bid id or provider) may be
// an avoided provider's: while anything is avoided, such a list never
// leads to an open by model.
func TestUnusableRatedRowsRefuseWhileAvoided(t *testing.T) {
	bad := []map[string]any{
		ratedRow(zeroHash, leaseProv1),               // the excluded provider, no bid id
		ratedRow(bidID(modelB, leaseProv1), "0xabc"), // malformed provider
	}
	for i, row := range bad {
		// Only unusable rows: as with no rated bid.
		lr, p := newLeaseRouter(t, newFakeClock())
		lr.rows[modelB] = []map[string]any{row}
		p.SetProviderExclude([]string{leaseProv1})
		if _, _, err := p.openAvoidingCooling(modelB, 600); !errors.Is(err, ErrNoEligibleProvider) {
			t.Fatalf("row %d alone: %v", i, err)
		}
		noOpens(t, lr, "unusable only")

		// Next to a usable row of a provider that is not avoided: refused.
		lr2, p2 := newLeaseRouter(t, newFakeClock())
		lr2.rows[modelB] = []map[string]any{ratedRow(bidID(modelB, leaseProv2), leaseProv2), row}
		p2.SetProviderExclude([]string{leaseProv1})
		var na *NoAvailableError
		if _, _, err := p2.openAvoidingCooling(modelB, 600); !errors.As(err, &na) {
			t.Fatalf("row %d with a usable row: %v", i, err)
		}
		noOpens(t, lr2, "unusable next to usable")
	}
}

// A stream-cut provider that does not bid on the model leaves the open by
// model in place, and the successful open clears the cut.
func TestStreamCutNonBidderOpensByModelAndClears(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.MarkStreamCut(modelB, leaseProv3)
	if _, prov, err := p.openAvoidingCooling(modelB, 600); err != nil || prov != leaseProv1 {
		t.Fatalf("open on %s: %v", prov, err)
	}
	if byModel, byBid := lr.paths(); byModel != 1 || len(byBid) != 0 {
		t.Fatalf("by model %d, by bid %v", byModel, byBid)
	}
	if c := p.pendingCut(modelB); c != "" {
		t.Fatalf("cut kept: %s", c)
	}
}

// A failover with an excluded provider that does not bid on the model
// follows the bidders: by bid while the failed provider still bids, by
// model once it has withdrawn.
func TestFailoverWithNonBiddingExclusion(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(0)
	p.SetProviderExclude([]string{leaseProv3})
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	res, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil || res.Provider != leaseProv2 {
		t.Fatalf("failover %+v %v", res, err)
	}
	res.Lease.Done()
	if byModel, byBid := lr.paths(); byModel != 1 || len(byBid) != 1 || byBid[0] != bidID(modelB, leaseProv2) {
		t.Fatalf("failed provider bids: by model %d, by bid %v", byModel, byBid)
	}
	lr.mu.Lock()
	lr.bidders[modelB] = []string{leaseProv1} // leaseProv2 withdrew its bid
	lr.mu.Unlock()
	res2, err := p.Failover(modelB, res.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil || res2.Provider != leaseProv1 {
		t.Fatalf("failover 2 %+v %v", res2, err)
	}
	res2.Lease.Done()
	if byModel, byBid := lr.paths(); byModel != 2 || len(byBid) != 1 {
		t.Fatalf("withdrawn: by model %d, by bid %v", byModel, byBid)
	}
}

// A failover whose rated bids have an unusable row keeps the session.
func TestFailoverUnusableRatedRowKeepsSession(t *testing.T) {
	lr, p := newLeaseRouter(t, newFakeClock())
	p.SetProviderCooldown(0)
	l, r := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1"})
	l.Done()
	lr.mu.Lock()
	lr.rows[modelB] = []map[string]any{ratedRow(bidID(modelB, leaseProv2), leaseProv2), ratedRow(zeroHash, leaseProv3)}
	lr.mu.Unlock()
	if _, err := p.Failover(modelB, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true}); !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("failover: %v", err)
	}
	if byModel, byBid := lr.paths(); byModel != 1 || len(byBid) != 0 {
		t.Fatalf("by model %d, by bid %v", byModel, byBid)
	}
	lr.mu.Lock()
	open := !lr.sessions[r.SessionID].closed
	lr.mu.Unlock()
	if !open {
		t.Fatal("session closed")
	}
}
