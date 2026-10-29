package pool

// The wallet's open slot (router RPC rate limiting). One open slot per
// wallet: router calls inside it are bounded by the request's remaining
// budget, the slot hold is capped, waiters are served first come first
// served, and closes (CloseSession, forced rescans) take the slot too, so
// a close never overlaps an open. These FAIL on the current code until
// the fix lands.

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// (c) First come, first served: while the first open (model A) is held
// at the router, requests for four other models arrive 50ms apart. No
// second open reaches the router until A's is released; then the opens
// arrive in the order the requests did.
//
// Timing note: the 50ms spacing orders the waiters' arrival at the slot.
func TestSlotWaitersFirstComeFirstServed(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	arrivals, release := gateOpens(t, lr)
	models := []string{modelA, modelB, modelC, modelD, extraModels[0]}
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	acquire := func(m string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.Acquire(m, AcquireOpts{KeyID: "k-" + m[:6]}); err != nil {
				t.Errorf("model %s: %v", m[:6], err)
			}
		}()
	}
	acquire(models[0])
	if got := awaitArrival(t, arrivals, 5*time.Second); got != bidID(models[0], leaseProv1) {
		t.Fatalf("first open on bid %s, want model A's top bid", got[:10])
	}
	for _, m := range models[1:] {
		acquire(m)
		time.Sleep(50 * time.Millisecond)
	}
	var got []string
	if early := noSecondOpen(t, arrivals); early != "" {
		got = append(got, early)
	}
	release()
	for len(got) < len(models)-1 {
		got = append(got, awaitArrival(t, arrivals, 5*time.Second))
	}
	wg.Wait()
	for i, m := range models[1:] {
		if want := bidID(m, leaseProv1); got[i] != want {
			t.Errorf("open %d after the held one: bid %s, want %s (model %s): waiters not served in arrival order", i+2, got[i][:10], want[:10], m[:6])
		}
	}
}

// (c) Deadlines and the slot hold cap. Model A's open hangs at the router
// until it is cancelled. The router call carries a deadline from the
// request's remaining budget, so A's request ends (cancelling the call)
// within the budget, and the slot is free again: a request for model C
// (no hang) is then served promptly; a waiter for model B that queued
// behind A gets an answer within the budget too (served or refused with
// Retry-After 1..4s).
//
// Real time: up to the 15s budget plus slack (skipped with -short).
func TestSlotRouterCallDeadlineAndHoldCap(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time: waits up to the retry budget")
	}
	_, lr, p, _ := rpcEnv(t)
	arrivals, release := gateOpens(t, lr)
	const slack = 3 * time.Second
	done := func(m string) (chan error, time.Time) {
		ch := make(chan error, 1)
		start := time.Now()
		go func() {
			_, _, err := p.Acquire(m, AcquireOpts{KeyID: "k-" + m[:6]})
			ch <- err
		}()
		return ch, start
	}
	aDone, aStart := done(modelA)
	awaitArrival(t, arrivals, 5*time.Second)
	bDone, bStart := done(modelB)
	select {
	case err := <-aDone:
		t.Logf("A after %v: %v", time.Since(aStart).Round(time.Millisecond), err)
	case <-time.After(retryBudget + slack - time.Since(aStart)):
		t.Errorf("A still waiting on its router call after %v: no deadline from the budget", retryBudget+slack)
		release()
		<-aDone
	}
	lr.mu.Lock()
	cancels := lr.gateCancels
	lr.mu.Unlock()
	if cancels == 0 {
		t.Error("held open never cancelled: the router call had no deadline")
	}
	bFinished := false
	select {
	case err := <-bDone:
		bFinished = true
		var na *NoAvailableError
		if err != nil && (!errors.As(err, &na) || na.RetryAfter < time.Second || na.RetryAfter > 4*time.Second) {
			t.Errorf("B after %v: %v (want served, or refused with Retry-After 1..4s)", time.Since(bStart).Round(time.Millisecond), err)
		}
	case <-time.After(retryBudget + slack - time.Since(bStart)):
		t.Errorf("B still waiting after %v: slot hold not capped", retryBudget+slack)
	}
	release()
	lr.mu.Lock()
	lr.gate, lr.arrivals = nil, nil
	lr.mu.Unlock()
	start := time.Now()
	cDone, _ := done(modelC)
	select {
	case err := <-cDone:
		if err != nil {
			t.Errorf("C after the slot freed: %v", err)
		}
	case <-time.After(slack):
		t.Errorf("C not served within %v of the slot freeing", slack)
	}
	t.Logf("C served in %v", time.Since(start).Round(time.Millisecond))
	if !bFinished {
		<-bDone
	}
}

// (e) Closes take the slot: while an open is held at the router, a
// CloseSession and a forced rescan (which closes an expired session) wait
// for it; no close reaches the router while an open is in flight, and
// both closes happen once it is released.
func TestCloseWaitsForSlot(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	l, res := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	lr.addChain("0xexpired1", modelC, c.Now().Add(-orphanAge-time.Hour), c.Now().Add(-time.Minute))
	arrivals, release := gateOpens(t, lr)
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _, _ = p.Acquire(modelB, AcquireOpts{KeyID: "k2"})
	}()
	awaitArrival(t, arrivals, 5*time.Second)
	go func() {
		defer wg.Done()
		_ = p.CloseSession(res.SessionID)
	}()
	go func() {
		defer wg.Done()
		_ = p.RehydrateForce()
	}()
	time.Sleep(300 * time.Millisecond)
	release()
	wg.Wait()
	lr.mu.Lock()
	overlap := lr.closeOverlap
	lr.mu.Unlock()
	if overlap != 0 {
		t.Errorf("%d close(s) reached the router while an open was in flight, want 0", overlap)
	}
	if n := lr.closedCount(res.SessionID); n != 1 {
		t.Errorf("CloseSession closes = %d, want 1", n)
	}
	if n := lr.closedCount("0xexpired1"); n != 1 {
		t.Errorf("expired session closes = %d, want 1 (rescan)", n)
	}
}

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends slog and log output to a buffer for the test.
func captureLogs(t *testing.T) *syncBuf {
	buf := &syncBuf{}
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// (f) A young unheld session that matches no attempt of ours suggests a
// second opener on the wallet: the sweep leaves it alone and logs a
// warning naming it.
func TestYoungUnmatchedSessionWarns(t *testing.T) {
	c, lr, p, _ := rpcEnv(t)
	logs := captureLogs(t)
	lr.addChainOn("0xyoung0001", modelB, leaseProv2, c.Now().Add(-30*time.Second), c.Now().Add(time.Hour))
	if err := p.RehydrateForce(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, short("0xyoung0001")) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning naming the young unmatched session %s; logs:\n%s", short("0xyoung0001"), logs.String())
	}
}
