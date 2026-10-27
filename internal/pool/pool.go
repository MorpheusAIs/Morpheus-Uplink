// Package pool keeps at most one open proxy-router session per model and
// reuses it across requests until it nears its on-chain expiry.
package pool

import (
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// expiryBuffer stops using a session this long before its on-chain end so
// we never send a prompt into a session that expires mid-answer.
const expiryBuffer = 90 * time.Second

type entry struct {
	sessionID string
	expiresAt time.Time
	stake     *big.Int
	provider  string // lowercase provider address; "" when not known
}

// OnSessionClosed is invoked after a successful on-chain close so callers can
// attribute actual usage (OpenedAt→ClosedAt) from the receipt.
type OnSessionClosed func(sessionID string)

type Pool struct {
	client   *router.Client
	duration atomic.Int64 // default session length (s); S6 settings change it live

	mu      sync.Mutex
	entries map[string]entry       // normModel(modelID) -> session
	opening map[string]*sync.Mutex // per-model open lock

	// Provider failover memory (failover.go). In memory only: a restart
	// forgets cool-downs, and Rehydrate rediscovers sessions.
	fmu      sync.Mutex
	cooldown time.Duration        // 0: no memory across requests
	cooling  map[string]time.Time // normModel|provider -> until
	retired  map[string]time.Time // session id -> on-chain end (never re-adopted)
	// cutNext: a provider that cut a stream; the model's next open walks
	// the bids excluding it, whatever the cool-down (one-shot).
	cutNext map[string]string // normModel -> provider
	// exclude: UPLINK_PROVIDER_EXCLUDE, model id or lowercased name ->
	// providers never opened for it. namer maps an id to its name.
	exclude map[string]map[string]bool
	namer   func(modelID string) string
	// excludeByName: some exclusions are keyed by catalog name.
	excludeByName bool
	// unresolvedRefused: models whose opens were refused because their
	// name-keyed exclusions could not be resolved (for the console).
	unresolvedRefused map[string]bool
	unresolvedCapped  bool        // an id was dropped at MaxUnresolvedRefusals
	catalogLoaded     func() bool // no network; see SetCatalogLoaded

	// Leases (lease.go), under mu.
	sess          map[string]*sessState // session id -> lease state
	clock         func() time.Time
	durMax        int
	testHookLease func(sessionID string, inflight int)

	onClosed OnSessionClosed

	// Boot reconcile: opens wait on gate (nil = open) and are refused until
	// some session list succeeded (stateKnown), so a restart never
	// double-opens next to a session it could not see.
	gate        chan struct{}
	reconcileMu sync.Mutex
	stateKnown  atomic.Bool
	gated       atomic.Bool
	recMu       sync.Mutex
	closing     map[string]bool // ids the reconcile is closing (rehydrate skips them)
	recState    ReconcileState
	onState     func(ReconcileState)
	onRescan    func(ReconcileEvent)

	// Last on-chain open-session scan (for GUI), refreshed by Rehydrate.
	chainMu      sync.Mutex
	chainOpen    []ChainSession
	chainFetched time.Time
	rehydrateErr string
}

// ChainSession is an open (ClosedAt==0) on-chain session for display.
type ChainSession struct {
	ModelID   string    `json:"modelId"`
	ModelName string    `json:"modelName,omitempty"` // catalog name when known
	SessionID string    `json:"sessionId"`
	StakeWei  string    `json:"stakeWei"`
	EndsAt    time.Time `json:"endsAt"`
	Usable    bool      `json:"usable"` // still within endsAt (minus buffer)
	Pooled    bool      `json:"pooled"` // adopted into the reuse pool
}

// New builds the session pool. Sessions are always staking-mode (never
// direct payment), and the router's own in-prompt failover is always off:
// provider failover is run by the pool (Failover).
func New(client *router.Client, durationSec int) *Pool {
	p := &Pool{
		client:   client,
		entries:  map[string]entry{},
		opening:  map[string]*sync.Mutex{},
		cooldown: DefaultProviderCooldown,
		cooling:  map[string]time.Time{},
		retired:  map[string]time.Time{},
		cutNext:  map[string]string{},

		sess:   map[string]*sessState{},
		durMax: 1800,
	}
	p.duration.Store(int64(durationSec))
	return p
}

// DurationSec is the configured default session length.
func (p *Pool) DurationSec() int { return int(p.duration.Load()) }

// SetDuration changes the default session length (S6 settings). Open
// sessions keep their own end time.
func (p *Pool) SetDuration(sec int) { p.duration.Store(int64(sec)) }

// SetOnSessionClosed registers a hook run after each successful close.
func (p *Pool) SetOnSessionClosed(fn OnSessionClosed) { p.onClosed = fn }

func (p *Pool) notifyClosed(sessionID string) {
	p.forget(sessionID)
	if p.onClosed == nil || sessionID == "" {
		return
	}
	// Caller may poll chain; never block close/rehydrate on usage attribution.
	go p.onClosed(sessionID)
}

// SetOnReconcileState registers a callback for reconcile state changes
// (used to write the status file).
func (p *Pool) SetOnReconcileState(f func(ReconcileState)) {
	p.recMu.Lock()
	p.onState = f
	p.recMu.Unlock()
}

// SetOnRescanEvent registers the audit hook for closes made by a rescan
// (expired sessions, including ones the boot reconcile deferred).
func (p *Pool) SetOnRescanEvent(f func(ReconcileEvent)) {
	p.recMu.Lock()
	p.onRescan = f
	p.recMu.Unlock()
}

func (p *Pool) emitRescan(ev ReconcileEvent) {
	p.recMu.Lock()
	f := p.onRescan
	p.recMu.Unlock()
	if f != nil {
		f(ev)
	}
}

// openLock returns the per-model open lock.
func (p *Pool) openLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	lock, ok := p.opening[key]
	if !ok {
		lock = &sync.Mutex{}
		p.opening[key] = lock
	}
	return lock
}

