package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Open attempts, the wallet scan, closes and the sweep.
//
// Every close Uplink sends runs while holding the wallet slot (slot.go), in
// the sweep, the boot plan or an admin or shutdown close. A scan decides
// adoption and the one close it may start in a single critical section
// under p.mu, and marks that id closing there, so a session is never
// adopted and closed by two paths at once.
//
// Session ages:
//   - young (opened less than pruneGrace ago) and not ours: left alone by
//     every scan (an open of ours may still be in flight), except ids
//     already known to be extra sessions from our own opens;
//   - older and unheld: one per model is adopted (cooling providers too),
//     the rest are closed by the sweep.

// attempt is one open POST whose outcome is not known yet.
type attempt struct {
	model string    // normModel
	bid   string    // lowercase bid id
	start time.Time // pool clock, taken before the POST
	cut   bool      // Uplink stopped waiting on it (it set the router-busy window)
}

// matches reports whether an on-chain session can be the result of a.
func (a *attempt) matches(s router.Session) bool {
	if normModel(s.ModelAgentID) != a.model || !strings.EqualFold(strings.TrimSpace(s.BidID), a.bid) {
		return false
	}
	op := time.Unix(s.OpenedAt, 0)
	return !op.Before(a.start.Add(-attemptSkew)) && !op.After(a.start.Add(routerOpenMaxLife+attemptSkew))
}

// addAttempt records an open POST about to be sent.
func (p *Pool) addAttempt(model, bid string) *attempt {
	a := &attempt{model: normModel(model), bid: strings.ToLower(strings.TrimSpace(bid)), start: p.now()}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneAttemptsLocked(a.start)
	if len(p.pending) >= maxPendingAttempts {
		old := p.pending[0]
		p.pending = p.pending[1:]
		slog.Warn("pool: open attempt list full; dropping the oldest", "bid", short(old.bid))
	}
	p.pending = append(p.pending, a)
	return a
}

// dropAttempt removes a (a definite answer: the open did not happen, or
// it returned a session id).
func (p *Pool) dropAttempt(a *attempt) {
	p.mu.Lock()
	p.resolveAttemptLocked(a)
	p.mu.Unlock()
}

func (p *Pool) resolveAttemptLocked(a *attempt) {
	for i, x := range p.pending {
		if x == a {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			break
		}
	}
	if p.busyAttempt == a {
		p.busyUntil, p.busyAttempt = time.Time{}, nil
		slog.Info("pool: router-busy window cleared (the cut open was found)", "bid", short(a.bid))
	}
}

func (p *Pool) pruneAttemptsLocked(now time.Time) {
	keep := p.pending[:0]
	for _, a := range p.pending {
		if now.Before(a.start.Add(attemptKeep + attemptSkew)) {
			keep = append(keep, a)
		}
	}
	for i := len(keep); i < len(p.pending); i++ {
		p.pending[i] = nil
	}
	p.pending = keep
}

// matchAttemptLocked is the pending attempt s resolves, if any.
func (p *Pool) matchAttemptLocked(s router.Session) *attempt {
	for _, a := range p.pending {
		if a.matches(s) {
			return a
		}
	}
	return nil
}

// pickOne chooses the session to keep among several: latest EndsAt, then
// later OpenedAt, then lowest id.
func pickOne(ss []router.Session) router.Session {
	best := ss[0]
	for _, s := range ss[1:] {
		switch {
		case s.EndsAt != best.EndsAt:
			if s.EndsAt > best.EndsAt {
				best = s
			}
		case s.OpenedAt != best.OpenedAt:
			if s.OpenedAt > best.OpenedAt {
				best = s
			}
		case strings.ToLower(s.ID) < strings.ToLower(best.ID):
			best = s
		}
	}
	return best
}

// Router-busy window: set only by a cut open (Uplink stopped waiting, or a
// transport error or 502/503/504 on the open). The router may still be
// sending that open's transaction, so no new open starts until the open is
// found on chain or routerOpenMaxLife passes. Pooled leases still work.

