package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Max stMOR/hr open-path tests (spec §5 tests 2-6 and 9) against a small
// mock router: rated bids with prices, supply, budget and opens by bid.

type mockBid struct {
	id, prov string
	pps      *big.Int // nil: no PricePerSecond in the row
	score    float64
}

type stakeMock struct {
	mu                 sync.Mutex
	bids               []mockBid
	fail               map[string]bool // bid id -> the open fails as a provider fault
	posts              []string
	supply, budget     string
	supplyCode         int // != 0: the supply read answers this status
	budgetCode         int
	supplyReads        atomic.Int64
	budgetReads        atomic.Int64
	ratedReads         atomic.Int64
	openedSessionCount int
	ratedHold          chan struct{} // non-nil: rated reads wait for it to close
	ratedCode          int           // != 0: rated reads answer this status
	ratedCur, ratedMax atomic.Int64
}

func mbid(n int) string  { return fmt.Sprintf("0x%064x", n) }
func mprov(n int) string { return fmt.Sprintf("0x%040x", n) }

// stakeSupply is the mock's supply and budget (equal: the price is about
// pps × 3600).
const stakeSupply = "1000000000000000000000000"

// morWei is a decimal MOR amount in wei (exact).
func morWei(t *testing.T, s string) *big.Int {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad MOR %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(big.NewInt(1e18)))
	if !r.IsInt() {
		t.Fatalf("%s MOR is not whole wei", s)
	}
	return new(big.Int).Set(r.Num())
}

// ppsNear is a price per second whose stMOR/hr is about w (w/3600 rounded
// up); price is its exact stMOR/hr at the mock's supply and budget.
func ppsNear(t *testing.T, w *big.Int) (pps, price *big.Int) {
	t.Helper()
	pps = new(big.Int).Add(w, big.NewInt(3599))
	pps.Quo(pps, big.NewInt(3600))
	s, _ := new(big.Int).SetString(stakeSupply, 10)
	price, err := router.StMorPerHourWei(s, s, pps)
	if err != nil {
		t.Fatal(err)
	}
	return pps, price
}

func (m *stakeMock) setBids(b ...mockBid) {
	m.mu.Lock()
	m.bids = b
	m.mu.Unlock()
}

func (m *stakeMock) postList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.posts...)
}

