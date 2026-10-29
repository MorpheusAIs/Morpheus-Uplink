// Package pool keeps at most one open proxy-router session per model and
// reuses it across requests until it nears its on-chain expiry.
package pool

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// expiryBuffer stops using a session this long before its on-chain end so
// we never send a prompt into a session that expires mid-answer.
const expiryBuffer = 90 * time.Second

// pruneGrace: a rescan never prunes a pool slot opened less than this long
// before the scan started, so an open the chain read does not show yet is
// not dropped (and then opened again, locking a second stake). Two router
// call bounds: the open's own call and the list call can each take up to
// router.AdminTimeout, so a read lagging behind an open stays inside it.
const pruneGrace = 2 * router.AdminTimeout

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
	// exclude: the providerExclude setting, lowercase provider addresses
	// never opened for any model.
	exclude map[string]bool
	// healthy: the healthy-bid source (catalog.HealthyBidIDs): lowercase
	// bid ids of the model a gateway health feed marks healthy. nil:
	// health unknown, every open is refused.
	healthy func(modelID string) (map[string]struct{}, error)

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
	recState    ReconcileState
	onState     func(ReconcileState)
	onRescan    func(ReconcileEvent)

	// Last on-chain open-session scan (for GUI), refreshed by Rehydrate.
	chainMu      sync.Mutex
	chainOpen    []ChainSession
	chainFetched time.Time
	rehydrateErr string
	// lastScanLog: the counts the last "rehydrated" log line reported; the
	// line repeats only when they change (status polls rescan often).
	lastScanLog *scanCounts

	// Wallet slot and open reconcile (slot.go, transient.go, sweep.go).
	slot    walletSlot
	sleepFn func(ctx context.Context, d time.Duration) error // SetSleep (tests)
	// Under mu:
	pending      []*attempt           // unresolved open attempts (maybe sent)
	knownDup     map[string]bool      // lowercase id: an extra session from our own open (the sweep closes it)
	closingIDs   map[string]time.Time // lowercase id -> mark expiry; zero: a close is in progress
	closeDueAt   map[string]time.Time // lowercase id -> when the sweep first saw it close-due
	busyUntil    time.Time            // router-busy window (a cut open), pool clock
	busyAttempt  *attempt             // the attempt that set it
	opened       map[string]bool      // lowercase ids this process opened or matched to its own attempt
	closedByUs   map[string]bool      // lowercase ids this process closed
	foreignAt    []time.Time          // adoptions of sessions this process never opened (last hour)
	sweepSkips   int                  // busy ScanOrphans ticks since one held the slot
	retiredSkips int                  // busy retired-close tries since one held the slot
	started      time.Time            // pool clock at New or SetClock (uptime)
	warnAt       map[string]time.Time
	warnSkipped  map[string]int
	walletAddr   string
	scanKick     chan struct{} // buffered 1: run the sweep now
	rpcURLs      atomic.Int64  // router RPC URL count (GET /config); 0: unknown
	retiredBusy  atomic.Bool   // a retired close found the wallet slot busy
}

// scanCounts is what the "rehydrated" log line reports.
type scanCounts struct {
	open, adopted int
	wallet        string
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

		knownDup:    map[string]bool{},
		closingIDs:  map[string]time.Time{},
		closeDueAt:  map[string]time.Time{},
		opened:      map[string]bool{},
		closedByUs:  map[string]bool{},
		warnAt:      map[string]time.Time{},
		warnSkipped: map[string]int{},
		scanKick:    make(chan struct{}, 1),
		started:     time.Now(),
	}
	p.slot.now = p.now
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

// Rehydrate is the status poll's scan: it lists the wallet's open
// sessions and adopts one usable unheld session per model (never a young
// one that may be an open of ours still in flight). It never closes
// anything; the sweep closes. It also refreshes the router's RPC URL count.
//
// It skips work when a scan ran recently.
func (p *Pool) Rehydrate() error {
	return p.rehydrate(false)
}

// RehydrateForce always rescans (adopt-only, like Rehydrate).
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
		p.RefreshRPCURLCount(context.Background())
	}
	_, err := p.scan(context.Background(), nil, scanAdoptOnly)
	return err
}