func (p *Pool) routerBusy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routerBusyLocked(p.now())
}

func (p *Pool) routerBusyLocked(now time.Time) bool {
	if p.busyUntil.IsZero() {
		return false
	}
	if !now.Before(p.busyUntil) {
		p.busyUntil, p.busyAttempt = time.Time{}, nil
		return false
	}
	return true
}

func (p *Pool) setRouterBusy(a *attempt) {
	p.mu.Lock()
	now := p.now()
	a.cut = true
	until := now.Add(routerOpenMaxLife)
	if until.After(p.busyUntil) {
		p.busyUntil, p.busyAttempt = until, a
	}
	p.mu.Unlock()
	slog.Warn("pool: router busy after a cut open; new opens wait until it is found or expires",
		"bid", short(a.bid), "for", routerOpenMaxLife)
	p.requestSweep()
}

// cutCloseMarkLocked: a cut close is still pending (its mark has not
// cleared). Other background closes wait for it.
func (p *Pool) cutCloseMarkLocked(now time.Time) bool {
	for id, exp := range p.closingIDs {
		if exp.IsZero() {
			continue
		}
		if !now.Before(exp) {
			delete(p.closingIDs, id)
			continue
		}
		return true
	}
	return false
}

// cutBidLocked: a pending cut attempt is for this bid.
func (p *Pool) cutBidLocked(bid string) bool {
	for _, a := range p.pending {
		if a.cut && strings.EqualFold(a.bid, strings.TrimSpace(bid)) {
			return true
		}
	}
	return false
}

func (p *Pool) closingMarkedLocked(id string) bool {
	_, ok := p.closingIDs[strings.ToLower(id)]
	return ok
}

// requestSweep asks the sweep loop to run soon (non-blocking).
func (p *Pool) requestSweep() {
	select {
	case p.scanKick <- struct{}{}:
	default:
	}
}

// rateWarn logs at most once per sharedWalletWarnEvery per key, counting
// what it suppressed.
func (p *Pool) rateWarn(key, msg string, args ...any) {
	p.mu.Lock()
	now := p.now()
	last, seen := p.warnAt[key]
	if seen && now.Sub(last) < sharedWalletWarnEvery {
		p.warnSkipped[key]++
		p.mu.Unlock()
		return
	}
	skipped := p.warnSkipped[key]
	p.warnAt[key] = now
	p.warnSkipped[key] = 0
	p.mu.Unlock()
	if skipped > 0 {
		args = append(args, "suppressed", skipped)
	}
	slog.Warn(msg, args...)
}

// SetProvidedRPC records whether the operator set the router's chain RPC
// (ETH_NODE_ADDRESS has a value). Only the presence is kept: the value is
// never passed in, stored or logged. With it set, a chain-stage error
// before the open is sent is retried on the same bid with a capped
// backoff; without it, the router's own RPC list and retries apply, and
// Uplink makes one attempt and answers router_transient.
func (p *Pool) SetProvidedRPC(set bool) { p.providedRPC.Store(set) }

// ProvidedRPC reports what SetProvidedRPC recorded.
func (p *Pool) ProvidedRPC() bool { return p.providedRPC.Load() }

// closeCand is one session the sweep closes.
type closeCand struct {
	id, reason string
}

const (
	scanAdoptOnly = iota // status poll, forced rescans, the pre-open scan
	scanSweep            // the sweep: holds the slot and may pick one close
)

type scanOut struct {
	open    int
	adopted int // adopted by this scan
	pooled  int // listed sessions in the pool after it
	wallet  string
	close   *closeCand // sweep: the close picked (already marked closing)
	due     int        // close-due ids after this scan
	oldest  time.Duration
	matched bool // a pending attempt was resolved
}