// Invalidate drops the pooled session for a model (e.g. after a failed
// prompt) so the next request opens a fresh one.
func (p *Pool) Invalidate(modelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := normModel(modelID)
	if e, ok := p.entries[key]; ok {
		delete(p.sess, e.sessionID) // the router already dropped it
	}
	delete(p.entries, key)
}

// Snapshot is one pooled session for the admin status. Holders are key
// ids (admin only, never sent to API keys).
type Snapshot struct {
	ModelID     string    `json:"modelId"`
	SessionID   string    `json:"sessionId"`
	ExpiresAt   time.Time `json:"expiresAt"`
	InFlight    int       `json:"inFlight"`
	HolderCount int       `json:"holderCount"`
	Holders     []string  `json:"holders"`
	OpenedAt    time.Time `json:"openedAt"`
	EndsAt      time.Time `json:"endsAt"`
	Opener      string    `json:"opener,omitempty"`
}

func (p *Pool) Snapshot() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Snapshot, 0, len(p.entries))
	for modelID, e := range p.entries {
		sn := Snapshot{ModelID: modelID, SessionID: e.sessionID, ExpiresAt: e.expiresAt, Holders: []string{}}
		if st := p.sess[e.sessionID]; st != nil {
			sn.InFlight, sn.HolderCount, sn.OpenedAt, sn.EndsAt, sn.Opener = st.inflight, len(st.holders), st.openedAt, st.endsAt, st.opener
			for k := range st.holders {
				sn.Holders = append(sn.Holders, k)
			}
			sort.Strings(sn.Holders)
		}
		out = append(out, sn)
	}
	return out
}

// ChainOpen returns the last scanned on-chain open sessions (may be empty
// before the first successful Rehydrate).
func (p *Pool) ChainOpen() []ChainSession {
	p.chainMu.Lock()
	defer p.chainMu.Unlock()
	out := make([]ChainSession, len(p.chainOpen))
	copy(out, p.chainOpen)
	return out
}

func (p *Pool) RehydrateError() string {
	p.chainMu.Lock()
	defer p.chainMu.Unlock()
	return p.rehydrateErr
}

// ActiveStakeWei sums stake across the last on-chain open-session scan.
func (p *Pool) ActiveStakeWei() *big.Int {
	p.chainMu.Lock()
	defer p.chainMu.Unlock()
	sum := big.NewInt(0)
	for _, s := range p.chainOpen {
		if n, ok := new(big.Int).SetString(s.StakeWei, 10); ok {
			sum.Add(sum, n)
		}
	}
	return sum
}

// Rehydrate loads open sessions from the router (which reads chain), adopts
// one usable session per model into the reuse pool, and asks the router to
// close sessions that are past endsAt (unused stake returns; used → daylock).
//
// force=false skips work when a scan ran recently (status polling).
func (p *Pool) Rehydrate() error {
	return p.rehydrate(false)
}

