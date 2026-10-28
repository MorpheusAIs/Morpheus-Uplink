package pool

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
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
	// MaxBidAttempts bounds the by-bid walk when more than one provider is
	// excluded.
	MaxBidAttempts = 3
	// FailoverCloseTimeout bounds the synchronous close of the failed
	// session before the replacement opens.
	FailoverCloseTimeout = 30 * time.Second
)

// ErrNoAlternate means no other provider serves the model right now (or
// the pool could not tell), so the failed session was kept, not closed.
var ErrNoAlternate = errors.New("no alternate provider for this model")

// ErrNoEligibleProvider means only providers excluded by
// the providerExclude setting serve the model: nothing is opened (a configured
// exclusion is never overridden, unlike a cool-down).
var ErrNoEligibleProvider = errors.New("no eligible provider for this model")

// ErrNoAvailableProvider means every eligible provider of the model is
// avoided right now (cooling, or cut a stream just before): nothing is
// opened. Unlike ErrNoEligibleProvider it clears on its own; the error
// value (*NoAvailableError) carries when to retry.
var ErrNoAvailableProvider = errors.New("no provider is available for this model right now")

// NoAvailableError is the ErrNoAvailableProvider value. RetryAfter is the
// shortest remaining cool-down of an eligible provider, rounded up to whole
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