// readOpen lists the wallet's open sessions; mk bounds each router call.
func (p *Pool) readOpen(mk func() (context.Context, context.CancelFunc)) ([]router.Session, string, error) {
	p.mu.Lock()
	addr := p.walletAddr
	p.mu.Unlock()
	if addr == "" {
		ctx, cancel := mk()
		a, err := p.client.WalletAddressCtx(ctx)
		cancel()
		if err != nil {
			return nil, "", err
		}
		addr = a
		p.mu.Lock()
		p.walletAddr = a
		p.mu.Unlock()
	}
	ctx, cancel := mk()
	open, err := p.client.ListOpenSessionsCtx(ctx, addr)
	cancel()
	if err != nil {
		return nil, addr, err
	}
	return open, addr, nil
}

func (p *Pool) retiredSet() map[string]bool {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	out := make(map[string]bool, len(p.retired))
	for id := range p.retired {
		out[strings.ToLower(id)] = true
	}
	return out
}

// scan lists the wallet's open sessions and applies them (see applyScan).
// tok != nil bounds each read by the slot hold.
func (p *Pool) scan(ctx context.Context, tok *slotToken, mode int) (scanOut, error) {
	scanStart := p.now()
	mk := func() (context.Context, context.CancelFunc) {
		if tok != nil {
			return p.slot.callCtx(ctx, tok, readCallTimeout)
		}
		return context.WithTimeout(ctx, router.AdminTimeout)
	}
	open, addr, err := p.readOpen(mk)
	if err != nil {
		p.setRehydrateErr(router.SafeError(err))
		return scanOut{}, err
	}
	out := p.applyScan(open, scanStart, mode)
	out.wallet = addr

	p.chainMu.Lock()
	p.rehydrateErr = ""
	counts := scanCounts{open: len(open), adopted: out.pooled, wallet: addr}
	changed := p.lastScanLog == nil || *p.lastScanLog != counts
	p.lastScanLog = &counts
	p.chainMu.Unlock()
	p.stateKnown.Store(true)
	if changed {
		log.Printf("pool: rehydrated %d open on-chain session(s), adopted %d into reuse pool (wallet %s)",
			len(open), out.pooled, addr)
	}
	return out, nil
}

