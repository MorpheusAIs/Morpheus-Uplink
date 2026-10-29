package pool

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
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
// seconds within 1s..MaxRetryAfter; 0 when none is cooling.
type NoAvailableError struct{ RetryAfter time.Duration }

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
	refuse := func(reason string, retry time.Duration, args ...any) ([]router.RatedBid, []router.RatedBid, error) {
		slog.Warn("pool: not opening", append([]any{"model", modelID, "reason", reason}, args...)...)
		return nil, nil, &NoAvailableError{RetryAfter: retry}
	}
	rows, rerr := p.client.RatedBids(modelID)
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

// openExcluding opens a session by walking alts (candidates' filtered
// list, router order) by bid: at most MaxBidAttempts opens, one per
// provider. Each open names its bid, so the pick is never left to the
// router. A failed open puts that provider in cool-down; a walk where
// every open failed returns an error matching errBidWalkExhausted.
func (p *Pool) openExcluding(modelID string, durationSec int, alts []router.RatedBid) (string, string, error) {
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
		id, err := p.client.OpenSessionByBid(b.ID, durationSec)
		if err != nil {
			lastErr = err
			warnf("pool: open bid %s (provider %s) for model %s: %v", short(b.ID), short(b.Provider), modelID, err)
			p.MarkCooling(modelID, b.Provider)
			continue
		}
		return id, b.Provider, nil
	}
	if lastErr == nil {
		return "", "", fmt.Errorf("open on another provider (none tried): %w", ErrNoAlternate)
	}
	return "", "", &walkError{tried: attempts, last: lastErr}
}

// openAvoidingCooling is the normal open: always by bid, on the first
// candidates row (healthy, not excluded, not cooling or cut, router
// order), walking on per openExcluding. It never drops a filter to get a
// session (fail fast): candidates' refusals are returned as they are, and
// a walk where every open failed → ErrNoAvailableProvider with when to
// retry. A refusal consumes a pending stream cut; a success clears it.
func (p *Pool) openAvoidingCooling(modelID string, durationSec int) (string, string, error) {
	cut := p.pendingCut(modelID)
	clear := func() {
		if cut != "" {
			p.clearCut(modelID)
		}
	}
	cands, pool, err := p.candidates(modelID, "")
	if err != nil {
		clear()
		return "", "", err
	}
	id, prov, err := p.openExcluding(modelID, durationSec, cands)
	if errors.Is(err, errBidWalkExhausted) {
		slog.Warn("pool: not opening", "model", modelID, "reason", "walk_exhausted", "tried", min(len(cands), MaxBidAttempts))
		clear()
		return "", "", &NoAvailableError{RetryAfter: p.retryAfter(modelID, pool)}
	}
	if err != nil {
		return "", "", err
	}
	clear()
	return id, prov, nil
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
	return clampRetryAfter(best)
}

// clampRetryAfter caps d at MaxRetryAfter before rounding up to seconds;
// any positive d is at least 1s, 0 stays 0 (no header).
func clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
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
		id, prov, err := p.openAvoidingCooling(modelID, durationSec)
		p.finishFailoverOpen(key, ao, id, prov, durationSec, err, &res)
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
	cands, _, err := p.candidates(modelID, failedProv)
	if err != nil || len(cands) == 0 {
		slog.Warn("pool: failover: no healthy alternate; keeping session", "model", modelID, "session", short(failedID), "err", err)
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
			slog.Warn("pool: failover: close failed (retired; not re-adopted)", "model", modelID, "session", short(failedID), "err", err)
		} else {
			p.notifyClosed(failedID)
		}
	} else {
		res.CloseDeferred = true
	}
	if !o.Reopen {
		// A mid-stream cut: the next open walks the bids without it.
		p.MarkStreamCut(modelID, failedProv)
		return res, nil
	}

	// 5. Open on another provider, by bid from the filtered list.
	// 6. Pool it with its provider, leased.
	id, prov, err := p.openExcluding(modelID, durationSec, cands)
	p.finishFailoverOpen(key, ao, id, prov, durationSec, err, &res)
	if err != nil {
		return res, err
	}
	log.Printf("pool: failed over model %s from provider %s to session %s (provider %s)", modelID, short(failedProv), short(id), short(prov))
	return res, nil
}

// finishFailoverOpen, when the open worked, pools the replacement and
// leases it for the caller in one critical section.
func (p *Pool) finishFailoverOpen(key string, ao AcquireOpts, id, prov string, durationSec int, err error, res *FailoverResult) {
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.entries[key] = entry{sessionID: id, expiresAt: now.Add(time.Duration(durationSec)*time.Second - expiryBuffer), provider: prov}
	st := p.newStateLocked(key, id, now, now.Add(time.Duration(durationSec)*time.Second), ao.KeyID)
	res.Lease = p.leaseLocked(id, st, ao.KeyID)
	res.SessionID, res.Provider, res.Opened = id, prov, true
}

// closeBounded closes a session, giving up waiting after
// FailoverCloseTimeout (the close may still land later).
func (p *Pool) closeBounded(sessionID string) error {
	done := make(chan error, 1)
	go func() { done <- p.client.CloseSession(sessionID) }()
	select {
	case err := <-done:
		return err
	case <-time.After(FailoverCloseTimeout):
		return fmt.Errorf("close timed out after %s", FailoverCloseTimeout)
	}
}