// avoidSet is every provider a new open for the model avoids: cooling,
// configured exclusions, and a pending stream cut. Only the ones that bid
// on the model change how it opens (avoidedBidder).
func (p *Pool) avoidSet(modelID string) map[string]bool {
	out := p.coolingProviders(modelID)
	for pr := range p.excludedProviders() {
		out[pr] = true
	}
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

// alternates returns the rated bids whose provider is not excluded, and the
// number of distinct such providers.
func alternates(bids []router.RatedBid, exclude map[string]bool) ([]router.RatedBid, int) {
	var out []router.RatedBid
	seen := map[string]bool{}
	for _, b := range bids {
		if exclude[b.Provider] {
			continue
		}
		out = append(out, b)
		seen[b.Provider] = true
	}
	return out, len(seen)
}

// avoidedBidder reports whether an avoided provider has a bid in bids.
// When none has, a plain open by model cannot land on one, so the router
// rates and walks the providers itself.
func avoidedBidder(bids []router.RatedBid, avoid map[string]bool) bool {
	for _, b := range bids {
		if avoid[b.Provider] {
			return true
		}
	}
	return false
}

// openByModel is the plain open by model: the router picks the provider.
func (p *Pool) openByModel(modelID string, durationSec int) (string, string, error) {
	id, err := p.client.OpenSession(modelID, router.OpenSessionRequest{SessionDuration: durationSec})
	if err != nil {
		return "", "", err
	}
	prov, _ := p.sessionProvider(modelID, id)
	return id, prov, nil
}

// openExcluding opens a session on a provider outside the avoided set by
// walking alts (the rated bids already filtered of it) by bid: at most
// MaxBidAttempts opens, one per provider. Avoidance is never left to the
// router's own provider pick. A failed open puts that provider in cool-down; a
// walk where every open failed returns an error matching
// errBidWalkExhausted.
func (p *Pool) openExcluding(modelID string, durationSec int, alts []router.RatedBid) (string, string, error) {
	tried := map[string]bool{}
	attempts := 0
	var lastErr error
	for _, b := range alts {
		if attempts >= MaxBidAttempts {
			break
		}
		if tried[b.Provider] {
			continue
		}
		tried[b.Provider] = true
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

// openAvoidingCooling is the normal open: a plain open by model when no
// provider is avoided, or when no avoided provider bids on the model (read
// from the rated bids); else a by-bid open outside the avoided set
// (cooling, configured exclusions, a pending stream cut), in the router's
// rated order. It never drops an avoidance to get a session (fail fast):
//   - only configured exclusions (the providerExclude setting) serve the model
//     → ErrNoEligibleProvider;
//   - every eligible provider is avoided, or each one the walk tried failed
//     to open, or the rated bids cannot be read → ErrNoAvailableProvider
//     (with when to retry), and nothing is opened.
//
// A refusal consumes a pending stream cut.
func (p *Pool) openAvoidingCooling(modelID string, durationSec int) (string, string, error) {
	exclude := p.avoidSet(modelID)
	if len(exclude) == 0 {
		return p.openByModel(modelID, durationSec)
	}
	configured := p.excludedProviders()
	cut := p.pendingCut(modelID)
	refuse := func(bids []router.RatedBid, bidsOK bool) (string, string, error) {
		if cut != "" {
			p.clearCut(modelID)
		}
		return "", "", &NoAvailableError{RetryAfter: p.retryAfter(modelID, bids, bidsOK, configured)}
	}
	bids, err := p.client.RatedBids(modelID)
	if err != nil {
		// An open by model could land on an avoided provider: refuse.
		slog.Warn("pool: rated bids unavailable and providers are avoided; not opening", "model", modelID, "err", err)
		return refuse(nil, false)
	}
	if !avoidedBidder(bids, exclude) {
		id, prov, err := p.openByModel(modelID, durationSec)
		if err == nil && cut != "" {
			p.clearCut(modelID)
		}
		return id, prov, err
	}
	if _, m := alternates(bids, configured); m == 0 && len(configured) > 0 {
		slog.Warn("pool: only configured-excluded providers serve this model", "model", modelID)
		if cut != "" {
			p.clearCut(modelID)
		}
		return "", "", ErrNoEligibleProvider
	}
	alts, n := alternates(bids, exclude)
	if n == 0 {
		slog.Warn("pool: every eligible provider is avoided; not opening", "model", modelID)
		return refuse(bids, true)
	}
	id, prov, err := p.openExcluding(modelID, durationSec, alts)
	if errors.Is(err, errBidWalkExhausted) {
		slog.Warn("pool: every provider tried failed to open; not opening on an avoided one", "model", modelID, "err", err)
		return refuse(bids, true)
	}
	if err != nil {
		return "", "", err
	}
	if cut != "" {
		p.clearCut(modelID)
	}
	return id, prov, nil
}

// retryAfter is the shortest remaining cool-down among the model's eligible
// providers (in the rated bids when they were read, never a configured
// exclusion), capped at MaxRetryAfter and rounded up to whole seconds
// (at least 1s). 0 when none is cooling.
func (p *Pool) retryAfter(modelID string, bids []router.RatedBid, bidsOK bool, configured map[string]bool) time.Duration {
	inBids := map[string]bool{}
	for _, b := range bids {
		inBids[strings.ToLower(strings.TrimSpace(b.Provider))] = true
	}
	excluded := map[string]bool{}
	for pr := range configured {
		excluded[strings.ToLower(strings.TrimSpace(pr))] = true
	}
	prefix := normModel(modelID) + "|"
	now := time.Now()
	var best time.Duration
	p.fmu.Lock()
	for k, until := range p.cooling {
		if !strings.HasPrefix(k, prefix) || !now.Before(until) {
			continue
		}
		pr := strings.TrimPrefix(k, prefix)
		if excluded[pr] || bidsOK && !inBids[pr] {
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

	// 3. Guardrail: count distinct providers outside the exclusions before
	// closing anything.
	exclude := p.avoidSet(modelID)
	exclude[failedProv] = true
	bids, err := p.client.RatedBids(modelID)
	if err != nil {
		slog.Warn("pool: failover: rated bids unavailable; keeping session", "model", modelID, "err", err)
		return res, fmt.Errorf("%w (bids unavailable: %v)", ErrNoAlternate, err)
	}
	alts, n := alternates(bids, exclude)
	if n == 0 {
		return res, ErrNoAlternate
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

	// 5. Open on another provider: by bid when an avoided provider bids
	// on the model, else by model. 6. Pool it with its provider, leased.
	var id, prov string
	if avoidedBidder(bids, exclude) {
		id, prov, err = p.openExcluding(modelID, durationSec, alts)
	} else {
		id, prov, err = p.openByModel(modelID, durationSec)
	}
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