// applyScan applies one open-session list: it resolves pending attempts,
// adopts, prunes pool slots closed elsewhere, refreshes the display, and in
// sweep mode records what is close-due and picks at most one close.
func (p *Pool) applyScan(open []router.Session, scanStart time.Time, mode int) scanOut {
	excluded := p.excludedProviders()
	retired := p.retiredSet()
	var closedLate []string
	var shared []any // triples of (session, bid, trigger)
	out := scanOut{open: len(open)}

	p.mu.Lock()
	now := p.now()
	nowUnix := now.Unix()
	buf := int64(expiryBuffer.Seconds())
	uptimeOK := now.Sub(p.started) > pruneGrace
	onChain := make(map[string]bool, len(open))
	for _, s := range open {
		onChain[strings.ToLower(s.ID)] = true
	}
	// A cut close resolved (gone from the list) or its mark expired.
	for id, exp := range p.closingIDs {
		if exp.IsZero() {
			continue
		}
		if !onChain[id] {
			delete(p.closingIDs, id)
			closedLate = append(closedLate, id)
		} else if !now.Before(exp) {
			delete(p.closingIDs, id)
		}
	}
	p.pruneAttemptsLocked(now)

	held := map[string]bool{}
	for _, e := range p.entries {
		held[strings.ToLower(e.sessionID)] = true
	}
	for id, st := range p.sess {
		if st.retired && !st.closing && st.inflight == 0 {
			// Retired with nothing in flight counts as unheld: it is
			// close-due as "retired" (after the young-age rule).
			continue
		}
		held[strings.ToLower(id)] = true // tracked by leases (in flight, or closing)
	}
	usableEntry := func(key string) bool {
		e, ok := p.entries[key]
		if !ok || !now.Before(e.expiresAt) || p.closingMarkedLocked(e.sessionID) {
			return false
		}
		if st := p.sess[e.sessionID]; st != nil && (st.retired || st.closing) {
			return false
		}
		return true
	}

	type cand struct {
		s      router.Session
		reason string
	}
	var due []cand
	matched := map[string][]router.Session{}
	older := map[string][]router.Session{}
	for _, s := range open {
		lid := strings.ToLower(s.ID)
		usable := s.EndsAt > nowUnix+buf
		if p.closingMarkedLocked(s.ID) {
			continue
		}
		if held[lid] {
			// An idle pool slot past its on-chain end holds stake for
			// nothing: it is close-due as expired (a lease never takes a
			// marked id).
			if st := p.sess[s.ID]; !usable && (st == nil || (st.inflight == 0 && !st.closing && !st.retired)) {
				due = append(due, cand{s, ReasonExpired})
			}
			continue
		}
		if a := p.matchAttemptLocked(s); a != nil {
			p.resolveAttemptLocked(a)
			p.opened[lid] = true
			out.matched = true
			if usable {
				matched[a.model] = append(matched[a.model], s)
			} else {
				due = append(due, cand{s, ReasonExpired})
			}
			continue
		}
		if st := p.sess[s.ID]; st != nil && st.retired {
			retired[lid] = true // released or failed over, nothing in flight
		}
		young := time.Unix(s.OpenedAt, 0).After(now.Add(-pruneGrace))
		switch {
		case !usable:
			due = append(due, cand{s, ReasonExpired})
		case p.knownDup[lid]:
			due = append(due, cand{s, ReasonDuplicate})
		case young:
			// Possibly an open of ours still in flight: left alone. Opened
			// within the last sweep interval with no attempt of ours to
			// match it: another opener on this wallet.
			if mode == scanSweep && uptimeOK && time.Unix(s.OpenedAt, 0).After(now.Add(-orphanScanEvery)) && !p.opened[lid] && !p.cutBidLocked(s.BidID) {
				shared = append(shared, s.ID, s.BidID, "a")
			}
		case retired[lid]:
			due = append(due, cand{s, "retired"})
		case excluded[normProv(s.Provider)]:
			due = append(due, cand{s, ReasonExcluded})
		default:
			k := normModel(s.ModelAgentID)
			older[k] = append(older[k], s)
		}
	}
	adopt := func(s router.Session) {
		key := normModel(s.ModelAgentID)
		ends := time.Unix(s.EndsAt, 0)
		p.entries[key] = entry{sessionID: s.ID, expiresAt: ends.Add(-expiryBuffer), stake: s.Stake, provider: normProv(s.Provider)}
		if p.sess[s.ID] == nil {
			p.newStateLocked(key, s.ID, time.Unix(s.OpenedAt, 0), ends, "")
		}
		out.adopted++
	}
	for key, ss := range matched {
		if usableEntry(key) {
			for _, s := range ss {
				p.knownDup[strings.ToLower(s.ID)] = true
				due = append(due, cand{s, ReasonDuplicate})
			}
			continue
		}
		keep := pickOne(ss)
		adopt(keep)
		for _, s := range ss {
			if s.ID != keep.ID {
				p.knownDup[strings.ToLower(s.ID)] = true
				due = append(due, cand{s, ReasonDuplicate})
			}
		}
		// Older sessions of the same model are extra now.
		for _, s := range older[key] {
			due = append(due, cand{s, ReasonDuplicate})
		}
		delete(older, key)
	}
	for key, ss := range older {
		if usableEntry(key) {
			for _, s := range ss {
				due = append(due, cand{s, ReasonDuplicate})
			}
			continue
		}
		keep := pickOne(ss)
		adopt(keep)
		lid := strings.ToLower(keep.ID)
		if !p.opened[lid] && uptimeOK {
			p.foreignAt = append(p.foreignAt, now)
		}
		for _, s := range ss {
			if s.ID != keep.ID {
				due = append(due, cand{s, ReasonDuplicate})
			}
		}
	}
	// Foreign adoptions in the last hour.
	fa := p.foreignAt[:0]
	for _, t := range p.foreignAt {
		if now.Sub(t) < time.Hour {
			fa = append(fa, t)
		}
	}
	p.foreignAt = fa
	foreignN := len(fa)

	// Prune pool slots whose session is not in the list (closed
	// elsewhere): only with nothing in flight, and never a session opened
	// after this scan started or within pruneGrace before it (the list may
	// lag behind it).
	keepAfter := scanStart.Add(-pruneGrace)
	for modelID, e := range p.entries {
		if onChain[strings.ToLower(e.sessionID)] {
			continue
		}
		st := p.sess[e.sessionID]
		if st != nil && (st.inflight > 0 || st.openedAt.After(keepAfter)) {
			continue
		}
		delete(p.entries, modelID)
		if mode == scanSweep && uptimeOK && !p.closedByUs[strings.ToLower(e.sessionID)] && e.expiresAt.Add(expiryBuffer).After(now) {
			shared = append(shared, e.sessionID, "", "b")
		}
	}
	// Known duplicates that are gone need no memory.
	for id := range p.knownDup {
		if !onChain[id] {
			delete(p.knownDup, id)
		}
	}

	if mode == scanSweep {
		dueNow := map[string]bool{}
		for _, c := range due {
			lid := strings.ToLower(c.s.ID)
			dueNow[lid] = true
			if _, ok := p.closeDueAt[lid]; !ok {
				p.closeDueAt[lid] = now
			}
		}
		for id := range p.closeDueAt {
			if !dueNow[id] {
				delete(p.closeDueAt, id)
			}
		}
		out.due = len(due)
		out.oldest = p.oldestDueLocked(now)
		if len(due) > 0 && !p.routerBusyLocked(now) && !p.cutCloseMarkLocked(now) {
			sort.Slice(due, func(i, j int) bool {
				ai, aj := p.closeDueAt[strings.ToLower(due[i].s.ID)], p.closeDueAt[strings.ToLower(due[j].s.ID)]
				if !ai.Equal(aj) {
					return ai.Before(aj)
				}
				return strings.ToLower(due[i].s.ID) < strings.ToLower(due[j].s.ID)
			})
			c := due[0]
			p.closingIDs[strings.ToLower(c.s.ID)] = time.Time{}
			out.close = &closeCand{id: c.s.ID, reason: c.reason}
		}
	}

	pooledNow := map[string]bool{}
	for _, e := range p.entries {
		pooledNow[e.sessionID] = true
	}
	p.mu.Unlock()

	display := make([]ChainSession, 0, len(open))
	for _, s := range open {
		stake := "0"
		if s.Stake != nil {
			stake = s.Stake.String()
		}
		display = append(display, ChainSession{
			ModelID: s.ModelAgentID, SessionID: s.ID, StakeWei: stake,
			EndsAt: time.Unix(s.EndsAt, 0).UTC(), Usable: s.EndsAt > nowUnix+buf, Pooled: pooledNow[s.ID],
		})
		if pooledNow[s.ID] {
			out.pooled++
		}
	}
	p.chainMu.Lock()
	p.chainOpen = display
	p.chainFetched = now
	p.chainMu.Unlock()

	for _, id := range closedLate {
		log.Printf("pool: session %s closed after its close call was cut", short(id))
		p.notifyClosed(id)
	}
	for i := 0; i+2 < len(shared); i += 3 {
		p.warnShared(shared[i].(string), shared[i+1].(string), shared[i+2].(string))
	}
	if mode == scanSweep && foreignN > foreignAdoptWarn {
		p.warnShared("", "", "c")
	}
	if mode == scanSweep && len(open) > 0 {
		var newest int64
		for _, s := range open {
			newest = max(newest, s.OpenedAt)
		}
		skew := now.Sub(time.Unix(newest, 0))
		slog.Debug("pool: sweep clock check", "skew", skew.Round(time.Second))
		if time.Unix(newest, 0).After(now.Add(attemptSkew)) {
			p.rateWarn("clock_skew", "pool: box clock behind chain", "skew", (-skew).Round(time.Second))
		}
	}
	return out
}

