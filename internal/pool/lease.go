package pool

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Leases, holders, release and the idle sweep (long-job design S5). Every
// state change below runs under p.mu, the same lock that hands a pooled
// session out, so a lease can never land on a session that is retired or
// closing:
//
//	OPEN --request(k)--> inflight++, holders[k]=now   (same lock as hand-out)
//	     --end/panic/cancel--> inflight--              (Lease.Done, deferred)
//	     --release(k)--> delete holders[k]; none left and idle → RETIRED
//	     --sweep: idle ≥ grace--> CLOSING
//	     --failover--> RETIRED (not handed out)
//	RETIRED --inflight==0, sweep--> CLOSING --ok--> CLOSED | --fail--> RETIRED (sweep retries, max 3)
//
// Every close runs in the sweep, holding the wallet slot (slot.go).

// maxCloseRetries bounds the sweep's retries of a failed close.
const maxCloseRetries = 3

type sessState struct {
	model      string // normModel
	inflight   int
	holders    map[string]time.Time
	lastUse    time.Time
	openedAt   time.Time
	endsAt     time.Time // on-chain end
	opener     string    // key id that opened it ("" adopted or failover)
	retired    bool      // not handed out any more; closed when inflight reaches 0
	closing    bool
	closeFails int
}

// SetClock sets the pool's clock (tests).
// Uptime is measured on that clock from this call.
func (p *Pool) SetClock(now func() time.Time) {
	p.mu.Lock()
	p.clock = now
	if now != nil {
		p.started = now()
	}
	p.mu.Unlock()
}

func (p *Pool) now() time.Time {
	if c := p.clock; c != nil {
		return c()
	}
	return time.Now()
}

// SetSleep replaces the pool's wait between attempts (tests). nil: a
// real, ctx-bounded sleep. A non-nil error ends the wait early.
func (p *Pool) SetSleep(f func(ctx context.Context, d time.Duration) error) {
	p.mu.Lock()
	p.sleepFn = f
	p.mu.Unlock()
}

// sleep waits d, or until ctx ends (ctx.Err()).
func (p *Pool) sleep(ctx context.Context, d time.Duration) error {
	p.mu.Lock()
	f := p.sleepFn
	p.mu.Unlock()
	if f != nil {
		return f(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetDurationMax sets the sessionDurationMax setting (seconds).
func (p *Pool) SetDurationMax(sec int) {
	p.mu.Lock()
	p.durMax = sec
	p.mu.Unlock()
}

// DurationMax is the longest session the pool opens.
func (p *Pool) DurationMax() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.durMax <= 0 {
		return 1800
	}
	return p.durMax
}

// stateLocked returns the lease state of a pooled entry, creating it for
// sessions that have none yet (adopted at boot).
func (p *Pool) stateLocked(key string, e entry) *sessState {
	st := p.sess[e.sessionID]
	if st == nil {
		now := p.now()
		st = &sessState{model: key, holders: map[string]time.Time{}, lastUse: now, openedAt: now,
			endsAt: e.expiresAt.Add(expiryBuffer)}
		p.sess[e.sessionID] = st
	}
	return st
}

// newStateLocked records a session this pool just opened or adopted.
func (p *Pool) newStateLocked(key, sessionID string, openedAt, endsAt time.Time, opener string) *sessState {
	st := &sessState{model: key, holders: map[string]time.Time{}, lastUse: p.now(), openedAt: openedAt,
		endsAt: endsAt, opener: opener}
	p.sess[sessionID] = st
	return st
}

// forget drops a closed session's lease state and any pool slot that
// still points at it (ids compared case-insensitively), so a closed
// session is never listed or handed out again.
func (p *Pool) forget(sessionID string) {
	p.mu.Lock()
	delete(p.sess, sessionID)
	if sessionID != "" {
		for key, e := range p.entries {
			if strings.EqualFold(e.sessionID, sessionID) {
				delete(p.entries, key)
			}
		}
	}
	p.mu.Unlock()
}

// Lease is one request's hold on a session. Done must be deferred right
// after the lease is taken; it is safe to call more than once.
type Lease struct {
	p         *Pool
	SessionID string
	once      sync.Once
}

// Done ends the lease. When it leaves a retired session with nothing in
// flight, the sweep is asked to close it.
func (l *Lease) Done() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		p := l.p
		p.mu.Lock()
		st := p.sess[l.SessionID]
		kick := false
		if st != nil {
			if st.inflight > 0 {
				st.inflight--
			}
			st.lastUse = p.now()
			kick = st.inflight == 0 && st.retired && !st.closing && st.closeFails < maxCloseRetries
		}
		p.mu.Unlock()
		if kick {
			p.requestSweep()
		}
	})
}

