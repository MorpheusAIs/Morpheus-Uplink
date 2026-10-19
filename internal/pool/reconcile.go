package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Boot-time session reconcile.
//
// A graceful close on SIGTERM is best-effort; a crash, SIGKILL or VM wipe
// leaves sessions open with their stake locked. On boot, before any request
// may open a session, Uplink lists the wallet's open sessions and decides
// per session (PlanReconcile):
//
//   - adopt: the latest-ending session per model that still has more than
//     expiryBuffer left (the pool's reuse rule). It is used instead of
//     opening a new one. Never extended, never re-opened.
//   - close "expired": past its end, or inside the expiry buffer.
//   - close "duplicate": another usable session for a model that already
//     has an adopted one.
//   - skip "unconfigured": the model is not in a non-empty catalog
//     (compared case-insensitively). Never closed at boot; left to expire.
//     An empty catalog, or one that cannot be read, is treated as unknown.
//
// Scope is the wallet: the router does not tell the scoped credential which
// router user opened a session, so a dedicated wallet is required.
//
// Fail safe: a failed or partial list (an error on any page, or a row that
// cannot be verified) means no closes and no opens, with bounded retry.
// After the gate deadline the gate opens, but no new session is opened
// until a list succeeds, and a late recovery closes only expired sessions.
//
// The open gate is released as soon as adopt/close is decided; the closes
// (at most MaxBootCloses, expired first) then run in the background.

// MaxBootCloses caps closes per reconcile pass. The excess is deferred: it
// is left for later scans (expired) or for expiry (duplicate), and the cap
// hit is audited.
const MaxBootCloses = 20

// Decision actions and reasons.
const (
	ActAdopt = "adopt"
	ActClose = "close"
	ActSkip  = "skip"

	ReasonUsable       = "usable"
	ReasonExpired      = "expired"
	ReasonDuplicate    = "duplicate"
	ReasonUnconfigured = "unconfigured"
)

// ConfiguredModels reports the model ids Uplink serves. ok=false means the
// set is unknown (catalog unavailable).
type ConfiguredModels func() (ids map[string]bool, ok bool)

// ReconcileDecision is the plan for one on-chain session.
type ReconcileDecision struct {
	Session router.Session
	Action  string
	Reason  string
}

// ReconcileResult is what one reconcile pass did.
type ReconcileResult struct {
	Open     int
	Adopted  int
	Closed   int
	Failed   int
	Skipped  int
	Deferred int
	Attempts int
	Late     bool // ran after the gate deadline (expired-only closes)
	Catalog  int  // configured models seen; -1 = catalog unknown
}

// ReconcileEvent is one per-session audit event (adopt, close, cap).
type ReconcileEvent struct {
	Kind    string // session_reconcile_adopt | session_reconcile_close | session_reconcile_cap
	Reason  string
	Session string // short id prefix
	Result  string // ok | failed
	Count   int    // cap: deferred count
	Source  string // boot | rescan
}