// warnShared: a sign that another opener uses this router wallet.
func (p *Pool) warnShared(session, bid, trigger string) {
	p.rateWarn("wallet_shared_suspected",
		"wallet_shared_suspected: session not opened by this Uplink — the router wallet must be dedicated; two Uplinks on one wallet close each other's sessions",
		"session", short(session), "bid", short(bid), "trigger", trigger)
}

// localDueLocked counts close-due sessions known without the slot: those
// the last held scan saw close-due, plus retired sessions with nothing in
// flight (unheld). oldest is the longest any of them has been due.
func (p *Pool) localDueLocked(now time.Time) (int, time.Duration) {
	n, oldest := len(p.closeDueAt), p.oldestDueLocked(now)
	for id, st := range p.sess {
		if !st.retired || st.closing || st.inflight > 0 || p.closingMarkedLocked(id) {
			continue
		}
		if _, seen := p.closeDueAt[strings.ToLower(id)]; !seen {
			n++
		}
		if d := now.Sub(st.unheldSince()); d > oldest {
			oldest = d
		}
	}
	return n, oldest
}

func (p *Pool) oldestDueLocked(now time.Time) time.Duration {
	var oldest time.Duration
	for _, t := range p.closeDueAt {
		if d := now.Sub(t); d > oldest {
			oldest = d
		}
	}
	return oldest
}