// leaseLocked takes a lease on st for keyID (p.mu held).
func (p *Pool) leaseLocked(id string, st *sessState, keyID string) *Lease {
	now := p.now()
	st.inflight++
	if keyID != "" {
		st.holders[keyID] = now
	}
	st.lastUse = now
	if p.testHookLease != nil {
		p.testHookLease(id, st.inflight)
	}
	return &Lease{p: p, SessionID: id}
}

// SetTestHookLease runs f under the pool lock right after a lease is taken
// (tests assert the lease exists before the lock is dropped).
func (p *Pool) SetTestHookLease(f func(sessionID string, inflight int)) {
	p.mu.Lock()
	p.testHookLease = f
	p.mu.Unlock()
}

// LeaseSession takes a lease on a known, usable session (the replacement
// after a failover). nil when the session is gone, retired or closing.
func (p *Pool) LeaseSession(sessionID, keyID string) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, e := range p.entries {
		if e.sessionID != sessionID || !p.now().Before(e.expiresAt) {
			continue
		}
		st := p.stateLocked(key, e)
		if st.retired || st.closing || p.closingMarkedLocked(sessionID) {
			return nil
		}
		return p.leaseLocked(sessionID, st, keyID)
	}
	return nil
}

// AcquireOpts describe one request.
type AcquireOpts struct {
	KeyID       string
	DurationSec int // X-Uplink-Session-Duration (0: pool default)
}

// AcquireResult says what Acquire handed out.
type AcquireResult struct {
	SessionID   string
	Opened      bool
	DurationSec int
}

// tryPooledLocked leases the pooled session when it is usable (not
// expired, retired or closing).
func (p *Pool) tryPooledLocked(key string, o AcquireOpts, dur int) (*Lease, AcquireResult) {
	e, ok := p.entries[key]
	if !ok || !p.now().Before(e.expiresAt) {
		return nil, AcquireResult{}
	}
	if p.closingMarkedLocked(e.sessionID) {
		return nil, AcquireResult{}
	}
	st := p.stateLocked(key, e)
	if st.retired || st.closing {
		return nil, AcquireResult{}
	}
	l := p.leaseLocked(e.sessionID, st, o.KeyID)
	return l, AcquireResult{SessionID: e.sessionID, DurationSec: dur}
}

// Acquire is AcquireCtx without a request context.
func (p *Pool) Acquire(modelID string, o AcquireOpts) (*Lease, AcquireResult, error) {
	return p.AcquireCtx(context.Background(), modelID, o)
}

