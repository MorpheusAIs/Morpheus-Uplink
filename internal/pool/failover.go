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
// UPLINK_PROVIDER_EXCLUDE serve the model: nothing is opened (a configured
// exclusion is never overridden, unlike a cool-down).
var ErrNoEligibleProvider = errors.New("no eligible provider for this model")

// SetProviderCooldown sets the per model+provider cool-down. 0 turns off
// memory across requests; an immediate failover still omits the provider
// that just failed.
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

// SetProviderExclude installs the configured exclusions
// (UPLINK_PROVIDER_EXCLUDE): model id or name -> providers. namer resolves
// a model id to its catalog name so name-keyed entries apply.
func (p *Pool) SetProviderExclude(m map[string][]string, namer func(string) string) {
	ex := map[string]map[string]bool{}
	for model, provs := range m {
		k := strings.ToLower(strings.TrimSpace(model))
		if ex[k] == nil {
			ex[k] = map[string]bool{}
		}
		for _, pr := range provs {
			ex[k][strings.ToLower(strings.TrimSpace(pr))] = true
		}
	}
	p.fmu.Lock()
	p.exclude, p.namer = ex, namer
	p.fmu.Unlock()
}

// excludedProviders is the configured exclusion set for a model.
func (p *Pool) excludedProviders(modelID string) map[string]bool {
	p.fmu.Lock()
	ex, namer := p.exclude, p.namer
	p.fmu.Unlock()
	out := map[string]bool{}
	if len(ex) == 0 {
		return out
	}
	keys := []string{normModel(modelID)}
	if namer != nil {
		if n := strings.ToLower(strings.TrimSpace(namer(modelID))); n != "" {
			keys = append(keys, n)
		}
	}
	for _, k := range keys {
		for pr := range ex[k] {
			out[pr] = true
		}
	}
	return out
}