type closeOutcome int

const (
	closeOK closeOutcome = iota
	closeAlready
	closeFailed
	closeCut // Uplink stopped waiting: the mark stays until it is seen closed
)

// closeWithToken closes one session while holding the slot. reason and
// source feed the rescan audit event ("" source: no event). A cut close
// keeps its closing mark (routerCloseMaxLife) so nothing adopts or leases
// it meanwhile; a definite failure clears it.
func (p *Pool) closeWithToken(ctx context.Context, tok *slotToken, id, reason, source string, per time.Duration) (closeOutcome, error) {
	if !p.slot.holds(tok) {
		if testing.Testing() {
			panic("pool: close without holding the wallet slot")
		}
		slog.Error("pool: close refused: wallet slot not held", "session", short(id))
		return closeFailed, errSlotBusy
	}
	if per <= 0 {
		per = closeCallTimeout
	}
	lid := strings.ToLower(id)
	p.mu.Lock()
	p.closingIDs[lid] = time.Time{}
	p.mu.Unlock()
	emit := func(result string) {
		if source != "" {
			p.emitRescan(ReconcileEvent{Kind: "session_reconcile_close", Reason: reason, Session: short(id), Result: result, Source: source})
		}
	}
	cctx, cancel := p.slot.callCtx(context.WithoutCancel(ctx), tok, per)
	err := p.client.CloseSessionCtx(cctx, id)
	cancel()
	done := func() {
		p.mu.Lock()
		delete(p.closingIDs, lid)
		delete(p.closeDueAt, lid)
		delete(p.knownDup, lid)
		p.closedByUs[lid] = true
		p.mu.Unlock()
		p.chainMu.Lock()
		kept := p.chainOpen[:0:0]
		for _, c := range p.chainOpen {
			if !strings.EqualFold(c.SessionID, id) {
				kept = append(kept, c)
			}
		}
		p.chainOpen = kept
		p.chainMu.Unlock()
		p.notifyClosed(id)
	}
	if err == nil {
		done()
		emit("ok")
		return closeOK, nil
	}
	if isTransportErr(err) {
		p.mu.Lock()
		p.closingIDs[lid] = p.now().Add(routerCloseMaxLife)
		p.closedByUs[lid] = true
		p.mu.Unlock()
		slog.Warn("pool: close cut; the session stays marked closing until it is seen closed", "session", short(id), "kind", cutKind(err))
		emit("failed")
		return closeCut, err
	}
	// A definite answer: maybe it is already closed.
	if p.slot.remaining(tok) > 0 {
		gctx, gcancel := p.slot.callCtx(context.WithoutCancel(ctx), tok, readCallTimeout)
		s, gerr := p.client.GetSessionCtx(gctx, id)
		gcancel()
		var se *router.StatusError
		if (gerr == nil && s.ClosedAt != 0) || (errors.As(gerr, &se) && se.Code == http.StatusNotFound) {
			done()
			emit("already_closed")
			return closeAlready, nil
		}
	}
	p.mu.Lock()
	delete(p.closingIDs, lid)
	p.mu.Unlock()
	emit("failed")
	return closeFailed, err
}