// ReconcileState is the latest reconcile status (for /admin/status, the GUI
// and the status file).
type ReconcileState struct {
	State     string    `json:"state"` // pending | reconciling | closing | ok | partial | deferred | retrying
	At        time.Time `json:"at,omitempty"`
	Attempts  int       `json:"attempts"`
	Open      int       `json:"open"`
	Adopted   int       `json:"adopted"`
	Closed    int       `json:"closed"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	Deferred  int       `json:"deferred"`
	OpensHeld bool      `json:"opensHeld"` // new opens refused until a list succeeds
	LastError string    `json:"lastError,omitempty"`
}

// ErrSessionStateUnknown is returned instead of opening a session while the
// wallet's open sessions could not be listed since boot.
var ErrSessionStateUnknown = errors.New("open sessions could not be listed yet; not opening a new session (would risk a duplicate)")

// errUnverifiable marks a list that came back but cannot be trusted whole.
var errUnverifiable = errors.New("session list incomplete")

func normModel(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// PlanReconcile decides adopt/close/skip for each open session. Pure.
// configured is honoured only when catalogKnown and non-empty.
func PlanReconcile(open []router.Session, now time.Time, configured map[string]bool, catalogKnown bool) []ReconcileDecision {
	cfg := map[string]bool{}
	for id, ok := range configured {
		if ok {
			cfg[normModel(id)] = true
		}
	}
	useCatalog := catalogKnown && len(cfg) > 0
	buf := int64(expiryBuffer.Seconds())
	best := map[string]int{}
	out := make([]ReconcileDecision, len(open))
	for i, s := range open {
		out[i].Session = s
		m := normModel(s.ModelAgentID)
		switch {
		case s.EndsAt <= now.Unix()+buf:
			out[i].Action, out[i].Reason = ActClose, ReasonExpired
		case useCatalog && !cfg[m]:
			out[i].Action, out[i].Reason = ActSkip, ReasonUnconfigured
		default:
			out[i].Action, out[i].Reason = ActAdopt, ReasonUsable
			if j, ok := best[m]; !ok || s.EndsAt > open[j].EndsAt {
				best[m] = i
			}
		}
	}
	for i, s := range open {
		if out[i].Action == ActAdopt && best[normModel(s.ModelAgentID)] != i {
			out[i].Action, out[i].Reason = ActClose, ReasonDuplicate
		}
	}
	return out
}

// Gate blocks session opens until ReleaseGate (call before serving).
func (p *Pool) Gate() {
	p.mu.Lock()
	if p.gate == nil {
		p.gate = make(chan struct{})
	}
	p.gated.Store(true)
	p.mu.Unlock()
	p.setState(func(s *ReconcileState) { s.State = "pending" })
}

// ReleaseGate lets session opens proceed. Safe to call more than once.
func (p *Pool) ReleaseGate() {
	p.mu.Lock()
	if p.gate != nil {
		select {
		case <-p.gate:
		default:
			close(p.gate)
		}
	}
	p.mu.Unlock()
}

func (p *Pool) gateClosed() bool {
	p.mu.Lock()
	g := p.gate
	p.mu.Unlock()
	if g == nil {
		return false
	}
	select {
	case <-g:
		return false
	default:
		return true
	}
}

func (p *Pool) waitGate() {
	p.mu.Lock()
	g := p.gate
	p.mu.Unlock()
	if g != nil {
		<-g
	}
}

// ReconcileStatus returns the latest reconcile state.
func (p *Pool) ReconcileStatus() ReconcileState {
	p.recMu.Lock()
	defer p.recMu.Unlock()
	s := p.recState
	s.OpensHeld = p.gated.Load() && !p.stateKnown.Load()
	return s
}

func (p *Pool) setState(f func(*ReconcileState)) {
	p.recMu.Lock()
	f(&p.recState)
	s := p.recState
	cb := p.onState
	p.recMu.Unlock()
	if cb != nil {
		s.OpensHeld = p.gated.Load() && !p.stateKnown.Load()
		cb(s)
	}
}

// verifiable is true when every open row can be acted on safely.
func verifiable(open []router.Session) bool {
	for _, s := range open {
		if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.ModelAgentID) == "" || s.EndsAt <= 0 || s.ClosedAt != 0 {
			return false
		}
	}
	return true
}

// plan lists open sessions and applies the adopt decisions. It returns the
// closes to run (capped, expired first). Any list error or an unverifiable
// row returns an error and changes nothing.
func (p *Pool) plan(configured ConfiguredModels, late bool, emit func(ReconcileEvent)) (ReconcileResult, []ReconcileDecision, error) {
	addr, err := p.client.WalletAddress()
	if err != nil {
		return ReconcileResult{}, nil, fmt.Errorf("wallet address: %w", err)
	}
	open, err := p.client.ListOpenSessions(addr)
	if err != nil {
		return ReconcileResult{}, nil, fmt.Errorf("list open sessions: %w", err)
	}
	if !verifiable(open) {
		return ReconcileResult{}, nil, errUnverifiable
	}
	var ids map[string]bool
	known := false
	if configured != nil {
		ids, known = configured()
	}
	res := ReconcileResult{Open: len(open), Late: late, Catalog: -1}
	if known {
		res.Catalog = len(ids)
	}
	var expired, dups []ReconcileDecision
	for _, d := range PlanReconcile(open, time.Now(), ids, known) {
		s := d.Session
		switch {
		case d.Action == ActAdopt:
			usableUntil := time.Unix(s.EndsAt, 0).Add(-expiryBuffer)
			key := normModel(s.ModelAgentID)
			p.mu.Lock()
			if e, ok := p.entries[key]; ok && e.expiresAt.After(usableUntil) {
				p.mu.Unlock()
				res.Skipped++
				continue
			}
			p.entries[key] = entry{sessionID: s.ID, expiresAt: usableUntil, stake: s.Stake}
			p.mu.Unlock()
			res.Adopted++
			emit(ReconcileEvent{Kind: "session_reconcile_adopt", Reason: ReasonUsable, Session: short(s.ID), Result: "ok", Source: "boot"})
		case d.Action == ActClose && d.Reason == ReasonExpired:
			expired = append(expired, d)
		case d.Action == ActClose && d.Reason == ReasonDuplicate && !late:
			dups = append(dups, d)
		default:
			// unconfigured, or a duplicate found by a late recovery (the
			// pool may already be serving it): left to expire.
			res.Skipped++
		}
	}
	closes := append(expired, dups...)
	if len(closes) > MaxBootCloses {
		res.Deferred = len(closes) - MaxBootCloses
		closes = closes[:MaxBootCloses]
		emit(ReconcileEvent{Kind: "session_reconcile_cap", Reason: fmt.Sprintf("max %d closes per pass", MaxBootCloses), Result: "deferred", Count: res.Deferred, Source: "boot"})
	}
	p.stateKnown.Store(true)
	return res, closes, nil
}

func (p *Pool) isClosing(id string) bool {
	p.recMu.Lock()
	defer p.recMu.Unlock()
	return p.closing[id]
}

func (p *Pool) runCloses(res *ReconcileResult, closes []ReconcileDecision, emit func(ReconcileEvent)) {
	p.recMu.Lock()
	if p.closing == nil {
		p.closing = map[string]bool{}
	}
	for _, d := range closes {
		p.closing[d.Session.ID] = true
	}
	p.recMu.Unlock()
	defer func() {
		p.recMu.Lock()
		for _, d := range closes {
			delete(p.closing, d.Session.ID)
		}
		p.recMu.Unlock()
	}()
	for _, d := range closes {
		ev := ReconcileEvent{Kind: "session_reconcile_close", Reason: d.Reason, Session: short(d.Session.ID), Result: "ok", Source: "boot"}
		if err := p.client.CloseSession(d.Session.ID); err != nil {
			log.Printf("pool: reconcile close (%s) session %s: %v", d.Reason, short(d.Session.ID), err)
			res.Failed++
			ev.Result = "failed"
		} else {
			res.Closed++
			p.notifyClosed(d.Session.ID)
		}
		emit(ev)
	}
}

// reconcileOnce runs one full pass synchronously (plan + closes).
func (p *Pool) reconcileOnce(configured ConfiguredModels) (ReconcileResult, error) {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()
	res, closes, err := p.plan(configured, false, func(ReconcileEvent) {})
	if err != nil {
		return res, err
	}
	p.runCloses(&res, closes, func(ReconcileEvent) {})
	return res, nil
}

// ReconcileConfig bounds the boot reconcile.
type ReconcileConfig struct {
	Backoff     time.Duration // first retry delay (default 2s)
	BackoffMax  time.Duration // cap (default 15s)
	GateFor     time.Duration // keep opens gated at most this long (default 60s)
	RetryEvery  time.Duration // background retry after the gate opened (default 30s)
	OnReconcile func(ReconcileResult)
	OnEvent     func(ReconcileEvent)
	OnGiveUp    func(error) // gate released before a successful list
}

// StartupReconcile runs the boot reconcile: retries with bounded backoff
// while opens are gated, releases the gate once adopt/close is decided (or
// after GateFor), runs the closes, and keeps retrying in the background
// until one pass succeeds. Call Gate first. Returns when a pass completed
// or ctx is done.
func (p *Pool) StartupReconcile(ctx context.Context, configured ConfiguredModels, cfg ReconcileConfig) {
	if cfg.Backoff <= 0 {
		cfg.Backoff = 2 * time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 15 * time.Second
	}
	if cfg.GateFor <= 0 {
		cfg.GateFor = 60 * time.Second
	}
	if cfg.RetryEvery <= 0 {
		cfg.RetryEvery = 30 * time.Second
	}
	var evMu sync.Mutex
	emit := func(ev ReconcileEvent) {
		if cfg.OnEvent != nil {
			evMu.Lock()
			cfg.OnEvent(ev)
			evMu.Unlock()
		}
	}
	defer p.ReleaseGate()
	deadline := time.Now().Add(cfg.GateFor)
	delay := cfg.Backoff
	attempts := 0
	gated := true
	p.setState(func(s *ReconcileState) { s.State = "reconciling" })
	for {
		attempts++
		p.reconcileMu.Lock()
		res, closes, err := p.plan(configured, !gated, emit)
		if err == nil {
			res.Attempts = attempts
			p.ReleaseGate() // decided: opens may proceed while closes run
			p.setState(func(s *ReconcileState) {
				*s = ReconcileState{State: "closing", At: time.Now().UTC(), Attempts: attempts,
					Open: res.Open, Adopted: res.Adopted, Skipped: res.Skipped, Deferred: res.Deferred}
			})
			p.runCloses(&res, closes, emit)
			p.reconcileMu.Unlock()
			st := "ok"
			if res.Failed > 0 || res.Deferred > 0 {
				st = "partial"
			}
			p.setState(func(s *ReconcileState) {
				*s = ReconcileState{State: st, At: time.Now().UTC(), Attempts: attempts, Open: res.Open,
					Adopted: res.Adopted, Closed: res.Closed, Failed: res.Failed, Skipped: res.Skipped, Deferred: res.Deferred}
			})
			if cfg.OnReconcile != nil {
				cfg.OnReconcile(res)
			}
			return
		}
		p.reconcileMu.Unlock()
		log.Printf("pool: boot session reconcile attempt %d: %v", attempts, err)
		wait := delay
		if gated {
			if time.Now().Add(wait).After(deadline) {
				gated = false
				p.ReleaseGate()
				if cfg.OnGiveUp != nil {
					cfg.OnGiveUp(err)
				}
				wait = cfg.RetryEvery
			} else if delay *= 2; delay > cfg.BackoffMax {
				delay = cfg.BackoffMax
			}
		} else {
			wait = cfg.RetryEvery
		}
		state := "reconciling"
		if !gated {
			state = "deferred"
			if attempts > 1 && p.ReconcileStatus().State == "deferred" {
				state = "retrying"
			}
		}
		msg := err.Error()
		p.setState(func(s *ReconcileState) { s.State, s.Attempts, s.LastError = state, attempts, msg })
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// ShortID is the log form of a session id (first 10 characters).
func ShortID(id string) string { return short(id) }

// ConfiguredFrom adapts a model-id lister (the catalog) to ConfiguredModels.
// A list error, or an empty list (e.g. {"models":[]} or {}), is "unknown".
func ConfiguredFrom(list func() ([]string, error)) ConfiguredModels {
	return func() (map[string]bool, bool) {
		ids, err := list()
		if err != nil {
			log.Printf("pool: reconcile: catalog unavailable, treating as unknown: %v", err)
			return nil, false
		}
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			if id = normModel(id); id != "" {
				out[id] = true
			}
		}
		if len(out) == 0 {
			log.Printf("pool: reconcile: catalog is empty, treating as unknown")
			return nil, false
		}
		return out, true
	}
}
