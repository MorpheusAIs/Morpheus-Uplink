package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Provider failover. Uplink owns it (the router's in-prompt failover is
// always off), so the pool can enforce three rules the router cannot:
//
//   - never close a session unless a different provider is there to go to
//     (stakes lock while open, so a close and reopen onto the same or no
//     provider only ties up liquidity);
//   - one failover per failed session, even when many requests fail on it
//     at once (compare-and-swap under the model's open lock);
//   - remember a failed provider for a while (cool-down) so new opens and
//     Rehydrate do not land on it again.

const (
	// DefaultProviderCooldown is how long a provider that failed a prompt
	// or an open is avoided for that model.
	DefaultProviderCooldown = 600 * time.Second
	// MaxBidAttempts bounds the by-bid walk: at most this many opens, one
	// per provider.
	MaxBidAttempts = 3
	// FailoverCloseTimeout bounds the synchronous close of the failed
	// session before the replacement opens.
	FailoverCloseTimeout = 30 * time.Second
)

// ErrNoAlternate means no other provider serves the model right now (or
// the pool could not tell), so the failed session was kept, not closed.
var ErrNoAlternate = errors.New("no alternate provider for this model")

// ErrNoEligibleProvider means every provider the router rates for the
// model is excluded by the providerExclude setting: nothing is opened (a
// configured exclusion is never overridden, unlike a cool-down).
var ErrNoEligibleProvider = errors.New("no eligible provider for this model")

// ErrNoAvailableProvider means no provider of the model can be opened
// right now: the router or the health feeds cannot be read, no rated bid
// is marked healthy (or every healthy one is excluded), or every healthy
// eligible provider is avoided (cooling, or cut a stream just before) or
// failed to open: nothing is opened. Unlike ErrNoEligibleProvider it clears on its own; the error
// value (*NoAvailableError) carries when to retry.
var ErrNoAvailableProvider = errors.New("no provider is available for this model right now")

// NoAvailableError is the ErrNoAvailableProvider value. RetryAfter is the
// shortest remaining cool-down of a healthy, not excluded provider, rounded up to whole
// seconds within 1s..MaxRetryAfter; 0 when none is cooling. Reason is
// set for the wallet-path refusals: router_transient (RetryAfter 1–4 s)
// and open_busy (no RetryAfter).
type NoAvailableError struct {
	RetryAfter time.Duration
	Reason     string
}

func (e *NoAvailableError) Error() string        { return ErrNoAvailableProvider.Error() }
func (e *NoAvailableError) Is(target error) bool { return target == ErrNoAvailableProvider }

// MaxRetryAfter caps the Retry-After of ErrNoAvailableProvider.
const MaxRetryAfter = 600 * time.Second

// errBidWalkExhausted means a by-bid walk tried at least one provider and
// every open failed (each tried provider is now cooling).
var errBidWalkExhausted = errors.New("every provider tried failed to open")

// walkError wraps the last open error of an exhausted walk.
type walkError struct {
	tried int
	last  error
}

func (e *walkError) Error() string {
	return fmt.Sprintf("open on another provider (%d tried): %v", e.tried, e.last)
}
func (e *walkError) Unwrap() []error { return []error{errBidWalkExhausted, e.last} }

// SetProviderCooldown sets the per model+provider cool-down. 0 turns off
// memory across requests; an immediate failover still walks the bids
// without the provider that just failed.
func (p *Pool) SetProviderCooldown(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.fmu.Lock()
	p.cooldown = d
	p.fmu.Unlock()
}

func coolKey(modelID, provider string) string {
	return normModel(modelID) + "|" + strings.ToLower(strings.TrimSpace(provider))
}

// MarkCooling starts the cool-down for a provider on a model. No-op when
// the cool-down is 0 or the provider is unknown.
func (p *Pool) MarkCooling(modelID, provider string) {
	if strings.TrimSpace(provider) == "" {
		return
	}
	now := time.Now()
	p.fmu.Lock()
	defer p.fmu.Unlock()
	for k, until := range p.cooling { // prune on write
		if !now.Before(until) {
			delete(p.cooling, k)
		}
	}
	if p.cooldown <= 0 {
		return
	}
	p.cooling[coolKey(modelID, provider)] = now.Add(p.cooldown)
}

// IsCooling reports whether the provider is in its cool-down for the model.
func (p *Pool) IsCooling(modelID, provider string) bool {
	if strings.TrimSpace(provider) == "" {
		return false
	}
	p.fmu.Lock()
	defer p.fmu.Unlock()
	until, ok := p.cooling[coolKey(modelID, provider)]
	return ok && time.Now().Before(until)
}

// SetProviderExclude installs the configured exclusions (the
// providerExclude setting): provider addresses never opened for any model.
func (p *Pool) SetProviderExclude(provs []string) {
	ex := map[string]bool{}
	for _, pr := range provs {
		if pr = strings.ToLower(strings.TrimSpace(pr)); pr != "" {
			ex[pr] = true
		}
	}
	p.fmu.Lock()
	p.exclude = ex
	p.fmu.Unlock()
}

// IsProviderExcluded reports whether the provider is in the configured
// exclusions (for every model).
func (p *Pool) IsProviderExcluded(provider string) bool {
	pr := strings.ToLower(strings.TrimSpace(provider))
	if pr == "" {
		return false
	}
	p.fmu.Lock()
	defer p.fmu.Unlock()
	return p.exclude[pr]
}

// excludedProviders is the configured exclusion set (a copy); the same
// for every model.
func (p *Pool) excludedProviders() map[string]bool {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	out := make(map[string]bool, len(p.exclude))
	for pr := range p.exclude {
		out[pr] = true
	}
	return out
}

// errNoHealthSource is the health error when no healthy-bid source is set.
var errNoHealthSource = errors.New("no bid health source")

// SetHealthyBids installs the healthy-bid source: for a model, the
// lowercase bid ids a gateway health feed marks healthy (an error when no
// health copy is known). Every open, failover reopen included, uses only
// rated bids in that set. nil: every open is refused.
func (p *Pool) SetHealthyBids(fn func(modelID string) (map[string]struct{}, error)) {
	p.fmu.Lock()
	p.healthy = fn
	p.fmu.Unlock()
}

func (p *Pool) healthyBids(modelID string) (map[string]struct{}, error) {
	p.fmu.Lock()
	fn := p.healthy
	p.fmu.Unlock()
	if fn == nil {
		return nil, errNoHealthSource
	}
	return fn(modelID)
}

// MarkStreamCut records that provider cut a stream for the model: the
// model's next open walks the bids without it, even when the cool-down is
// off or this is the only provider cooling.
func (p *Pool) MarkStreamCut(modelID, provider string) {
	if strings.TrimSpace(provider) == "" {
		return
	}
	p.fmu.Lock()
	p.cutNext[normModel(modelID)] = strings.ToLower(strings.TrimSpace(provider))
	p.fmu.Unlock()
}

func (p *Pool) pendingCut(modelID string) string {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	return p.cutNext[normModel(modelID)]
}

func (p *Pool) clearCut(modelID string) {
	p.fmu.Lock()
	delete(p.cutNext, normModel(modelID))
	p.fmu.Unlock()
}

// avoidedProviders is every provider a new open for the model skips on
// top of the configured exclusions: cooling, and a pending stream cut.
func (p *Pool) avoidedProviders(modelID string) map[string]bool {
	out := p.coolingProviders(modelID)
	if c := p.pendingCut(modelID); c != "" {
		out[c] = true
	}
	return out
}

// coolingProviders lists the model's providers in cool-down.
func (p *Pool) coolingProviders(modelID string) map[string]bool {
	prefix := normModel(modelID) + "|"
	now := time.Now()
	out := map[string]bool{}
	p.fmu.Lock()
	defer p.fmu.Unlock()
	for k, until := range p.cooling {
		if strings.HasPrefix(k, prefix) && now.Before(until) {
			out[strings.TrimPrefix(k, prefix)] = true
		}
	}
	return out
}

// retire records a failed-over session so Rehydrate never adopts it back,
// even when its close failed. Entries drop once the session has ended.
func (p *Pool) retire(sessionID string, endsAt time.Time) {
	now := time.Now()
	if !endsAt.After(now) {
		endsAt = now.Add(time.Duration(p.DurationSec())*time.Second + expiryBuffer)
	}
	p.fmu.Lock()
	defer p.fmu.Unlock()
	for id, end := range p.retired {
		if !now.Before(end) {
			delete(p.retired, id)
		}
	}
	p.retired[sessionID] = endsAt
}

func (p *Pool) isRetired(sessionID string) bool {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	_, ok := p.retired[sessionID]
	return ok
}

// sessionProvider is the provider of a session: from the pool entry when
// known, else from the router (best effort, "" when unknown).
func (p *Pool) sessionProvider(modelID, sessionID string) (string, time.Time) {
	p.mu.Lock()
	e, ok := p.entries[normModel(modelID)]
	p.mu.Unlock()
	if ok && e.sessionID == sessionID && e.provider != "" {
		return e.provider, e.expiresAt.Add(expiryBuffer)
	}
	s, err := p.client.GetSession(sessionID)
	if err != nil {
		warnf("pool: provider of session %s: %v", short(sessionID), err)
		return "", time.Time{}
	}
	end := time.Time{}
	if s.EndsAt > 0 {
		end = time.Unix(s.EndsAt, 0)
	}
	return s.Provider, end
}

// MarkSessionCooling starts the cool-down for the provider behind a session.
func (p *Pool) MarkSessionCooling(modelID, sessionID string) string {
	prov, _ := p.sessionProvider(modelID, sessionID)
	p.MarkCooling(modelID, prov)
	return prov
}

// candidates is the one pipeline every open uses: the router's rated bids
// for the model (router Score order), kept only when a health feed marks
// the bid healthy, minus configured exclusions, minus cooling, pending-cut
// and extraAvoid providers. Filters only remove rows; the order is the
// router's. pool is the healthy, not excluded rows (for Retry-After).
//
// Errors: ErrNoEligibleProvider when every rated provider is excluded;
// else ErrNoAvailableProvider (a *NoAvailableError) when the router or
// the health source cannot be read, no rated bid is healthy, every
// healthy one is excluded, or every remaining one is avoided. Each refusal
// logs its reason with counts and short ids only.
func (p *Pool) candidates(modelID, extraAvoid string) (cands, pool []router.RatedBid, err error) {
	rows, rerr := p.client.RatedBids(modelID)
	return p.candidatesFrom(modelID, extraAvoid, rows, rerr)
}

// candidatesFrom filters one rated-bids answer (see candidates).
func (p *Pool) candidatesFrom(modelID, extraAvoid string, rows []router.RatedBid, rerr error) (cands, pool []router.RatedBid, err error) {
	refuse := func(reason string, retry time.Duration, args ...any) ([]router.RatedBid, []router.RatedBid, error) {
		slog.Warn("pool: not opening", append([]any{"model", modelID, "reason", reason}, args...)...)
		return nil, nil, &NoAvailableError{RetryAfter: retry}
	}
	if rerr != nil {
		var ue *url.Error
		switch {
		case errors.As(rerr, &ue):
			return refuse("router_unreachable", 0)
		case errors.Is(rerr, router.ErrRatedBidsIncomplete) && len(rows) > 0:
			slog.Warn("pool: rated bids have unusable rows; using the valid ones", "model", modelID, "valid", len(rows))
		default:
			return refuse("rated_unreadable", 0)
		}
	}
	if len(rows) == 0 {
		return refuse("rated_unreadable", 0, "rated", 0)
	}
	excluded := p.excludedProviders()
	allExcluded := true
	for _, b := range rows {
		if !excluded[normProv(b.Provider)] {
			allExcluded = false
			break
		}
	}
	if allExcluded {
		slog.Warn("pool: not opening", "model", modelID, "reason", "all_excluded", "rated", len(rows))
		return nil, nil, ErrNoEligibleProvider
	}
	h, herr := p.healthyBids(modelID)
	if herr != nil {
		return refuse("health_unknown", 0, "rated", len(rows))
	}
	var healthy []router.RatedBid
	for _, b := range rows {
		if _, ok := h[strings.ToLower(strings.TrimSpace(b.ID))]; ok {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return refuse("none_healthy", 0, "rated", len(rows))
	}
	for _, b := range healthy {
		if !excluded[normProv(b.Provider)] {
			pool = append(pool, b)
		}
	}
	if len(pool) == 0 {
		return refuse("healthy_all_excluded", 0, "rated", len(rows), "healthy", len(healthy))
	}
	avoid := p.avoidedProviders(modelID)
	if x := normProv(extraAvoid); x != "" {
		avoid[x] = true
	}
	for _, b := range pool {
		if !avoid[normProv(b.Provider)] {
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 {
		return refuse("all_cooling", p.retryAfter(modelID, pool), "rated", len(rows), "healthy", len(healthy), "eligible", len(pool))
	}
	return cands, pool, nil
}

func normProv(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// openOp is one open operation (an Acquire open, or a failover reopen)
// holding the wallet slot. Its transient retries share one budget.
type openOp struct {
	p             *Pool
	tok           *slotToken
	model         string
	start         time.Time // pool clock
	retries       int
	windowRetries int
}

func (p *Pool) newOpenOp(tok *slotToken, modelID string) *openOp {
	return &openOp{p: p, tok: tok, model: modelID, start: p.now()}
}

// openResult is a session an open produced: opened (the router returned
// its id) or adopted (found on chain after an open whose answer was lost).
type openResult struct {
	id, provider     string
	adopted          bool
	openedAt, endsAt time.Time
	stake            *big.Int
}

// backoff waits before the next transient retry. It reports false (and
// the step it would have waited) when the operator did not set the
// router's RPC (one attempt: the router's own RPC list and retries
// apply), when the retry count, the operation budget or the slot
// deadline does not allow another, or ctx ended.
func (op *openOp) backoff(ctx context.Context) (time.Duration, bool) {
	d := backoffStep(op.retries)
	if !op.p.providedRPC.Load() {
		return d, false
	}
	now := op.p.now()
	if op.retries >= transientMaxRetries || now.Sub(op.start)+d > transientBudget ||
		now.Add(d).After(op.tok.deadline) || ctx.Err() != nil {
		return d, false
	}
	if err := op.p.sleep(ctx, jittered(d)); err != nil {
		return d, false
	}
	op.retries++
	return d, true
}

// candidates reads the rated bids inside the slot, backing off on a
// rate-limited read, then filters them (Pool.candidatesFrom).
func (op *openOp) candidates(ctx context.Context, extraAvoid string) (cands, pool []router.RatedBid, err error) {
	p := op.p
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rctx, cancel := p.slot.callCtx(ctx, op.tok, readCallTimeout)
		rows, rerr := p.client.RatedBidsCtx(rctx, op.model)
		cancel()
		if rerr != nil && ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if rerr != nil && isTransientRead(rerr) {
			if d, ok := op.backoff(ctx); !ok {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				slog.Warn("pool: not opening", "model", op.model, "reason", ReasonRouterTransient, "stage", "rated", "retries", op.retries)
				return nil, nil, errTransient(d)
			}
			continue
		}
		return p.candidatesFrom(op.model, extraAvoid, rows, rerr)
	}
}

// openExcluding opens a session by walking alts (candidates' filtered
// list, router order) by bid: at most MaxBidAttempts opens, one per
// provider. Each open names its bid, so the pick is never left to the
// router. Each error is classified (transient.go):
//   - provider fault: that provider cools and the walk moves on;
//   - a chain-stage error before the send: the same bid again after a
//     backoff when the operator set the router's RPC; otherwise one
//     attempt, then router_transient (never cooled, never walked);
//   - maybe sent: never re-opened; the wallet's sessions are polled for
//     it (reconcile). Found: it is the result. Not found: router_transient,
//     and the attempt stays pending for later scans. A cut open (Uplink
//     stopped waiting, transport error, 502/503/504) also starts the
//     router-busy window.
//
// A walk where every open was a provider fault returns an error matching
// errBidWalkExhausted.
func (op *openOp) openExcluding(ctx context.Context, durationSec int, alts []router.RatedBid) (openResult, error) {
	p := op.p
	tried := map[string]bool{}
	attempts := 0
	var lastErr error
	for _, b := range alts {
		if attempts >= MaxBidAttempts {
			break
		}
		if tried[normProv(b.Provider)] {
			continue
		}
		tried[normProv(b.Provider)] = true
		attempts++
	sameBid:
		for {
			if err := ctx.Err(); err != nil {
				return openResult{}, err
			}
			if p.routerBusy() {
				slog.Warn("pool: not opening", "model", op.model, "reason", "router_busy")
				return openResult{}, errOpenBusyRefusal()
			}
			if left := p.slot.remaining(op.tok); left < openMinPostTime+reconcileReserve {
				slog.Warn("pool: not opening", "model", op.model, "reason", ReasonRouterTransient, "stage", "slot_deadline", "left", left.Round(time.Second))
				return openResult{}, errTransient(transientCap)
			}
			att := p.addAttempt(op.model, b.ID)
			postStart := p.now()
			// Once sent, the open is never cancelled by the caller: its
			// result is reconciled whatever happens to the request.
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), realDur(p.slot.remaining(op.tok)-reconcileReserve))
			id, err := p.client.OpenSessionByBidCtx(pctx, b.ID, durationSec)
			cancel()
			elapsed := p.now().Sub(postStart)
			if err == nil {
				p.dropAttempt(att)
				return openResult{id: id, provider: b.Provider}, nil
			}
			windowOK := p.providedRPC.Load() && op.windowRetries < maxWindowRetries
			class := classifyOpenErr(err, elapsed, windowOK)
			slog.Warn("pool: open failed", "model", op.model, "bid", short(b.ID), "provider", short(b.Provider),
				"class", class.String(), "after", elapsed.Round(time.Millisecond), "err", router.SafeError(err))
			switch class {
			case providerFault:
				p.dropAttempt(att)
				lastErr = err
				p.MarkCooling(op.model, b.Provider)
				break sameBid
			case transientPreSend, transientWindow:
				if class == transientWindow {
					op.windowRetries++ // the attempt stays pending: it may still land
				} else {
					p.dropAttempt(att)
				}
				d, ok := op.backoff(ctx)
				if !ok {
					if ctx.Err() != nil {
						return openResult{}, ctx.Err()
					}
					slog.Warn("pool: not opening", "model", op.model, "reason", ReasonRouterTransient, "stage", "open", "retries", op.retries)
					return openResult{}, errTransient(d)
				}
			default: // maybe sent
				if isTransportErr(err) {
					p.setRouterBusy(att)
				}
				return op.reconcile(context.WithoutCancel(ctx), att)
			}
		}
	}
	if lastErr == nil {
		return openResult{}, fmt.Errorf("open on another provider (none tried): %w", ErrNoAlternate)
	}
	return openResult{}, &walkError{tried: attempts, last: lastErr}
}

// reconcile polls the wallet's open sessions (reconcilePolls times,
// reconcileGap apart, inside the slot hold) for the session att may have
// opened. A match is the result (extra matches are known duplicates the
// sweep closes). No match: router_transient; att stays pending, so a
// later scan adopts the session if it lands.
func (op *openOp) reconcile(ctx context.Context, att *attempt) (openResult, error) {
	p := op.p
	for i := 0; i < reconcilePolls; i++ {
		if i > 0 {
			if p.slot.remaining(op.tok) < reconcileGap+readCallTimeout/2 {
				break
			}
			if err := p.sleep(ctx, reconcileGap); err != nil {
				break
			}
		}
		if p.slot.remaining(op.tok) <= 0 {
			break
		}
		mk := func() (context.Context, context.CancelFunc) { return p.slot.callCtx(ctx, op.tok, readCallTimeout) }
		open, _, err := p.readOpen(mk)
		if err != nil {
			continue
		}
		if got, ok := p.matchOpen(att, open); ok {
			return got, nil
		}
	}
	p.requestSweep()
	slog.Warn("pool: not opening", "model", op.model, "reason", ReasonRouterTransient, "stage", "reconcile", "bid", short(att.bid))
	return openResult{}, errTransient(transientCap)
}

// matchOpen picks the session att opened from a list, if it is there.
func (p *Pool) matchOpen(att *attempt, open []router.Session) (openResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ms []router.Session
	now := p.now()
	for _, s := range open {
		if !att.matches(s) || p.closingMarkedLocked(s.ID) || p.sess[s.ID] != nil {
			continue
		}
		if s.EndsAt <= now.Unix()+int64(expiryBuffer.Seconds()) {
			continue
		}
		ms = append(ms, s)
	}
	if len(ms) == 0 {
		return openResult{}, false
	}
	keep := pickOne(ms)
	for _, s := range ms {
		if s.ID != keep.ID {
			p.knownDup[strings.ToLower(s.ID)] = true
		}
	}
	p.resolveAttemptLocked(att)
	return openResult{id: keep.ID, provider: keep.Provider, adopted: true,
		openedAt: time.Unix(keep.OpenedAt, 0), endsAt: time.Unix(keep.EndsAt, 0), stake: keep.Stake}, true
}

// openAvoidingCooling is the normal open: always by bid, on the first
// candidates row (healthy, not excluded, not cooling or cut, router
// order), walking on per openExcluding. It never drops a filter to get a
// session (fail fast): candidates' refusals are returned as they are, and
// a walk where every open failed → ErrNoAvailableProvider with when to
// retry. A refusal consumes a pending stream cut; a success clears it.
func (op *openOp) openAvoidingCooling(ctx context.Context, durationSec int) (openResult, error) {
	p := op.p
	modelID := op.model
	cut := p.pendingCut(modelID)
	clear := func() {
		if cut != "" {
			p.clearCut(modelID)
		}
	}
	cands, pool, err := op.candidates(ctx, "")
	if err != nil {
		clear()
		return openResult{}, err
	}
	got, err := op.openExcluding(ctx, durationSec, cands)
	if errors.Is(err, errBidWalkExhausted) {
		slog.Warn("pool: not opening", "model", modelID, "reason", "walk_exhausted", "tried", min(len(cands), MaxBidAttempts))
		clear()
		return openResult{}, &NoAvailableError{RetryAfter: p.retryAfter(modelID, pool)}
	}
	if err != nil {
		return openResult{}, err
	}
	clear()
	return got, nil
}

// openAvoidingCooling runs one open operation under the wallet slot
// (tests and callers without a slot of their own).
func (p *Pool) openAvoidingCooling(modelID string, durationSec int) (string, string, error) {
	ctx := context.Background()
	tok, err := p.slot.acquire(ctx, "")
	if err != nil {
		return "", "", errOpenBusyRefusal()
	}
	defer p.slot.release(tok)
	got, err := p.newOpenOp(tok, modelID).openAvoidingCooling(ctx, durationSec)
	return got.id, got.provider, err
}

// retryAfter is the shortest remaining cool-down among pool's providers
// (the model's healthy, not excluded rated bids), capped at MaxRetryAfter
// and rounded up to whole seconds (at least 1s). 0 when none is cooling.
func (p *Pool) retryAfter(modelID string, pool []router.RatedBid) time.Duration {
	inPool := map[string]bool{}
	for _, b := range pool {
		inPool[normProv(b.Provider)] = true
	}
	prefix := normModel(modelID) + "|"
	now := time.Now()
	var best time.Duration
	p.fmu.Lock()
	for k, until := range p.cooling {
		if !strings.HasPrefix(k, prefix) || !now.Before(until) {
			continue
		}
		if !inPool[strings.TrimPrefix(k, prefix)] {
			continue
		}
		if d := until.Sub(now); best == 0 || d < best {
			best = d
		}
	}
	p.fmu.Unlock()
	if best == 0 {
		return 0 // no cool-down known: no Retry-After
	}
	return clampRetryAfter(best)
}

// clampRetryAfter caps d at MaxRetryAfter before rounding up to seconds;
// it is never below 1s (0 or less gives 1s).
func clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	if d > MaxRetryAfter {
		d = MaxRetryAfter
	}
	sec := (d + time.Second - 1) / time.Second
	if sec < 1 {
		sec = 1
	}
	return sec * time.Second
}

// FailoverResult reports what Failover did.
type FailoverResult struct {
	SessionID      string // session to retry on ("" when reopen was false)
	Opened         bool   // a new on-chain session was opened
	Replaced       bool   // another request had already failed over
	FailedProvider string
	Provider       string // provider of SessionID when known
	CloseFailed    bool   // the failed session's close did not land (it stays retired)
	CloseDeferred  bool   // still in flight on another request: closes when that ends
	DurationSec    int
	// Lease is the caller's hold on SessionID, taken in the same critical
	// section that pooled it (nil when reopen was false, or the
	// replacement is already retiring).
	Lease *Lease
}

// FailoverOpts describe the request that hit the failure.
type FailoverOpts struct {
	KeyID       string
	DurationSec int  // 0: pool default; capped at the duration max
	Reopen      bool // open a replacement and lease it (false: mid-stream cut)
}

// Failover moves a model off the provider behind failedID. It runs under
// the model's open lock, so concurrent failures on one session do one
// close and one open. With reopen=false (a mid-stream failure) it only
// retires and closes the session; the next request opens through
// EnsureDuration, which avoids cooling providers.
//
// It returns ErrNoAlternate, without closing anything, when no other
// provider serves the model.
func (p *Pool) Failover(modelID, failedID string, o FailoverOpts) (FailoverResult, error) {
	return p.FailoverCtx(context.Background(), modelID, failedID, o)
}

// FailoverCtx is Failover bound to the request context, like AcquireCtx:
// ctx bounds the slot waits, the candidate read and every open step up to
// the open POST; once a POST is sent the open runs to its own deadline and
// is reconciled whatever happens to ctx. The failed session's close is
// cleanup and does not follow ctx (closeBounded).
func (p *Pool) FailoverCtx(ctx context.Context, modelID, failedID string, o FailoverOpts) (FailoverResult, error) {
	durationSec := o.DurationSec
	if durationSec <= 0 {
		durationSec = p.DurationSec()
	}
	if mx := p.DurationMax(); mx > 0 && durationSec > mx {
		durationSec = mx
	}
	res := FailoverResult{DurationSec: durationSec}
	key := normModel(modelID)
	lock := p.openLock(key)
	lock.Lock()
	defer lock.Unlock()
	p.waitGate()

	// 1. Someone else already swapped the session: use the replacement,
	// leased in the same critical section that found it.
	p.mu.Lock()
	cur, pooled := p.entries[key]
	if pooled && cur.sessionID != failedID && p.now().Before(cur.expiresAt) {
		res.SessionID, res.Replaced, res.Provider = cur.sessionID, true, cur.provider
		if o.Reopen {
			if st := p.stateLocked(key, cur); !st.retired && !st.closing {
				res.Lease = p.leaseLocked(cur.sessionID, st, o.KeyID)
			}
		}
		p.mu.Unlock()
		return res, nil
	}
	p.mu.Unlock()
	ao := AcquireOpts{KeyID: o.KeyID}
	if p.isRetired(failedID) {
		// Already failed over, but its replacement is gone (open failed or
		// expired): open normally, avoiding cooling providers.
		res.Replaced = true
		if !o.Reopen {
			return res, nil
		}
		got, err := p.failoverOpen(ctx, o.KeyID, modelID, func(op *openOp) (openResult, error) {
			return op.openAvoidingCooling(ctx, durationSec)
		})
		p.finishFailoverOpen(key, ao, got, durationSec, err, &res)
		return res, err
	}

	// 2. Cool the failed provider.
	failedProv, endsAt := p.sessionProvider(modelID, failedID)
	res.FailedProvider = failedProv
	if failedProv == "" {
		// Cannot tell which provider failed, so cannot pick another.
		return res, ErrNoAlternate
	}
	p.MarkCooling(modelID, failedProv)

	// 3. Guardrail: the same filtered list as a normal open, without the
	// failed provider, before closing anything.
	var cands []router.RatedBid
	_, err := p.failoverOpen(ctx, o.KeyID, modelID, func(op *openOp) (openResult, error) {
		var cerr error
		cands, _, cerr = op.candidates(ctx, failedProv)
		return openResult{}, cerr
	})
	if err != nil || len(cands) == 0 {
		slog.Warn("pool: failover: no healthy alternate; keeping session", "model", modelID, "session", short(failedID), "err", router.SafeError(err))
		if err == nil {
			return res, ErrNoAlternate
		}
		return res, fmt.Errorf("%w (%v)", ErrNoAlternate, err)
	}
	// 4. Retire and close the failed session synchronously (bounded), so
	// our own close and open never race on the wallet nonce.
	// A session another request is still streaming on is retired, not
	// closed: it closes when its last lease ends [C9].
	p.retire(failedID, endsAt)
	if p.retireForFailover(key, failedID) {
		if err := p.closeBounded(failedID); err != nil {
			res.CloseFailed = true
			p.failoverCloseFailed(failedID)
			slog.Warn("pool: failover: close failed (retired; not re-adopted)", "model", modelID, "session", short(failedID), "err", router.SafeError(err))
		}
	} else {
		res.CloseDeferred = true
	}
	if !o.Reopen {
		// A mid-stream cut: the next open walks the bids without it.
		p.MarkStreamCut(modelID, failedProv)
		return res, nil
	}

	// 5. Open on another provider, by bid from the filtered list (its own
	// slot hold).
	// 6. Pool it with its provider, leased.
	if p.routerBusy() {
		slog.Warn("pool: not opening", "model", modelID, "reason", "router_busy")
		return res, errOpenBusyRefusal()
	}
	got, err := p.failoverOpen(ctx, o.KeyID, modelID, func(op *openOp) (openResult, error) {
		return op.openExcluding(ctx, durationSec, cands)
	})
	p.finishFailoverOpen(key, ao, got, durationSec, err, &res)
	if err != nil {
		return res, err
	}
	log.Printf("pool: failed over model %s from provider %s to session %s (provider %s)", modelID, short(failedProv), short(got.id), short(got.provider))
	return res, nil
}

// failoverOpen runs one failover step holding the wallet slot.
// ctx bounds the slot wait (and is passed to f by the caller).
func (p *Pool) failoverOpen(ctx context.Context, keyID, modelID string, f func(op *openOp) (openResult, error)) (openResult, error) {
	tok, err := p.slot.acquire(ctx, keyID)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return openResult{}, cerr
		}
		slog.Warn("pool: not opening", "model", modelID, "reason", ReasonOpenBusy)
		return openResult{}, errOpenBusyRefusal()
	}
	defer p.slot.release(tok)
	return f(p.newOpenOp(tok, modelID))
}

// finishFailoverOpen, when the open worked, pools the replacement and
// leases it for the caller in one critical section.
func (p *Pool) finishFailoverOpen(key string, ao AcquireOpts, got openResult, durationSec int, err error, res *FailoverResult) {
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.poolOpenedLocked(key, got, durationSec, ao.KeyID)
	res.Lease = p.leaseLocked(got.id, st, ao.KeyID)
	res.SessionID, res.Provider, res.Opened = got.id, got.provider, true
}

// closeBounded closes a session holding the wallet slot (queued), giving
// up waiting after FailoverCloseTimeout. A cut close keeps the session
// marked closing until it is seen closed; it does not hold up opens.
func (p *Pool) closeBounded(sessionID string) error {
	// Not the request context: the failed session's close is cleanup that
	// must run even if the client has gone; FailoverCloseTimeout bounds it.
	ctx := context.Background()
	tok, err := p.slot.acquire(ctx, "")
	if err != nil {
		return err
	}
	defer p.slot.release(tok)
	out, err := p.closeWithToken(ctx, tok, sessionID, "", "", FailoverCloseTimeout)
	if out == closeOK || out == closeAlready {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("close failed")
	}
	return err
}