func newStakeEnv(t *testing.T, m *stakeMock, limit *big.Int) *Pool {
	t.Helper()
	if m.supply == "" {
		m.supply = stakeSupply
	}
	if m.budget == "" {
		m.budget = stakeSupply
	}
	if m.fail == nil {
		m.fail = map[string]bool{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", func(w http.ResponseWriter, r *http.Request) {
		m.ratedReads.Add(1)
		cur := m.ratedCur.Add(1)
		defer m.ratedCur.Add(-1)
		for {
			mx := m.ratedMax.Load()
			if cur <= mx || m.ratedMax.CompareAndSwap(mx, cur) {
				break
			}
		}
		m.mu.Lock()
		hold, code := m.ratedHold, m.ratedCode
		m.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if code != 0 {
			http.Error(w, `{"error":"x"}`, code)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		rows := []map[string]any{}
		for _, b := range m.bids {
			bid := map[string]any{"Id": b.id, "Provider": b.prov}
			if b.pps != nil {
				bid["PricePerSecond"] = b.pps.String()
			}
			rows = append(rows, map[string]any{"ID": zeroHash, "Bid": bid, "Score": b.score})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bids": rows})
	})
	mux.HandleFunc("GET /blockchain/token/supply", func(w http.ResponseWriter, r *http.Request) {
		m.supplyReads.Add(1)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.supplyCode != 0 {
			http.Error(w, `{"error":"x"}`, m.supplyCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"supply": m.supply})
	})
	mux.HandleFunc("GET /blockchain/sessions/budget", func(w http.ResponseWriter, r *http.Request) {
		m.budgetReads.Add(1)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.budgetCode != 0 {
			http.Error(w, `{"error":"x"}`, m.budgetCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"budget": m.budget})
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		m.posts = append(m.posts, id)
		if m.fail[id] {
			http.Error(w, `{"error":"provider request failed"}`, http.StatusInternalServerError)
			return
		}
		m.openedSessionCount++
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": fmt.Sprintf("0x%064x", 1000+m.openedSessionCount)})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.SetHealthyBids(func(string) (map[string]struct{}, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		h := map[string]struct{}{}
		for _, b := range m.bids {
			h[strings.ToLower(b.id)] = struct{}{}
		}
		return h, nil
	})
	p.SetStakeLimit(limit, false)
	return p
}

const stakeModel = "0x" + "ab00000000000000000000000000000000000000000000000000000000000001"

func openStake(p *Pool) error {
	_, _, err := p.openAvoidingCooling(stakeModel, 600)
	return err
}

// Spec §5 test 2: incident replay at 150. The 74 bid is tried and fails,
// the 1674.5 bid is skipped, nothing else is opened: stake_limit_exceeded,
// and the Status refusal slot shows it until an open succeeds.
func TestStakeLimitSpec2IncidentReplay(t *testing.T) {
	ppsA, _ := ppsNear(t, morWei(t, "74"))
	ppsB, priceB := ppsNear(t, morWei(t, "1674.5"))
	if priceB.Cmp(morWei(t, "150")) <= 0 {
		t.Fatal("test setup: B must be above the limit")
	}
	m := &stakeMock{fail: map[string]bool{mbid(1): true}}
	m.setBids(mockBid{mbid(1), mprov(1), ppsA, 10}, mockBid{mbid(2), mprov(2), ppsB, 9})
	p := newStakeEnv(t, m, morWei(t, "150"))
	err := openStake(p)
	if !errors.Is(err, ErrStakeLimitExceeded) {
		t.Fatalf("err %v, want stake_limit_exceeded", err)
	}
	if got := m.postList(); !slices.Equal(got, []string{mbid(1)}) {
		t.Fatalf("opens %v, want only the 74 bid", got)
	}
	if _, ok := p.LastStakeRefusal(); !ok {
		t.Fatal("no Status refusal after stake_limit_exceeded")
	}
	// Any open clears it: a new in-limit bid from another provider.
	ppsC, _ := ppsNear(t, morWei(t, "10"))
	m.setBids(mockBid{mbid(1), mprov(1), ppsA, 10}, mockBid{mbid(2), mprov(2), ppsB, 9}, mockBid{mbid(3), mprov(3), ppsC, 8})
	if err := openStake(p); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, ok := p.LastStakeRefusal(); ok {
		t.Fatal("Status refusal kept after a successful open")
	}
}

// Spec §5 test 3: three over-limit bids then one in-limit: it opens, and
// skips use no attempts.
func TestStakeLimitSpec3SkipsUseNoAttempts(t *testing.T) {
	over, _ := ppsNear(t, morWei(t, "200"))
	in, _ := ppsNear(t, morWei(t, "100"))
	m := &stakeMock{}
	m.setBids(mockBid{mbid(1), mprov(1), over, 10}, mockBid{mbid(2), mprov(2), over, 9}, mockBid{mbid(3), mprov(3), over, 8},
		mockBid{mbid(4), mprov(4), in, 7})
	p := newStakeEnv(t, m, morWei(t, "150"))
	if err := openStake(p); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := m.postList(); !slices.Equal(got, []string{mbid(4)}) {
		t.Fatalf("opens %v, want only the in-limit bid", got)
	}

	// Three skips, then three failing in-limit bids use the three
	// attempts (MaxBidAttempts); the seventh is never reached.
	m2 := &stakeMock{fail: map[string]bool{mbid(4): true, mbid(5): true, mbid(6): true}}
	m2.setBids(mockBid{mbid(1), mprov(1), over, 10}, mockBid{mbid(2), mprov(2), over, 9}, mockBid{mbid(3), mprov(3), over, 8},
		mockBid{mbid(4), mprov(4), in, 7}, mockBid{mbid(5), mprov(5), in, 6}, mockBid{mbid(6), mprov(6), in, 5}, mockBid{mbid(7), mprov(7), in, 4})
	p2 := newStakeEnv(t, m2, morWei(t, "150"))
	err := openStake(p2)
	if !errors.Is(err, ErrStakeLimitExceeded) {
		t.Fatalf("err %v, want stake_limit_exceeded (bids were skipped, none opened)", err)
	}
	if got := m2.postList(); !slices.Equal(got, []string{mbid(4), mbid(5), mbid(6)}) {
		t.Fatalf("opens %v, want the three in-limit bids", got)
	}
}

// Spec §5 test 4: a price equal to the limit passes; +1 wei is skipped.
func TestStakeLimitSpec4EqualPassesPlusOneSkips(t *testing.T) {
	pps, price := ppsNear(t, morWei(t, "150"))
	m := &stakeMock{}
	m.setBids(mockBid{mbid(1), mprov(1), pps, 10})
	if err := openStake(newStakeEnv(t, m, price)); err != nil {
		t.Fatalf("price == limit: %v", err)
	}
	if n := len(m.postList()); n != 1 {
		t.Fatalf("price == limit: %d opens, want 1", n)
	}
	m2 := &stakeMock{}
	m2.setBids(mockBid{mbid(1), mprov(1), pps, 10})
	err := openStake(newStakeEnv(t, m2, new(big.Int).Sub(price, big.NewInt(1))))
	if !errors.Is(err, ErrStakeLimitExceeded) || len(m2.postList()) != 0 {
		t.Fatalf("price == limit+1 wei: err %v, opens %v; want skipped", err, m2.postList())
	}
}

// Spec §5 test 5: a bid with no price is skipped while the limit is on and
// opens when it is off.
func TestStakeLimitSpec5NoPriceBid(t *testing.T) {
	m := &stakeMock{}
	m.setBids(mockBid{mbid(1), mprov(1), nil, 10})
	err := openStake(newStakeEnv(t, m, morWei(t, "150")))
	if !errors.Is(err, ErrStakeLimitExceeded) || len(m.postList()) != 0 {
		t.Fatalf("on: err %v, opens %v; want skipped", err, m.postList())
	}
	m2 := &stakeMock{}
	m2.setBids(mockBid{mbid(1), mprov(1), nil, 10})
	if err := openStake(newStakeEnv(t, m2, nil)); err != nil || len(m2.postList()) != 1 {
		t.Fatalf("off: err %v, opens %v; want one open", err, m2.postList())
	}
}

// Spec §5 test 6: a supply or budget read error, or a value <= 0, refuses
// with no open, even with a held pair.
func TestStakeLimitSpec6FreshReadsRefuse(t *testing.T) {
	in, _ := ppsNear(t, morWei(t, "10"))
	for name, m := range map[string]*stakeMock{
		"supply error": {supplyCode: http.StatusInternalServerError},
		"budget error": {budgetCode: http.StatusInternalServerError},
		"supply zero":  {supply: "0"},
		"budget -1":    {budget: "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			m.setBids(mockBid{mbid(1), mprov(1), in, 10})
			p := newStakeEnv(t, m, morWei(t, "150"))
			p.SetHeldStakeInputs(big.NewInt(1), big.NewInt(1))
			err := openStake(p)
			if !errors.Is(err, ErrNoAvailableProvider) || errors.Is(err, ErrStakeLimitExceeded) {
				t.Fatalf("err %v, want a refusal", err)
			}
			if got := m.postList(); len(got) != 0 {
				t.Fatalf("opens %v, want none", got)
			}
		})
	}
}

// Spec §5 test 9: with the limit off the walk's order equals the router's
// (and no supply or budget is read); on, with every bid in limit, the same.
func TestStakeLimitSpec9OrderKept(t *testing.T) {
	in, _ := ppsNear(t, morWei(t, "10"))
	for name, limit := range map[string]*big.Int{"off": nil, "on": morWei(t, "150")} {
		t.Run(name, func(t *testing.T) {
			m := &stakeMock{fail: map[string]bool{mbid(1): true, mbid(2): true, mbid(3): true, mbid(4): true}}
			// Listed out of Score order: the client sorts by Score.
			m.setBids(mockBid{mbid(3), mprov(3), in, 8}, mockBid{mbid(1), mprov(1), in, 10},
				mockBid{mbid(4), mprov(4), in, 7}, mockBid{mbid(2), mprov(2), in, 9})
			p := newStakeEnv(t, m, limit)
			if err := openStake(p); err == nil {
				t.Fatal("every open fails: want an error")
			}
			if got := m.postList(); !slices.Equal(got, []string{mbid(1), mbid(2), mbid(3)}) {
				t.Fatalf("opens %v, want router order", got)
			}
			if limit == nil && (m.supplyReads.Load() != 0 || m.budgetReads.Load() != 0) {
				t.Fatalf("limit off read supply %d, budget %d; want none", m.supplyReads.Load(), m.budgetReads.Load())
			}
		})
	}
}