// AcquireCtx hands out a session for the model with a lease taken in the
// same critical section. Reuse order: pooled → adopt an on-chain session →
// open. A session is used until it expires or the sweep closes it.
//
// An open holds the wallet slot (one wallet call at a time, FIFO, 90 s
// queue cap, 60 s hold cap): it rereads the pool, scans the wallet
// (adopt-only), and opens by bid. ctx bounds the wait and every step up
// to the open POST; once the POST is sent the open runs to its own
// deadline and is reconciled whatever happens to ctx. Refusals:
// open_busy (queue full or timed out, or the router-busy window after a
// cut open; no Retry-After) and router_transient (Retry-After 1–4 s).
func (p *Pool) AcquireCtx(ctx context.Context, modelID string, o AcquireOpts) (*Lease, AcquireResult, error) {
	dur := o.DurationSec
	if dur <= 0 {
		dur = p.DurationSec()
	}
	key := normModel(modelID)
	p.mu.Lock()
	if l, res := p.tryPooledLocked(key, o, dur); l != nil {
		p.mu.Unlock()
		return l, res, nil
	}
	p.mu.Unlock()

	lock := p.openLock(key)
	if err := p.waitGateCtx(ctx); err != nil {
		return nil, AcquireResult{}, err
	}
	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	l, res := p.tryPooledLocked(key, o, dur)
	p.mu.Unlock()
	if l != nil {
		return l, res, nil
	}
	// A router-busy window (a cut open) is checked after the pre-open
	// scan below: that scan may find the cut open's session and clear it.
	tok, err := p.slot.acquire(ctx, o.KeyID)
	if err != nil {
		if errors.Is(err, errSlotBusy) {
			slog.Warn("pool: not opening", "model", modelID, "reason", ReasonOpenBusy, "queued", p.slot.queued())
			return nil, AcquireResult{}, errOpenBusyRefusal()
		}
		return nil, AcquireResult{}, err
	}
	defer p.slot.release(tok)

	p.mu.Lock()
	l, res = p.tryPooledLocked(key, o, dur)
	p.mu.Unlock()
	if l != nil {
		return l, res, nil
	}
	// Memory miss: prefer an already-staked on-chain session (adopt-only;
	// it also resolves earlier open attempts).
	if _, err := p.scan(ctx, tok, scanAdoptOnly); err != nil {
		if ctx.Err() != nil {
			return nil, AcquireResult{}, ctx.Err()
		}
		warnf("pool: rehydrate before open for %s: %v", modelID, err)
		if p.gated.Load() && !p.stateKnown.Load() {
			return nil, AcquireResult{}, ErrSessionStateUnknown
		}
	}
	p.mu.Lock()
	l, res = p.tryPooledLocked(key, o, dur)
	p.mu.Unlock()
	if l != nil {
		log.Printf("pool: adopted on-chain session %s for model %s", short_(res.SessionID), modelID)
		return l, res, nil
	}
	if p.routerBusy() {
		slog.Warn("pool: not opening", "model", modelID, "reason", "router_busy")
		return nil, AcquireResult{}, errOpenBusyRefusal()
	}

	openDur := dur
	// The cap applies to every open, with or without the header.
	if mx := p.DurationMax(); mx > 0 && openDur > mx {
		openDur = mx
	}
	op := p.newOpenOp(tok, modelID)
	got, err := op.openAvoidingCooling(ctx, openDur)
	if err != nil {
		return nil, AcquireResult{}, err
	}
	p.mu.Lock()
	st := p.poolOpenedLocked(key, got, openDur, o.KeyID)
	l = p.leaseLocked(got.id, st, o.KeyID)
	p.mu.Unlock()
	if got.adopted {
		log.Printf("pool: open for model %s found on chain as session %s", modelID, short_(got.id))
		return l, AcquireResult{SessionID: got.id, Opened: true, DurationSec: int(got.endsAt.Sub(got.openedAt) / time.Second)}, nil
	}
	log.Printf("pool: opened session %s for model %s (duration %ds)", short_(got.id), modelID, openDur)
	return l, AcquireResult{SessionID: got.id, Opened: true, DurationSec: openDur}, nil
}

// poolOpenedLocked pools a session this process just opened (or found on
// chain after an open whose answer was lost) and returns its state.
func (p *Pool) poolOpenedLocked(key string, got openResult, durSec int, opener string) *sessState {
	now := p.now()
	opened, ends := now, now.Add(time.Duration(durSec)*time.Second)
	if got.adopted {
		opened, ends = got.openedAt, got.endsAt
	}
	p.opened[strings.ToLower(got.id)] = true
	p.entries[key] = entry{sessionID: got.id, expiresAt: ends.Add(-expiryBuffer), provider: normProv(got.provider), stake: got.stake}
	return p.newStateLocked(key, got.id, opened, ends, opener)
}

func short_(id string) string { return short(id) }

// ReleaseOutcome is the answer to a release.
type ReleaseOutcome struct {
	Released bool   `json:"released"`
	Closed   bool   `json:"closed"`
	Reason   string `json:"reason,omitempty"` // shared, in_flight, not_held, closing
}

