package pool

import (
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
	if byModel != 0 || len(byBid) != 1 || byBid[0] != modelA+"|"+leaseProv1 {
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
	if byModel, byBid := lr.paths(); byModel != 0 || len(byBid) != 1 || byBid[0] != modelB+"|"+leaseProv2 {
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
	if byModel, byBid := lr.paths(); byModel != 2 || len(byBid) != 1 || byBid[0] != modelB+"|"+leaseProv1 {
		t.Fatalf("bidding failed provider: by model %d, by bid %v", byModel, byBid)
	}
}