// RehydrateForce always rescans (e.g. after Close all).
func (p *Pool) RehydrateForce() error {
	return p.rehydrate(true)
}

func (p *Pool) rehydrate(force bool) error {
	// While the boot reconcile runs, it owns the first scan: a status poll
	// returns the (empty) last scan, a forced scan (housekeep, close) waits.
	if p.gateClosed() {
		if !force {
			return nil
		}
		p.waitGate()
	}
	if !force {
		p.chainMu.Lock()
		fresh := time.Since(p.chainFetched) < 20*time.Second && p.rehydrateErr == ""
		p.chainMu.Unlock()
		if fresh {
			return nil
		}
	}

	addr, err := p.client.WalletAddress()
	if err != nil {
		p.setRehydrateErr(err.Error())
		return err
	}
	open, err := p.client.ListOpenSessions(addr)
	if err != nil {
		p.setRehydrateErr(err.Error())
		return err
	}

	now := p.now()
	nowUnix := now.Unix()
	best := map[string]router.Session{} // model -> farthest-ending usable
	display := make([]ChainSession, 0, len(open))

	for _, s := range open {
		usable := s.EndsAt > nowUnix+int64(expiryBuffer.Seconds())
		stakeStr := "0"
		if s.Stake != nil {
			stakeStr = s.Stake.String()
		}
		if p.isClosing(s.ID) {
			continue // the boot reconcile is closing it
		}
		skipAdopt := p.isRetired(s.ID) || p.IsCooling(s.ModelAgentID, s.Provider) || p.isExcluded(s.ModelAgentID, s.Provider)
		if !usable {
			// Past end (or inside buffer): ask router to close so unused
			// returns and used moves to the daylock queue.
			ev := ReconcileEvent{Kind: "session_reconcile_close", Reason: ReasonExpired, Session: short(s.ID), Result: "ok", Source: "rescan"}
			if err := p.client.CloseSession(s.ID); err != nil {
				ev.Result = "failed"
				p.emitRescan(ev)
				warnf("pool: close expired session %s: %v", short(s.ID), err)
				// Still show it — close failed, stake remains active.
				display = append(display, ChainSession{
					ModelID: s.ModelAgentID, SessionID: s.ID, StakeWei: stakeStr,
					EndsAt: time.Unix(s.EndsAt, 0).UTC(), Usable: false,
				})
			} else {
				p.emitRescan(ev)
				log.Printf("pool: closed expired session %s (model %s)", short(s.ID), s.ModelAgentID)
				p.notifyClosed(s.ID)
			}
			continue
		}
		// Retired (failed over) sessions and sessions on a cooling provider
		// stay visible but are never adopted back into the pool.
		if cur, ok := best[normModel(s.ModelAgentID)]; !skipAdopt && (!ok || s.EndsAt > cur.EndsAt) {
			best[normModel(s.ModelAgentID)] = s
		}
		display = append(display, ChainSession{
			ModelID: s.ModelAgentID, SessionID: s.ID, StakeWei: stakeStr,
			EndsAt: time.Unix(s.EndsAt, 0).UTC(), Usable: true,
		})
	}

	adopted := 0
	p.mu.Lock()
	for modelID, s := range best {
		ends := time.Unix(s.EndsAt, 0)
		usableUntil := ends.Add(-expiryBuffer)
		if e, ok := p.entries[modelID]; ok && e.expiresAt.After(usableUntil) {
			// Keep a fresher local open.
			continue
		}
		if st := p.sess[s.ID]; st != nil && (st.retired || st.closing) {
			continue // retired by a failover, release or sweep: never handed out again
		}
		p.entries[modelID] = entry{
			sessionID: s.ID,
			expiresAt: usableUntil,
			stake:     s.Stake,
			provider:  s.Provider,
		}
		if p.sess[s.ID] == nil {
			// Adopted: opener unknown.
			p.newStateLocked(modelID, s.ID, time.Unix(s.OpenedAt, 0), ends, "")
		}
		adopted++
	}
	// Mark which display rows are pooled.
	pooledNow := map[string]bool{}
	for _, e := range p.entries {
		pooledNow[e.sessionID] = true
	}
	p.mu.Unlock()

	for i := range display {
		display[i].Pooled = pooledNow[display[i].SessionID]
	}

	p.chainMu.Lock()
	p.chainOpen = display
	p.chainFetched = now
	p.rehydrateErr = ""
	p.chainMu.Unlock()
	p.stateKnown.Store(true)

	log.Printf("pool: rehydrated %d open on-chain session(s), adopted %d into reuse pool (wallet %s)",
		len(open), adopted, addr)
	return nil
}