// ScanOrphans is one sweep tick: when the slot is free it scans (adopting
// and resolving attempts) and closes at most one close-due session per
// hold, releasing between closes so a queued open goes first. When opens
// keep the slot busy for sweepMaxSkips ticks (whether or not a close is
// known to be due), or a close-due session (seen by the last scan, or
// retired with nothing in flight) is orphanAlertAge old, it queues for
// one hold and closes at most one session.
func (p *Pool) ScanOrphans(ctx context.Context) {
	if p.gateClosed() {
		return
	}
	tok := p.slot.tryAcquire("")
	forced := false
	if tok == nil {
		p.mu.Lock()
		p.sweepSkips++
		skips := p.sweepSkips
		due, oldest := p.localDueLocked(p.now())
		p.mu.Unlock()
		p.alertStarved(due, oldest)
		// Skips and the close-due age are counted without the slot. After
		// sweepMaxSkips busy ticks, or once a close-due session is
		// orphanAlertAge old, queue for one hold whether or not anything is
		// known to be due: an on-chain orphan is only seen by a scan, and
		// the queued scan recomputes everything under p.mu.
		if skips < sweepMaxSkips && (due == 0 || oldest < orphanAlertAge) {
			return
		}
		t, err := p.slot.acquire(ctx, "")
		if err != nil {
			return
		}
		tok, forced = t, true
	}
	p.mu.Lock()
	p.sweepSkips = 0 // this tick holds the slot
	p.mu.Unlock()
	for {
		out, err := p.scan(ctx, tok, scanSweep)
		if err != nil {
			p.slot.release(tok)
			warnf("pool: sweep scan: %v", err)
			return
		}
		if out.close != nil {
			p.closeWithToken(ctx, tok, out.close.id, out.close.reason, "rescan", closeCallTimeout)
		} else {
			p.alertStarved(out.due, out.oldest)
		}
		p.slot.release(tok)
		if out.close == nil || out.due <= 1 || forced || ctx.Err() != nil {
			return
		}
		if tok = p.slot.tryAcquire(""); tok == nil {
			return
		}
	}
}

func (p *Pool) alertStarved(due int, oldest time.Duration) {
	if due >= orphanAlertCount || (due > 0 && oldest >= orphanAlertAge) {
		p.mu.Lock()
		skips := p.sweepSkips
		p.mu.Unlock()
		p.rateWarn("orphan_close_starved", "orphan_close_starved", "count", due, "oldestAge", oldest.Round(time.Second), "skips", skips)
	}
}

// RunSweep runs the sweep every orphanScanEvery, and soon after an open
// was cut or a session was released for closing: it closes retired
// sessions, then runs ScanOrphans. It returns when ctx ends.
func (p *Pool) RunSweep(ctx context.Context) {
	t := time.NewTicker(orphanScanEvery)
	defer t.Stop()
	var again <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.scanKick:
		case <-again:
		}
		again = nil
		p.retiredBusy.Store(false)
		p.sweepRetired(ctx, 0, false)
		p.ScanOrphans(ctx)
		if p.retiredBusy.Load() {
			// A retired session waits for the slot: try again soon rather
			// than at the next tick (the try never queues behind opens).
			again = time.After(sweepRetryEvery)
		}
	}
}

// cutKind names a cut close call for the log without its text (which
// names the router path and the full session id).
func cutKind(err error) string {
	var se *router.StatusError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("status_%d", se.Code)
	case errors.Is(err, context.Canceled):
		return "canceled"
	case router.IsUplinkTimeout(context.Background(), err):
		return "timeout"
	}
	return "transport"
}