func (p *Pool) isExcluded(modelID, provider string) bool {
	return provider != "" && p.excludedProviders(modelID)[strings.ToLower(strings.TrimSpace(provider))]
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
// configured exclusions, and a pending stream cut.
func (p *Pool) avoidSet(modelID string) map[string]bool {
	out := p.coolingProviders(modelID)
	for pr := range p.excludedProviders(modelID) {
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

// openExcluding opens a session that avoids the excluded providers: one
// exclusion uses the router's omitProvider on the model open; more than
// one walks the filtered rated bids by bid (at most MaxBidAttempts opens,
// one per provider). A failed open puts that provider in cool-down.
//
// byBid forces the bid walk even for one exclusion (the open after a
// stream cut, [F3]).
func (p *Pool) openExcluding(modelID string, durationSec int, exclude map[string]bool, alts []router.RatedBid, byBid bool) (string, string, error) {
	if len(exclude) == 1 && !byBid {
		var omit string
		for k := range exclude {
			omit = k
		}
		id, err := p.client.OpenSession(modelID, router.OpenSessionRequest{SessionDuration: durationSec, OmitProvider: omit})
		if err != nil {
			return "", "", err
		}
		prov, _ := p.sessionProvider(modelID, id)
		// Do not rely on the router alone: a session that landed on a
		// configured exclusion is closed and nothing is handed out.
		if prov != "" && p.isConfiguredExcluded(modelID, prov) {
			warnf("pool: open with omitProvider landed on an excluded provider for model %s; closing session %s", modelID, short(id))
			if err := p.closeBounded(id); err != nil {
				warnf("pool: close session %s on an excluded provider: %v (left for the sweep)", short(id), err)
			} else {
				p.notifyClosed(id)
			}
			return "", "", ErrNoEligibleProvider
		}
		return id, prov, nil
	}
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
		lastErr = ErrNoAlternate
	}
	return "", "", fmt.Errorf("open on another provider (%d tried): %w", attempts, lastErr)
}

// isConfiguredExcluded reports whether prov is in UPLINK_PROVIDER_EXCLUDE
// for the model.
func (p *Pool) isConfiguredExcluded(modelID, prov string) bool {
	for k := range p.excludedProviders(modelID) {
		if strings.EqualFold(k, prov) {
			return true
		}
	}
	return false
}

// openAvoidingCooling is the normal open: plain open by model when no
// provider of this model is avoided, else the exclusion open. Cool-downs
// and the one-shot after-cut exclusion are soft: when every provider is
// avoided, or the bids cannot be read, or the exclusion open fails, it
// falls back (best effort: a request needs a session). Exclusions set in
// UPLINK_PROVIDER_EXCLUDE are hard: the fallback still avoids them, and
// when only excluded providers remain it returns ErrNoEligibleProvider.
func (p *Pool) openAvoidingCooling(modelID string, durationSec int) (string, string, error) {
	plain := func() (string, string, error) {
		id, err := p.client.OpenSession(modelID, router.OpenSessionRequest{SessionDuration: durationSec})
		if err != nil {
			return "", "", err
		}
		prov, _ := p.sessionProvider(modelID, id)
		return id, prov, nil
	}
	configured := p.excludedProviders(modelID)
	// fallback drops the soft avoidances and keeps the configured ones.
	fallback := func(bids []router.RatedBid, bidsOK bool) (string, string, error) {
		if len(configured) == 0 {
			return plain()
		}
		if !bidsOK {
			if len(configured) == 1 {
				// The router's omitProvider keeps the one exclusion.
				return p.openExcluding(modelID, durationSec, configured, nil, false)
			}
			return "", "", fmt.Errorf("open without bids would ignore %d configured exclusions", len(configured))
		}
		alts, n := alternates(bids, configured)
		if n == 0 {
			return "", "", ErrNoEligibleProvider
		}
		return p.openExcluding(modelID, durationSec, configured, alts, len(configured) > 1)
	}
	exclude := p.avoidSet(modelID)
	if len(exclude) == 0 {
		return plain()
	}
	cut := p.pendingCut(modelID)
	bids, err := p.client.RatedBids(modelID)
	if err != nil {
		slog.Warn("pool: rated bids unavailable; opening without soft exclusions", "model", modelID, "err", err)
		return fallback(nil, false)
	}
	alts, n := alternates(bids, exclude)
	if n == 0 {
		if len(configured) > 0 {
			if _, m := alternates(bids, configured); m == 0 {
				slog.Warn("pool: only configured-excluded providers serve this model", "model", modelID)
				return "", "", ErrNoEligibleProvider
			}
		}
		slog.Warn("pool: every eligible provider is cooling; opening without soft exclusions", "model", modelID)
		id, prov, err := fallback(bids, true)
		if err == nil && cut != "" {
			p.clearCut(modelID)
		}
		return id, prov, err
	}
	id, prov, err := p.openExcluding(modelID, durationSec, exclude, alts, cut != "")
	if errors.Is(err, ErrNoEligibleProvider) {
		return "", "", err
	}
	if err != nil {
		slog.Warn("pool: open avoiding cooling providers failed; opening without soft exclusions", "model", modelID, "err", err)
		return fallback(bids, true)
	}
	if cut != "" {
		p.clearCut(modelID)
	}
	return id, prov, nil
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
	Long           bool // the replacement was opened long (counted in Limits)
	// Lease is the caller's hold on SessionID, taken in the same critical
	// section that pooled it (nil when reopen was false, or the
	// replacement is already retiring).
	Lease *Lease
}

// FailoverOpts describe the request that hit the failure.
type FailoverOpts struct {
	KeyID       string
	Shared      bool // master or prompt key: never opens long sessions
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
	long := durationSec > p.DurationSec()
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
	ao := AcquireOpts{KeyID: o.KeyID, Shared: o.Shared}
	if p.isRetired(failedID) {
		// Already failed over, but its replacement is gone (open failed or
		// expired): open normally, avoiding cooling providers.
		res.Replaced = true
		if !o.Reopen {
			return res, nil
		}
		p.mu.Lock()
		stamp, err := p.reserveLocked(key, ao, long, long, "")
		p.mu.Unlock()
		if err != nil {
			return res, err
		}
		id, prov, err := p.openAvoidingCooling(modelID, durationSec)
		p.finishFailoverOpen(key, ao, long, stamp, id, prov, durationSec, err, &res)
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
	// A long replacement counts toward the long-session limits like any
	// long open; the failed session itself is not counted against it.
	var stamp time.Time
	if o.Reopen {
		p.mu.Lock()
		stamp, err = p.reserveLocked(key, ao, long, long, failedID)
		p.mu.Unlock()
		if err != nil {
			return res, err
		}
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

	// 5. Open on another provider. 6. Pool it with its provider, leased.
	id, prov, err := p.openExcluding(modelID, durationSec, exclude, alts, false)
	p.finishFailoverOpen(key, ao, long, stamp, id, prov, durationSec, err, &res)
	if err != nil {
		return res, err
	}
	log.Printf("pool: failed over model %s from provider %s to session %s (provider %s)", modelID, short(failedProv), short(id), short(prov))
	return res, nil
}

// finishFailoverOpen ends the reservation and, when the open worked, pools
// the replacement and leases it for the caller in one critical section.
func (p *Pool) finishFailoverOpen(key string, ao AcquireOpts, long bool, stamp time.Time, id, prov string, durationSec int, err error, res *FailoverResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreserveLocked(ao.KeyID, key, long)
	if err != nil {
		if long {
			p.refundLocked(key, ao.KeyID, stamp)
		}
		return
	}
	now := p.now()
	p.entries[key] = entry{sessionID: id, expiresAt: now.Add(time.Duration(durationSec)*time.Second - expiryBuffer), provider: prov}
	st := p.newStateLocked(key, id, now, now.Add(time.Duration(durationSec)*time.Second), ao.KeyID, long)
	res.Lease = p.leaseLocked(id, st, ao.KeyID)
	res.SessionID, res.Provider, res.Opened, res.Long = id, prov, true, long
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
