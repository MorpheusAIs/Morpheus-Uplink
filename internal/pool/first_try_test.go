package pool

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"
)

func held(p *Pool) {
	s, _ := new(big.Int).SetString(stakeSupply, 10)
	p.SetHeldStakeInputs(s, s)
}

// Spec §5 test 10: FirstTry names the bid the open sends first, past a
// cheaper low-Score bid, an excluded, a cooling and an unhealthy provider;
// with every bid over the limit it marks the router's first candidate and
// the open answers stake_limit_exceeded.
func TestStakeLimitSpec10FirstTryEqualsFirstSend(t *testing.T) {
	cheap, _ := ppsNear(t, morWei(t, "5"))
	mid, midPrice := ppsNear(t, morWei(t, "100"))
	m := &stakeMock{}
	m.setBids(
		mockBid{mbid(1), mprov(1), mid, 10},  // excluded
		mockBid{mbid(2), mprov(2), mid, 9},   // cooling
		mockBid{mbid(3), mprov(3), mid, 8},   // unhealthy
		mockBid{mbid(4), mprov(4), mid, 7},   // first send
		mockBid{mbid(5), mprov(5), cheap, 1}, // cheaper, lower Score
	)
	p := newStakeEnv(t, m, morWei(t, "150"))
	p.SetProviderExclude([]string{mprov(1)})
	p.MarkCooling(stakeModel, mprov(2))
	p.SetHealthyBids(func(string) (map[string]struct{}, error) {
		return map[string]struct{}{mbid(1): {}, mbid(2): {}, mbid(4): {}, mbid(5): {}}, nil
	})
	held(p)
	ft, err := p.FirstTry(context.Background(), stakeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := openStake(p); err != nil {
		t.Fatalf("open: %v", err)
	}
	posts := m.postList()
	if len(posts) == 0 || ft.Bid.ID != posts[0] || ft.OverLimit {
		t.Fatalf("FirstTry %s (over %v), first send %v", ft.Bid.ID, ft.OverLimit, posts)
	}
	if ft.StMorPerHourWei == nil || ft.StMorPerHourWei.Cmp(midPrice) != 0 {
		t.Fatalf("FirstTry price %v, want %v", ft.StMorPerHourWei, midPrice)
	}

	// All over the limit: marked, and the open refuses.
	over, _ := ppsNear(t, morWei(t, "200"))
	m2 := &stakeMock{}
	m2.setBids(mockBid{mbid(1), mprov(1), over, 10}, mockBid{mbid(2), mprov(2), over, 9})
	p2 := newStakeEnv(t, m2, morWei(t, "150"))
	held(p2)
	ft2, err := p2.FirstTry(context.Background(), stakeModel)
	if err != nil || !ft2.OverLimit || ft2.Bid.ID != mbid(1) {
		t.Fatalf("all over: %+v, %v; want the first candidate marked", ft2, err)
	}
	if err := openStake(p2); !errors.Is(err, ErrStakeLimitExceeded) {
		t.Fatalf("all over: open %v, want stake_limit_exceeded", err)
	}
}

// Spec §5 test 13: the FirstTry cache.
func TestStakeLimitSpec13Cache(t *testing.T) {
	in, _ := ppsNear(t, morWei(t, "10"))
	clock := time.Unix(1_800_000_000, 0)
	var cmu sync.Mutex
	now := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock }
	advance := func(d time.Duration) { cmu.Lock(); clock = clock.Add(d); cmu.Unlock() }
	ctx := context.Background()

	t.Run("repeat within 60 s reads once", func(t *testing.T) {
		m := &stakeMock{}
		m.setBids(mockBid{mbid(1), mprov(1), in, 10})
		p := newStakeEnv(t, m, morWei(t, "150"))
		p.SetClock(now)
		held(p)
		for i := 0; i < 3; i++ {
			if _, err := p.FillFirstTry(ctx, stakeModel); err != nil {
				t.Fatal(err)
			}
			advance(19 * time.Second)
		}
		if n := m.ratedReads.Load(); n != 1 {
			t.Fatalf("%d rated reads within 60 s, want 1", n)
		}
		advance(10 * time.Second) // past 60 s
		if _, ok := p.CachedFirstTry(stakeModel); ok {
			t.Fatal("held past 60 s")
		}
		if _, err := p.FillFirstTry(ctx, stakeModel); err != nil || m.ratedReads.Load() != 2 {
			t.Fatalf("after 60 s: err %v, reads %d; want a second read", err, m.ratedReads.Load())
		}
	})

	t.Run("shared read, at most 4 at once, cancel does not fail it", func(t *testing.T) {
		m := &stakeMock{ratedHold: make(chan struct{})}
		m.setBids(mockBid{mbid(1), mprov(1), in, 10})
		p := newStakeEnv(t, m, morWei(t, "150"))
		held(p)
		// A caller that gives up: the read goes on and its result is held.
		cctx, cancel := context.WithCancel(ctx)
		errc := make(chan error, 1)
		go func() { _, err := p.FillFirstTry(cctx, stakeModel); errc <- err }()
		var wg sync.WaitGroup
		models := []string{stakeModel}
		for i := 2; i <= 8; i++ {
			models = append(models, fmt.Sprintf("0x%064x", 0xab00+i))
		}
		results := make([]error, 0)
		var rmu sync.Mutex
		for _, mid := range models {
			for j := 0; j < 3; j++ { // three callers per model share one read
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := p.FillFirstTry(ctx, mid)
					rmu.Lock()
					results = append(results, err)
					rmu.Unlock()
				}()
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for m.ratedCur.Load() < firstTryMaxReads && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller: %v", err)
		}
		close(m.ratedHold)
		wg.Wait()
		for _, err := range results {
			if err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
		if mx := m.ratedMax.Load(); mx > firstTryMaxReads {
			t.Fatalf("%d reads at once, want <= %d", mx, firstTryMaxReads)
		}
		if n := m.ratedReads.Load(); n != int64(len(models)) {
			t.Fatalf("%d reads for %d models, want one each", n, len(models))
		}
		if _, ok := p.CachedFirstTry(stakeModel); !ok {
			t.Fatal("the cancelled caller's read was not held")
		}
	})

	t.Run("error retried after 10 s", func(t *testing.T) {
		m := &stakeMock{ratedCode: http.StatusInternalServerError}
		m.setBids(mockBid{mbid(1), mprov(1), in, 10})
		p := newStakeEnv(t, m, morWei(t, "150"))
		p.SetClock(now)
		held(p)
		if _, err := p.FillFirstTry(ctx, stakeModel); err == nil {
			t.Fatal("want the read error")
		}
		advance(5 * time.Second)
		if _, err := p.FillFirstTry(ctx, stakeModel); err == nil || m.ratedReads.Load() != 1 {
			t.Fatalf("within 10 s: err %v, reads %d; want the held error", err, m.ratedReads.Load())
		}
		m.mu.Lock()
		m.ratedCode = 0
		m.mu.Unlock()
		advance(6 * time.Second)
		if _, err := p.FillFirstTry(ctx, stakeModel); err != nil || m.ratedReads.Load() != 2 {
			t.Fatalf("after 10 s: err %v, reads %d; want a new read", err, m.ratedReads.Load())
		}
	})

	t.Run("no healthy bid, no read", func(t *testing.T) {
		m := &stakeMock{}
		m.setBids(mockBid{mbid(1), mprov(1), in, 10})
		p := newStakeEnv(t, m, morWei(t, "150"))
		p.SetHealthyBids(func(string) (map[string]struct{}, error) { return map[string]struct{}{}, nil })
		if _, err := p.FillFirstTry(ctx, stakeModel); !errors.Is(err, errNoHealthyBid) {
			t.Fatalf("err %v", err)
		}
		if n := m.ratedReads.Load(); n != 0 {
			t.Fatalf("%d rated reads, want 0", n)
		}
	})
}

// Spec §5 test 16: an open after a cached list reads supply and budget
// fresh in the wallet slot and never waits on a display read.
func TestStakeLimitSpec16OpenReadsFresh(t *testing.T) {
	in, _ := ppsNear(t, morWei(t, "10"))
	m := &stakeMock{}
	m.setBids(mockBid{mbid(1), mprov(1), in, 10})
	p := newStakeEnv(t, m, morWei(t, "150"))
	held(p)
	if _, err := p.FillFirstTry(context.Background(), stakeModel); err != nil {
		t.Fatal(err)
	}
	// A display read for another model hangs.
	m.mu.Lock()
	m.ratedHold = make(chan struct{})
	m.mu.Unlock()
	other := fmt.Sprintf("0x%064x", 0xabcd)
	go func() { _, _ = p.FillFirstTry(context.Background(), other) }()
	for m.ratedCur.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The open's own rated read must not wait: release only its path by
	// answering every later read at once.
	m.mu.Lock()
	hold := m.ratedHold
	m.ratedHold = nil
	m.mu.Unlock()
	defer close(hold)
	sup, bud := m.supplyReads.Load(), m.budgetReads.Load()
	done := make(chan error, 1)
	go func() { done <- openStake(p) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("open: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open waited on a display read")
	}
	if m.supplyReads.Load() != sup+1 || m.budgetReads.Load() != bud+1 {
		t.Fatalf("supply reads %d→%d, budget %d→%d; want one fresh read each", sup, m.supplyReads.Load(), bud, m.budgetReads.Load())
	}
}