// Release removes keyID from the holders of the model's session. With no
// holder and nothing in flight left, the session is retired (never handed
// out again) and the sweep closes it: the answer is closed:false with
// reason "closing". The answer never depends on whether a session exists
// when the key holds none (not_held).
func (p *Pool) Release(modelID, keyID string) ReleaseOutcome {
	key := normModel(modelID)
	p.mu.Lock()
	var id string
	var st *sessState
	if e, ok := p.entries[key]; ok {
		if s := p.sess[e.sessionID]; s != nil && !s.closing {
			if _, held := s.holders[keyID]; held {
				id, st = e.sessionID, s
			}
		}
	}
	if st == nil {
		// A retired session the key still holds (draining).
		ids := make([]string, 0)
		for sid, s := range p.sess {
			if _, held := s.holders[keyID]; held && s.model == key && !s.closing {
				ids = append(ids, sid)
			}
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			id, st = ids[0], p.sess[ids[0]]
		}
	}
	if st == nil || keyID == "" {
		p.mu.Unlock()
		return ReleaseOutcome{Released: false, Reason: "not_held"}
	}
	delete(st.holders, keyID)
	switch {
	case len(st.holders) > 0:
		p.mu.Unlock()
		return ReleaseOutcome{Released: true, Reason: "shared"}
	case st.inflight > 0:
		p.mu.Unlock()
		return ReleaseOutcome{Released: true, Reason: "in_flight"}
	}
	st.retired = true
	if e, ok := p.entries[key]; ok && e.sessionID == id {
		delete(p.entries, key)
	}
	end := st.endsAt
	p.mu.Unlock()
	p.retire(id, end)
	p.requestSweep()
	return ReleaseOutcome{Released: true, Reason: "closing"}
}

// SweepIdle closes sessions with nothing in flight and no use for grace,
// and retired sessions (released, failed over, or a close that failed). It
// returns the number closed. Each close takes the wallet slot only when it
// is free (a queued open goes first) and closes one session per hold; the
// rest wait for the next tick.
func (p *Pool) SweepIdle(grace time.Duration) int {
	return p.sweepRetired(context.Background(), grace, true)
}

// sweepRetired is SweepIdle; with idle false it closes retired sessions
// only.
func (p *Pool) sweepRetired(ctx context.Context, grace time.Duration, idle bool) int {
	n := 0
	for ctx.Err() == nil && !p.gateClosed() {
		pick := func() string {
			now := p.now()
			if p.routerBusyLocked(now) || p.cutCloseMarkLocked(now) {
				return ""
			}
			var ids []string
			for id, st := range p.sess {
				if st.closing || st.inflight > 0 || p.closingMarkedLocked(id) {
					continue
				}
				if !st.retired && (!idle || now.Sub(st.lastUse) < grace) {
					continue
				}
				if st.retired && st.closeFails >= maxCloseRetries {
					continue
				}
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) == 0 {
				return ""
			}
			return ids[0]
		}
		p.mu.Lock()
		id := pick()
		p.mu.Unlock()
		if id == "" {
			return n
		}
		tok := p.slot.tryAcquire("")
		if tok == nil {
			p.retiredBusy.Store(true) // RunSweep tries again soon
			return n
		}
		p.mu.Lock()
		if pick() != id {
			p.mu.Unlock()
			p.slot.release(tok)
			continue
		}
		st := p.sess[id]
		st.closing = true
		if e, ok := p.entries[st.model]; ok && e.sessionID == id {
			delete(p.entries, st.model)
		}
		end := st.endsAt
		p.mu.Unlock()
		p.retire(id, end)
		out, err := p.closeWithToken(ctx, tok, id, "", "", closeCallTimeout)
		p.slot.release(tok)
		if out == closeOK || out == closeAlready {
			n++
			log.Printf("pool: idle sweep closed session %s", short(id))
			continue
		}
		p.mu.Lock()
		if st := p.sess[id]; st != nil {
			st.closing, st.retired = false, true
			st.closeFails++
		}
		p.mu.Unlock()
		warnf("pool: close session %s: %v (retired; retried by the sweep)", short(id), err)
		return n
	}
	return n
}

// retireForFailover takes a failed session out of the pool. It reports
// whether it may be closed now (nothing in flight); otherwise it closes
// when the last lease ends.
func (p *Pool) retireForFailover(key, sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok && e.sessionID == sessionID {
		delete(p.entries, key)
	}
	st := p.sess[sessionID]
	if st == nil {
		return true
	}
	st.retired = true
	if st.inflight > 0 || st.closing {
		return false
	}
	st.closing = true
	return true
}

// failoverCloseFailed puts a failover's failed close back to retired.
func (p *Pool) failoverCloseFailed(sessionID string) {
	p.mu.Lock()
	if st := p.sess[sessionID]; st != nil {
		st.closing = false
		st.closeFails++
	}
	p.mu.Unlock()
}

// InFlight is the in-flight count of a session (tests, status).
func (p *Pool) InFlight(sessionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.sess[sessionID]; st != nil {
		return st.inflight
	}
	return 0
}

// TotalInFlight sums in-flight over every tracked session.
func (p *Pool) TotalInFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, st := range p.sess {
		n += st.inflight
	}
	return n
}
