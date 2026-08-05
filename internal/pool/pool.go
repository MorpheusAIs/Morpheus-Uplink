// Package pool keeps at most one open proxy-router session per model and
// reuses it across requests until it nears its on-chain expiry.
package pool

import (
	"log"
	"sync"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// expiryBuffer stops using a session this long before its on-chain end so
// we never send a prompt into a session that expires mid-answer.
const expiryBuffer = 90 * time.Second

type entry struct {
	sessionID string
	expiresAt time.Time
}

type Pool struct {
	client   *router.Client
	duration int
	failover bool
	direct   bool

	mu      sync.Mutex
	entries map[string]entry     // modelID -> session
	opening map[string]*sync.Mutex // per-model open lock
}

func New(client *router.Client, durationSec int, failover, directPayment bool) *Pool {
	return &Pool{
		client:   client,
		duration: durationSec,
		failover: failover,
		direct:   directPayment,
		entries:  map[string]entry{},
		opening:  map[string]*sync.Mutex{},
	}
}

// Ensure returns an open session for the model, opening one if needed.
// Concurrent requests for the same model share a single open call.
func (p *Pool) Ensure(modelID string) (string, error) {
	p.mu.Lock()
	if e, ok := p.entries[modelID]; ok && time.Now().Before(e.expiresAt) {
		p.mu.Unlock()
		return e.sessionID, nil
	}
	lock, ok := p.opening[modelID]
	if !ok {
		lock = &sync.Mutex{}
		p.opening[modelID] = lock
	}
	p.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	// Someone else may have opened it while we waited on the lock.
	p.mu.Lock()
	if e, ok := p.entries[modelID]; ok && time.Now().Before(e.expiresAt) {
		p.mu.Unlock()
		return e.sessionID, nil
	}
	p.mu.Unlock()

	sessionID, err := p.client.OpenSession(modelID, router.OpenSessionRequest{
		SessionDuration: p.duration,
		Failover:        p.failover,
		DirectPayment:   p.direct,
	})
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.entries[modelID] = entry{
		sessionID: sessionID,
		expiresAt: time.Now().Add(time.Duration(p.duration)*time.Second - expiryBuffer),
	}
	p.mu.Unlock()
	log.Printf("pool: opened session %s for model %s (duration %ds)", sessionID, modelID, p.duration)
	return sessionID, nil
}

// Invalidate drops the pooled session for a model (e.g. after a failed
// prompt) so the next request opens a fresh one.
func (p *Pool) Invalidate(modelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, modelID)
}

type Snapshot struct {
	ModelID   string    `json:"modelId"`
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (p *Pool) Snapshot() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Snapshot, 0, len(p.entries))
	for modelID, e := range p.entries {
		out = append(out, Snapshot{ModelID: modelID, SessionID: e.sessionID, ExpiresAt: e.expiresAt})
	}
	return out
}

// CloseAll closes every pooled session on the chain (recovers unused stake
// sooner than waiting for expiry).
func (p *Pool) CloseAll() {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]entry{}
	p.mu.Unlock()
	for modelID, e := range entries {
		if err := p.client.CloseSession(e.sessionID); err != nil {
			log.Printf("pool: close session %s (model %s): %v", e.sessionID, modelID, err)
		} else {
			log.Printf("pool: closed session %s (model %s)", e.sessionID, modelID)
		}
	}
}