func (p *Pool) setRehydrateErr(msg string) {
	p.chainMu.Lock()
	p.rehydrateErr = msg
	p.chainMu.Unlock()
}

// CloseSession closes one session by id (pooled and/or on-chain), then rescans.
func (p *Pool) CloseSession(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("empty session id")
	}
	p.mu.Lock()
	for modelID, e := range p.entries {
		if e.sessionID == sessionID {
			delete(p.entries, modelID)
			break
		}
	}
	p.mu.Unlock()
	if err := p.client.CloseSession(sessionID); err != nil {
		_ = p.RehydrateForce()
		return err
	}
	p.notifyClosed(sessionID)
	return p.RehydrateForce()
}

// CloseAllResult summarizes a mass close attempt.
type CloseAllResult struct {
	Attempted int      `json:"attempted"`
	Closed    int      `json:"closed"`
	Failed    int      `json:"failed"`
	Left      int      `json:"left,omitempty"` // shutdown: not closed in time
	Errors    []string `json:"errors,omitempty"`
}

// CloseAll closes every pooled session, then any remaining on-chain opens
// from the last scan (recovers unused stake sooner than waiting for expiry).
func (p *Pool) CloseAll() CloseAllResult {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]entry{}
	p.mu.Unlock()
	seen := map[string]bool{}
	res := CloseAllResult{}
	tryClose := func(sessionID, label string) {
		if sessionID == "" || seen[sessionID] {
			return
		}
		seen[sessionID] = true
		res.Attempted++
		if err := p.client.CloseSession(sessionID); err != nil {
			res.Failed++
			msg := label + ": close failed" // fixed text; the router's text is logged only
			if len(res.Errors) < 5 {
				res.Errors = append(res.Errors, msg)
			}
			warnf("pool: close session %s: %v", short(sessionID), err)
			return
		}
		res.Closed++
		log.Printf("pool: closed session %s (%s)", short(sessionID), label)
		p.notifyClosed(sessionID)
	}
	for modelID, e := range entries {
		tryClose(e.sessionID, "pooled "+modelID)
	}
	for _, s := range p.ChainOpen() {
		tryClose(s.SessionID, "on-chain "+s.ModelID)
	}
	_ = p.RehydrateForce()
	return res
}

// CloseAllWithin is CloseAll for shutdown: each close gets at most perClose,
// and nothing is attempted after deadline. Sessions not closed in time are
// counted in Left and logged as left for the boot reconcile. It does not
// rescan afterwards.
func (p *Pool) CloseAllWithin(deadline time.Time, perClose time.Duration) CloseAllResult {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]entry{}
	p.mu.Unlock()
	type target struct{ id, label string }
	var targets []target
	seen := map[string]bool{}
	add := func(id, label string) {
		if id != "" && !seen[id] {
			seen[id] = true
			targets = append(targets, target{id, label})
		}
	}
	for modelID, e := range entries {
		add(e.sessionID, "pooled "+modelID)
	}
	for _, s := range p.ChainOpen() {
		add(s.SessionID, "on-chain "+s.ModelID)
	}
	res := CloseAllResult{}
	for i, t := range targets {
		left := time.Until(deadline)
		if left <= 0 {
			for _, r := range targets[i:] {
				res.Left++
				warnf("pool: session %s (%s) left for boot reconcile (shutdown deadline)", short(r.id), r.label)
			}
			break
		}
		wait := perClose
		if left < wait {
			wait = left
		}
		res.Attempted++
		done := make(chan error, 1)
		go func(id string) { done <- p.client.CloseSession(id) }(t.id)
		select {
		case err := <-done:
			if err != nil {
				res.Failed++
				if len(res.Errors) < 5 {
					res.Errors = append(res.Errors, t.label+": close failed")
				}
				warnf("pool: close session %s: %v (left for boot reconcile)", short(t.id), err)
				continue
			}
			res.Closed++
			log.Printf("pool: closed session %s (%s)", short(t.id), t.label)
			p.notifyClosed(t.id)
		case <-time.After(wait):
			res.Left++
			warnf("pool: session %s (%s) left for boot reconcile (close timed out after %s)", short(t.id), t.label, wait)
		}
	}
	return res
}
