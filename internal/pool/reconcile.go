package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Boot-time session reconcile.
//
// A graceful close on SIGTERM is best-effort; a crash, SIGKILL or VM wipe
// leaves sessions open on-chain with their stake locked. On boot, before
// any request may open a session, Uplink lists the wallet's open sessions
// once and decides per session:
//
//   - adopt: the farthest-ending session per configured model that still
//     has more than expiryBuffer left (the same rule the pool uses for
//     reuse). It is used instead of opening a new one.
//   - close "expired": past its end, or inside the expiry buffer.
//   - close "unconfigured": the model is not in the current catalog (only
//     when the catalog was read successfully; otherwise nothing is closed
//     for this reason).
//   - close "duplicate": another usable session for a model that already
//     has an adopted one.
//
// Session opens are gated until the reconcile finishes. If the list call
// fails, it is retried with bounded backoff; after that the gate opens, but
// no new session is opened until some session list succeeds (a request
// that needs a new session fails rather than risk a duplicate), and the
// reconcile keeps retrying in the background.

// ConfiguredModels reports the model ids Uplink serves. ok=false means the
// set is unknown (catalog unavailable).
type ConfiguredModels func() (ids map[string]bool, ok bool)

// ReconcileDecision is the plan for one on-chain session.
type ReconcileDecision struct {
	Session router.Session
	Action  string // adopt | close | keep
	Reason  string // usable | expired | unconfigured | duplicate | unknown-catalog
}

// ReconcileResult is what one successful reconcile did.
type ReconcileResult struct {
	Open     int
	Adopted  int
	Closed   int
	Failed   int
	Skipped  int
	Attempts int
}

// ErrSessionStateUnknown is returned instead of opening a session while the
// wallet's open sessions could not be listed since boot.
var ErrSessionStateUnknown = errors.New("open sessions could not be listed yet; not opening a new session (would risk a duplicate)")

// PlanReconcile decides adopt/close/keep for each open session. Pure.
func PlanReconcile(open []router.Session, now time.Time, configured map[string]bool, catalogKnown bool) []ReconcileDecision {
	buf := int64(expiryBuffer.Seconds())
	best := map[string]int{}
	out := make([]ReconcileDecision, len(open))
	for i, s := range open {
		out[i].Session = s
		switch {
		case s.EndsAt <= now.Unix()+buf:
			out[i].Action, out[i].Reason = "close", "expired"
		case catalogKnown && !configured[s.ModelAgentID]:
			out[i].Action, out[i].Reason = "close", "unconfigured"
		default:
			out[i].Action, out[i].Reason = "adopt", "usable"
			if !catalogKnown {
				out[i].Reason = "unknown-catalog"
			}
			if j, ok := best[s.ModelAgentID]; !ok || s.EndsAt > open[j].EndsAt {
				best[s.ModelAgentID] = i
			}
		}
	}
	for i, s := range open {
		if out[i].Action == "adopt" && best[s.ModelAgentID] != i {
			out[i].Action, out[i].Reason = "close", "duplicate"
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

// reconcileOnce lists open sessions and applies PlanReconcile. Holding
// reconcileMu makes concurrent calls (boot and retries) serialize; adopting
// an already-adopted session or closing an already-closed one is harmless,
// so the operation is idempotent and retry-safe.
func (p *Pool) reconcileOnce(configured ConfiguredModels) (ReconcileResult, error) {
	p.reconcileMu.Lock()
	defer p.reconcileMu.Unlock()
	addr, err := p.client.WalletAddress()
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("wallet address: %w", err)
	}
	open, err := p.client.ListOpenSessions(addr)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("list open sessions: %w", err)
	}
	p.stateKnown.Store(true)
	var ids map[string]bool
	known := false
	if configured != nil {
		ids, known = configured()
	}
	res := ReconcileResult{Open: len(open)}
	for _, d := range PlanReconcile(open, time.Now(), ids, known) {
		s := d.Session
		switch d.Action {
		case "adopt":
			usableUntil := time.Unix(s.EndsAt, 0).Add(-expiryBuffer)
			p.mu.Lock()
			if e, ok := p.entries[s.ModelAgentID]; ok && e.expiresAt.After(usableUntil) {
				p.mu.Unlock()
				res.Skipped++
				continue
			}
			p.entries[s.ModelAgentID] = entry{sessionID: s.ID, expiresAt: usableUntil, stake: s.Stake}
			p.mu.Unlock()
			res.Adopted++
		case "close":
			if err := p.client.CloseSession(s.ID); err != nil {
				log.Printf("pool: reconcile close (%s) session %s: %v", d.Reason, short(s.ID), err)
				res.Failed++
				continue
			}
			res.Closed++
			p.notifyClosed(s.ID)
		default:
			res.Skipped++
		}
	}
	return res, nil
}

// ReconcileConfig bounds the boot reconcile.
type ReconcileConfig struct {
	Backoff     time.Duration // first retry delay (default 2s)
	BackoffMax  time.Duration // cap (default 15s)
	GateFor     time.Duration // keep opens gated at most this long (default 60s)
	RetryEvery  time.Duration // background retry after the gate opened (default 30s)
	OnReconcile func(ReconcileResult)
	OnGiveUp    func(error) // gate released before a successful list
}

// StartupReconcile runs the boot reconcile: retries with bounded backoff
// while opens are gated, releases the gate on success or after GateFor,
// and then keeps retrying in the background until one succeeds. Call Gate
// first. Returns when a reconcile succeeded or ctx is done.
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
	defer p.ReleaseGate()
	deadline := time.Now().Add(cfg.GateFor)
	delay := cfg.Backoff
	attempts := 0
	gated := true
	for {
		attempts++
		res, err := p.reconcileOnce(configured)
		if err == nil {
			res.Attempts = attempts
			p.ReleaseGate()
			if cfg.OnReconcile != nil {
				cfg.OnReconcile(res)
			}
			return
		}
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}
