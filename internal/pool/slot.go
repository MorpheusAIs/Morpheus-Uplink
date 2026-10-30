package pool

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// The wallet slot. The router wallet is dedicated to this Uplink, so one
// slot per instance serializes every wallet call Uplink sends (an open or
// a close). It is FIFO (the head waiter is handed the slot on release, no
// barging) and non-reentrant: functions that need it take a *slotToken,
// public entry points acquire it, and nothing holding a token calls a
// public entry point.
//
// Lock order: model lock → boot gate → slot → p.mu (brief). Nothing holding
// the slot waits on a model lock or on the gate.
const (
	openSlotWait     = 90 * time.Second // max wait in the slot queue
	slotHoldCap      = 60 * time.Second // hard hold deadline, set at acquisition
	openMinPostTime  = 25 * time.Second // an open POST starts only with ≥ this + reconcileReserve left
	readCallTimeout  = 10 * time.Second // per router read inside the slot
	closeCallTimeout = 30 * time.Second // per router close
	reconcileReserve = 10 * time.Second // held back from the open POST for reconcile
	maxQueuedPerKey  = 2                // queued opens per API key ("" is exempt)
	slotLongHold     = 30 * time.Second // warn when a hold lasts longer
	watchdogSlack    = 5 * time.Second  // the watchdog logs a holder past slotHoldCap + this
)

// sweepRetryEvery: how soon RunSweep tries again when a retired session's
// close found the wallet slot busy (internal; not a setting).
const sweepRetryEvery = 2 * time.Second

// callTimeScale divides every wall-clock bound of the slot: call
// deadlines, the queue wait and the watchdog (tests only: with an injected
// clock, a 60 s hold runs as 60 ms of real time). 0 or 1 is real time.
var callTimeScale atomic.Int64

// realDur is d of slot time as wall-clock time.
func realDur(d time.Duration) time.Duration {
	if k := callTimeScale.Load(); k > 1 {
		return d / time.Duration(k)
	}
	return d
}

// errSlotBusy: the slot queue wait cap or the per-key queue cap was hit.
var errSlotBusy = errors.New("wallet slot busy")

// slotToken is one hold of the wallet slot.
type slotToken struct {
	id       uint64
	key      string
	acquired time.Time // on the pool clock
	deadline time.Time // acquired + slotHoldCap, on the pool clock
	realAt   time.Time // wall clock at acquisition (hold-time log)
	released atomic.Bool
	watchdog *time.Timer
}

type slotWaiter struct {
	key string
	ch  chan *slotToken // buffered 1: the handoff never blocks
}

type walletSlot struct {
	mu      sync.Mutex
	holder  *slotToken
	waiters list.List // of *slotWaiter
	nextID  uint64
	now     func() time.Time

	// watchdogFired counts holders that overran slotHoldCap+watchdogSlack
	// (a bug; tests assert it stays 0).
	watchdogFired atomic.Int64
	// maxHeld is the most tokens ever held at once (tests assert ≤ 1).
	held    atomic.Int64
	maxHeld atomic.Int64
}

func (s *walletSlot) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// grantLocked makes a new token the holder (s.mu held).
func (s *walletSlot) grantLocked(key string) *slotToken {
	s.nextID++
	now := s.clock()
	tok := &slotToken{id: s.nextID, key: key, acquired: now, deadline: now.Add(slotHoldCap), realAt: time.Now()}
	s.holder = tok
	if n := s.held.Add(1); n > s.maxHeld.Load() {
		s.maxHeld.Store(n)
	}
	tok.watchdog = time.AfterFunc(realDur(slotHoldCap+watchdogSlack), func() {
		if !tok.released.Load() {
			s.watchdogFired.Add(1)
			slog.Warn("pool: wallet slot held past its cap (bug)", "holdCap", slotHoldCap)
		}
	})
	return tok
}

// tryAcquire takes the slot only when it is free and nobody is queued.
func (s *walletSlot) tryAcquire(key string) *slotToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder != nil || s.waiters.Len() > 0 {
		return nil
	}
	return s.grantLocked(key)
}

// acquire takes the slot or queues for it (FIFO). It returns ctx.Err()
// when ctx ends first, and errSlotBusy after openSlotWait or when
// maxQueuedPerKey waiters of the same non-empty key are already queued.
func (s *walletSlot) acquire(ctx context.Context, key string) (*slotToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.holder == nil && s.waiters.Len() == 0 {
		tok := s.grantLocked(key)
		s.mu.Unlock()
		return tok, nil
	}
	if key != "" {
		n := 0
		for e := s.waiters.Front(); e != nil; e = e.Next() {
			if e.Value.(*slotWaiter).key == key {
				n++
			}
		}
		if n >= maxQueuedPerKey {
			s.mu.Unlock()
			return nil, errSlotBusy
		}
	}
	w := &slotWaiter{key: key, ch: make(chan *slotToken, 1)}
	el := s.waiters.PushBack(w)
	s.mu.Unlock()

	timer := time.NewTimer(realDur(openSlotWait))
	defer timer.Stop()
	var err error
	select {
	case tok := <-w.ch:
		return tok, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errSlotBusy
	}
	s.mu.Lock()
	select {
	case tok := <-w.ch:
		// Handed the slot at the same moment: pass it on.
		s.mu.Unlock()
		s.release(tok)
		return nil, err
	default:
	}
	s.waiters.Remove(el)
	s.mu.Unlock()
	return nil, err
}

// release ends a hold; idempotent. The head waiter, if any, gets the slot.
func (s *walletSlot) release(tok *slotToken) {
	if tok == nil || !tok.released.CompareAndSwap(false, true) {
		return
	}
	if tok.watchdog != nil {
		tok.watchdog.Stop()
	}
	held := time.Since(tok.realAt)
	s.mu.Lock()
	if s.holder != tok {
		s.mu.Unlock()
		return
	}
	s.held.Add(-1)
	s.holder = nil
	depth := s.waiters.Len()
	if e := s.waiters.Front(); e != nil {
		w := s.waiters.Remove(e).(*slotWaiter)
		w.ch <- s.grantLocked(w.key)
	}
	s.mu.Unlock()
	slog.Debug("pool: wallet slot released", "held", held.Round(time.Millisecond), "queued", depth)
	if held > realDur(slotLongHold) {
		slog.Warn("pool: wallet slot held long", "held", held.Round(time.Second))
	}
}

// holds reports whether tok is the current holder.
func (s *walletSlot) holds(tok *slotToken) bool {
	if tok == nil || tok.released.Load() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holder == tok
}

// queued is the number of waiters.
func (s *walletSlot) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters.Len()
}

// remaining is the hold time left on tok (pool clock).
func (s *walletSlot) remaining(tok *slotToken) time.Duration {
	return tok.deadline.Sub(s.clock())
}

// callCtx bounds one router call inside the slot: the deadline is
// min(tok.deadline, now + per), turned into a wall-clock deadline (the pool
// clock may be a test clock).
func (s *walletSlot) callCtx(base context.Context, tok *slotToken, per time.Duration) (context.Context, context.CancelFunc) {
	left := s.remaining(tok)
	if per > 0 && per < left {
		left = per
	}
	if left < 0 {
		left = 0
	}
	return context.WithTimeout(base, realDur(left))
}
