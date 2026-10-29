package pool

import (
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Leases, holders, release and the idle sweep (long-job design S5). Every
// state change below runs under p.mu, the same lock that hands a pooled
// session out, so a lease can never land on a session that is retired or
// closing:
//
//	OPEN --request(k)--> inflight++, holders[k]=now   (same lock as hand-out)
//	     --end/panic/cancel--> inflight--              (Lease.Done, deferred)
//	     --release(k)--> delete holders[k]; none left and idle → CLOSING
//	     --sweep: idle ≥ grace--> CLOSING
//	     --failover--> RETIRED (not handed out)
//	RETIRED --inflight==0--> CLOSING --ok--> CLOSED | --fail--> RETIRED (sweep retries, max 3)

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
func (p *Pool) SetClock(now func() time.Time) {
	p.mu.Lock()
	p.clock = now
	p.mu.Unlock()
}

func (p *Pool) now() time.Time {
	if c := p.clock; c != nil {
		return c()
	}
	return time.Now()
}

// sleepHook, when set, replaces time.Sleep for the pool's waits between
// attempts (tests, via SetSleep). Unset: the real time.Sleep.
var sleepHook atomic.Pointer[func(time.Duration)]

// SetSleep replaces the pool's sleep (tests) and returns a func that puts
// the previous one back. nil restores the real time.Sleep.
func SetSleep(f func(time.Duration)) (restore func()) {
	var next *func(time.Duration)
	if f != nil {
		next = &f
	}
	prev := sleepHook.Swap(next)
	return func() { sleepHook.Store(prev) }
}

// sleep waits d through the hook (default: time.Sleep).
func sleep(d time.Duration) {
	if f := sleepHook.Load(); f != nil {
		(*f)(d)
		return
	}
	time.Sleep(d)
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
// flight, the session is closed (in the background).
func (l *Lease) Done() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		p := l.p
		p.mu.Lock()
		st := p.sess[l.SessionID]
		closeIt := false
		if st != nil {
			if st.inflight > 0 {
				st.inflight--
			}
			st.lastUse = p.now()
			if st.inflight == 0 && st.retired && !st.closing && st.closeFails < maxCloseRetries {
				st.closing, closeIt = true, true
			}
		}
		p.mu.Unlock()
		if closeIt {
			go p.closeState(l.SessionID)
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
		if st.retired || st.closing {
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
	st := p.stateLocked(key, e)
	if st.retired || st.closing {
		return nil, AcquireResult{}
	}
	l := p.leaseLocked(e.sessionID, st, o.KeyID)
	return l, AcquireResult{SessionID: e.sessionID, DurationSec: dur}
}

// Acquire hands out a session for the model with a lease taken in the same
// critical section. Reuse order: pooled → adopt an on-chain session → open.
// A session is used until it expires or the idle sweep closes it; the next
// call then opens a new one.
func (p *Pool) Acquire(modelID string, o AcquireOpts) (*Lease, AcquireResult, error) {
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
	p.waitGate()
	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	l, res := p.tryPooledLocked(key, o, dur)
	p.mu.Unlock()
	if l != nil {
		return l, res, nil
	}
	// Memory miss: prefer an already-staked on-chain session.
	if err := p.RehydrateForce(); err != nil {
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

	openDur := dur
	// The cap applies to every open, with or without the header.
	if mx := p.DurationMax(); mx > 0 && openDur > mx {
		openDur = mx
	}
	id, prov, err := p.openAvoidingCooling(modelID, openDur)
	if err != nil {
		return nil, AcquireResult{}, err
	}
	now := p.now()
	p.mu.Lock()
	p.entries[key] = entry{sessionID: id, expiresAt: now.Add(time.Duration(openDur)*time.Second - expiryBuffer), provider: prov}
	st := p.newStateLocked(key, id, now, now.Add(time.Duration(openDur)*time.Second), o.KeyID)
	l = p.leaseLocked(id, st, o.KeyID)
	p.mu.Unlock()
	log.Printf("pool: opened session %s for model %s (duration %ds)", short_(id), modelID, openDur)
	return l, AcquireResult{SessionID: id, Opened: true, DurationSec: openDur}, nil
}

func short_(id string) string { return short(id) }

// closeState closes a session marked closing; a failure puts it back to
// retired for the sweep to retry (at most maxCloseRetries times).
func (p *Pool) closeState(sessionID string) bool {
	err := p.closeBounded(sessionID)
	if err != nil {
		p.mu.Lock()
		if st := p.sess[sessionID]; st != nil {
			st.closing, st.retired = false, true
			st.closeFails++
		}
		p.mu.Unlock()
		warnf("pool: close session %s: %v (retired; retried by the sweep)", short(sessionID), err)
		return false
	}
	p.notifyClosed(sessionID)
	return true
}

// ReleaseOutcome is the answer to a release.
type ReleaseOutcome struct {
	Released bool   `json:"released"`
	Closed   bool   `json:"closed"`
	Reason   string `json:"reason,omitempty"` // shared, in_flight, not_held, retry
}

// Release removes keyID from the holders of the model's session. With no
// holder and nothing in flight left, the session is closed now; otherwise
// the close is left to the idle sweep. The answer never depends on
// whether a session exists when the key holds none (not_held).
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
	st.closing = true
	if e, ok := p.entries[key]; ok && e.sessionID == id {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	p.retire(id, st.endsAt)
	if !p.closeState(id) {
		return ReleaseOutcome{Released: true, Reason: "retry"}
	}
	return ReleaseOutcome{Released: true, Closed: true}
}

// SweepIdle closes sessions with nothing in flight and no use for grace,
// and retries retired sessions whose close failed. It returns the number
// closed.
func (p *Pool) SweepIdle(grace time.Duration) int {
	now := p.now()
	var ids []string
	p.mu.Lock()
	for id, st := range p.sess {
		if st.closing || st.inflight > 0 {
			continue
		}
		if !st.retired && now.Sub(st.lastUse) < grace {
			continue
		}
		if st.retired && st.closeFails >= maxCloseRetries {
			continue
		}
		st.closing = true
		if e, ok := p.entries[st.model]; ok && e.sessionID == id {
			delete(p.entries, st.model)
		}
		ids = append(ids, id)
	}
	p.mu.Unlock()
	sort.Strings(ids)
	n := 0
	for _, id := range ids {
		p.mu.Lock()
		end := time.Time{}
		if st := p.sess[id]; st != nil {
			end = st.endsAt
		}
		p.mu.Unlock()
		p.retire(id, end)
		if p.closeState(id) {
			n++
			log.Printf("pool: idle sweep closed session %s", short(id))
		}
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