func (p *Pool) setRehydrateErr(msg string) {
	p.chainMu.Lock()
	p.rehydrateErr = msg
	p.chainMu.Unlock()
}

// CloseSession closes one session by id (admin): it waits for the wallet
// slot, closes, then rescans (adopt-only) outside the slot.
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
	ctx := context.Background()
	tok, err := p.slot.acquire(ctx, "")
	if err != nil {
		return err
	}
	_, cerr := p.closeWithToken(ctx, tok, sessionID, "", "", closeCallTimeout)
	p.slot.release(tok)
	rerr := p.RehydrateForce()
	if cerr != nil {
		return cerr
	}
	return rerr
}

// CloseAllResult summarizes a mass close attempt.
type CloseAllResult struct {
	Attempted int      `json:"attempted"`
	Closed    int      `json:"closed"`
	Failed    int      `json:"failed"`
	Left      int      `json:"left,omitempty"` // shutdown: not closed in time
	Errors    []string `json:"errors,omitempty"`
}

type closeTarget struct{ id, label string }

// closeTargets empties the pool and lists every pooled and last-scanned
// on-chain session once.
func (p *Pool) closeTargets() []closeTarget {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]entry{}
	p.mu.Unlock()
	var targets []closeTarget
	seen := map[string]bool{}
	add := func(id, label string) {
		if id != "" && !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			targets = append(targets, closeTarget{id, label})
		}
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(entries[k].sessionID, "pooled "+k)
	}
	for _, s := range p.ChainOpen() {
		add(s.SessionID, "on-chain "+s.ModelID)
	}
	return targets
}

// CloseAll closes every pooled session, then any remaining on-chain opens
// from the last scan (recovers unused stake sooner than waiting for
// expiry). Each close waits for the wallet slot; a rescan follows.
func (p *Pool) CloseAll() CloseAllResult {
	res := CloseAllResult{}
	ctx := context.Background()
	for _, t := range p.closeTargets() {
		res.Attempted++
		tok, err := p.slot.acquire(ctx, "")
		if err == nil {
			var out closeOutcome
			out, err = p.closeWithToken(ctx, tok, t.id, "", "", closeCallTimeout)
			p.slot.release(tok)
			if out == closeOK || out == closeAlready {
				res.Closed++
				log.Printf("pool: closed session %s (%s)", short(t.id), t.label)
				continue
			}
		}
		res.Failed++
		if len(res.Errors) < 5 {
			res.Errors = append(res.Errors, t.label+": close failed") // fixed text; the router's text is logged only
		}
		warnf("pool: close session %s: %v", short(t.id), err)
	}
	_ = p.RehydrateForce()
	return res
}

// CloseAllWithin is CloseAll for shutdown: each close gets at most perClose,
// and nothing is attempted after deadline. Sessions not closed in time are
// counted in Left and logged as left for the boot reconcile. It does not
// rescan afterwards.
func (p *Pool) CloseAllWithin(deadline time.Time, perClose time.Duration) CloseAllResult {
	targets := p.closeTargets()
	res := CloseAllResult{}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
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
		tok, err := p.slot.acquire(ctx, "")
		if err != nil {
			res.Left++
			warnf("pool: session %s (%s) left for boot reconcile (wallet slot busy)", short(t.id), t.label)
			continue
		}
		res.Attempted++
		out, cerr := p.closeWithToken(ctx, tok, t.id, "", "", wait)
		p.slot.release(tok)
		switch out {
		case closeOK, closeAlready:
			res.Closed++
			log.Printf("pool: closed session %s (%s)", short(t.id), t.label)
		case closeCut:
			res.Left++
			warnf("pool: session %s (%s) left for boot reconcile (close timed out after %s)", short(t.id), t.label, wait)
		default:
			res.Failed++
			if len(res.Errors) < 5 {
				res.Errors = append(res.Errors, t.label+": close failed")
			}
			warnf("pool: close session %s: %v (left for boot reconcile)", short(t.id), cerr)
		}
	}
	return res
}
