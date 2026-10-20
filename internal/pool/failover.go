package pool

import (
	"errors"
	"fmt"
	"log"
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
		endsAt = now.Add(time.Duration(p.duration)*time.Second + expiryBuffer)
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
		log.Printf("pool: provider of session %s: %v", short(sessionID), err)
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
func (p *Pool) openExcluding(modelID string, durationSec int, exclude map[string]bool, alts []router.RatedBid) (string, string, error) {
	if len(exclude) == 1 {
		var omit string
		for k := range exclude {
			omit = k
		}
		id, err := p.client.OpenSession(modelID, router.OpenSessionRequest{SessionDuration: durationSec, OmitProvider: omit})
		if err != nil {
			return "", "", err
		}
		prov, _ := p.sessionProvider(modelID, id)
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
			log.Printf("pool: open bid %s (provider %s) for model %s: %v", short(b.ID), short(b.Provider), modelID, err)
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

// openAvoidingCooling is the normal open: plain open by model when no
// provider of this model is cooling, else the exclusion open. When every
// provider is cooling, or the bids cannot be read, or the exclusion open
// fails, it falls back to the plain open (best effort: a request needs a
// session).
func (p *Pool) openAvoidingCooling(modelID string, durationSec int) (string, string, error) {
	plain := func() (string, string, error) {
		id, err := p.client.OpenSession(modelID, router.OpenSessionRequest{SessionDuration: durationSec})
		if err != nil {
			return "", "", err
		}
		prov, _ := p.sessionProvider(modelID, id)
		return id, prov, nil
	}
	exclude := p.coolingProviders(modelID)
	if len(exclude) == 0 {
		return plain()
	}
	bids, err := p.client.RatedBids(modelID)
	if err != nil {
		log.Printf("pool: rated bids for %s: %v (opening without exclusions)", modelID, err)
		return plain()
	}
	alts, n := alternates(bids, exclude)
	if n == 0 {
		log.Printf("pool: every provider of model %s is cooling; opening without exclusions", modelID)
		return plain()
	}
	id, prov, err := p.openExcluding(modelID, durationSec, exclude, alts)
	if err != nil {
		log.Printf("pool: open avoiding cooling providers for %s: %v (opening without exclusions)", modelID, err)
		return plain()
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
	DurationSec    int
}

// Failover moves a model off the provider behind failedID. It runs under
// the model's open lock, so concurrent failures on one session do one
// close and one open. With reopen=false (a mid-stream failure) it only
// retires and closes the session; the next request opens through
// EnsureDuration, which avoids cooling providers.
//
// It returns ErrNoAlternate, without closing anything, when no other
// provider serves the model.
func (p *Pool) Failover(modelID, failedID string, durationSec int, reopen bool) (FailoverResult, error) {
	if durationSec <= 0 {
		durationSec = p.duration
	}
	res := FailoverResult{DurationSec: durationSec}
	key := normModel(modelID)
	lock := p.openLock(key)
	lock.Lock()
	defer lock.Unlock()

	// 1. Someone else already swapped the session: use the replacement.
	p.mu.Lock()
	cur, pooled := p.entries[key]
	p.mu.Unlock()
	if pooled && cur.sessionID != failedID && time.Now().Before(cur.expiresAt) {
		res.SessionID, res.Replaced, res.Provider = cur.sessionID, true, cur.provider
		return res, nil
	}
	if p.isRetired(failedID) {
		// Already failed over, but its replacement is gone (open failed or
		// expired): open normally, avoiding cooling providers.
		res.Replaced = true
		if !reopen {
			return res, nil
		}
		id, prov, err := p.openAvoidingCooling(modelID, durationSec)
		if err != nil {
			return res, err
		}
		p.store(key, id, prov, durationSec)
		res.SessionID, res.Provider, res.Opened = id, prov, true
		return res, nil
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
	exclude := p.coolingProviders(modelID)
	exclude[failedProv] = true
	bids, err := p.client.RatedBids(modelID)
	if err != nil {
		log.Printf("pool: failover %s: rated bids: %v (keeping session)", modelID, err)
		return res, fmt.Errorf("%w (bids unavailable: %v)", ErrNoAlternate, err)
	}
	alts, n := alternates(bids, exclude)
	if n == 0 {
		return res, ErrNoAlternate
	}

	// 4. Retire and close the failed session synchronously (bounded), so
	// our own close and open never race on the wallet nonce.
	p.retire(failedID, endsAt)
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && e.sessionID == failedID {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	if err := p.closeBounded(failedID); err != nil {
		res.CloseFailed = true
		log.Printf("pool: failover %s: close session %s: %v (retired; not re-adopted)", modelID, short(failedID), err)
	} else {
		p.notifyClosed(failedID)
	}
	if !reopen {
		return res, nil
	}

	// 5. Open on another provider. 6. Pool it with its provider.
	id, prov, err := p.openExcluding(modelID, durationSec, exclude, alts)
	if err != nil {
		return res, err
	}
	p.store(key, id, prov, durationSec)
	res.SessionID, res.Provider, res.Opened = id, prov, true
	log.Printf("pool: failed over model %s from provider %s to session %s (provider %s)", modelID, short(failedProv), short(id), short(prov))
	return res, nil
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
