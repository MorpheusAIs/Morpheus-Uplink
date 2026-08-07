// Package pool keeps at most one open proxy-router session per model and
// reuses it across requests until it nears its on-chain expiry.
package pool

import (
	"fmt"
	"log"
	"math/big"
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
	stake     *big.Int
}

type Pool struct {
	client   *router.Client
	duration int
	failover bool
	direct   bool

	mu      sync.Mutex
	entries map[string]entry       // modelID -> session
	opening map[string]*sync.Mutex // per-model open lock

	// Last on-chain open-session scan (for GUI), refreshed by Rehydrate.
	chainMu       sync.Mutex
	chainOpen     []ChainSession
	chainFetched  time.Time
	rehydrateErr  string
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

// DurationSec is the configured default session length.
func (p *Pool) DurationSec() int { return p.duration }

// EnsureResult is returned by EnsureDuration.
type EnsureResult struct {
	SessionID   string
	Opened      bool // true when a new on-chain session was opened
	DurationSec int
}

// Ensure returns an open session for the model, opening one if needed.
func (p *Pool) Ensure(modelID string) (string, error) {
	res, err := p.EnsureDuration(modelID, 0)
	if err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// EnsureDuration returns an open session. durationSec <= 0 uses the pool default.
// An existing pooled session is reused when it still has usable time left.
func (p *Pool) EnsureDuration(modelID string, durationSec int) (EnsureResult, error) {
	if durationSec <= 0 {
		durationSec = p.duration
	}
	p.mu.Lock()
	if e, ok := p.entries[modelID]; ok && time.Now().Before(e.expiresAt) {
		p.mu.Unlock()
		return EnsureResult{SessionID: e.sessionID, Opened: false, DurationSec: durationSec}, nil
	}
	lock, ok := p.opening[modelID]
	if !ok {
		lock = &sync.Mutex{}
		p.opening[modelID] = lock
	}
	p.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	if e, ok := p.entries[modelID]; ok && time.Now().Before(e.expiresAt) {
		p.mu.Unlock()
		return EnsureResult{SessionID: e.sessionID, Opened: false, DurationSec: durationSec}, nil
	}
	p.mu.Unlock()

	sessionID, err := p.client.OpenSession(modelID, router.OpenSessionRequest{
		SessionDuration: durationSec,
		Failover:        p.failover,
		DirectPayment:   p.direct,
	})
	if err != nil {
		return EnsureResult{}, err
	}
	p.mu.Lock()
	p.entries[modelID] = entry{
		sessionID: sessionID,
		expiresAt: time.Now().Add(time.Duration(durationSec)*time.Second - expiryBuffer),
	}
	p.mu.Unlock()
	log.Printf("pool: opened session %s for model %s (duration %ds)", sessionID, modelID, durationSec)
	return EnsureResult{SessionID: sessionID, Opened: true, DurationSec: durationSec}, nil
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

// Rehydrate loads open sessions from the router (which reads chain), adopts
// one usable session per model into the reuse pool, and asks the router to
// close sessions that are past endsAt (unused stake returns; used → daylock).
//
// force=false skips work when a scan ran recently (status polling).
func (p *Pool) Rehydrate() error {
	return p.rehydrate(false)
}

// RehydrateForce always rescans (e.g. after Close all).
func (p *Pool) RehydrateForce() error {
	return p.rehydrate(true)
}

func (p *Pool) rehydrate(force bool) error {
	if !force {
		p.chainMu.Lock()
		fresh := time.Since(p.chainFetched) < 20*time.Second && p.rehydrateErr == ""
		p.chainMu.Unlock()
		if fresh {
			return nil
		}
	}

	addr, err := p.client.WalletAddress()
	if err != nil {
		p.setRehydrateErr(err.Error())
		return err
	}
	open, err := p.client.ListOpenSessions(addr)
	if err != nil {
		p.setRehydrateErr(err.Error())
		return err
	}

	now := time.Now()
	nowUnix := now.Unix()
	best := map[string]router.Session{} // model -> farthest-ending usable
	display := make([]ChainSession, 0, len(open))

	for _, s := range open {
		usable := s.EndsAt > nowUnix+int64(expiryBuffer.Seconds())
		stakeStr := "0"
		if s.Stake != nil {
			stakeStr = s.Stake.String()
		}
		if !usable {
			// Past end (or inside buffer): ask router to close so unused
			// returns and used moves to the daylock queue.
			if err := p.client.CloseSession(s.ID); err != nil {
				log.Printf("pool: close expired session %s: %v", s.ID, err)
				// Still show it — close failed, stake remains active.
				display = append(display, ChainSession{
					ModelID: s.ModelAgentID, SessionID: s.ID, StakeWei: stakeStr,
					EndsAt: time.Unix(s.EndsAt, 0).UTC(), Usable: false,
				})
			} else {
				log.Printf("pool: closed expired session %s (model %s)", s.ID, s.ModelAgentID)
			}
			continue
		}
		if cur, ok := best[s.ModelAgentID]; !ok || s.EndsAt > cur.EndsAt {
			best[s.ModelAgentID] = s
		}
		display = append(display, ChainSession{
			ModelID: s.ModelAgentID, SessionID: s.ID, StakeWei: stakeStr,
			EndsAt: time.Unix(s.EndsAt, 0).UTC(), Usable: true,
		})
	}

	adopted := 0
	p.mu.Lock()
	for modelID, s := range best {
		ends := time.Unix(s.EndsAt, 0)
		usableUntil := ends.Add(-expiryBuffer)
		if e, ok := p.entries[modelID]; ok && e.expiresAt.After(usableUntil) {
			// Keep a fresher local open.
			continue
		}
		p.entries[modelID] = entry{
			sessionID: s.ID,
			expiresAt: usableUntil,
			stake:     s.Stake,
		}
		adopted++
	}
	// Mark which display rows are pooled.
	pooledNow := map[string]bool{}
	for _, e := range p.entries {
		pooledNow[e.sessionID] = true
	}
	p.mu.Unlock()

	for i := range display {
		display[i].Pooled = pooledNow[display[i].SessionID]
	}

	p.chainMu.Lock()
	p.chainOpen = display
	p.chainFetched = now
	p.rehydrateErr = ""
	p.chainMu.Unlock()

	log.Printf("pool: rehydrated %d open on-chain session(s), adopted %d into reuse pool (wallet %s)",
		len(open), adopted, addr)
	return nil
}

func (p *Pool) setRehydrateErr(msg string) {
	p.chainMu.Lock()
	p.rehydrateErr = msg
	p.chainMu.Unlock()
}

// CloseSession closes one session by id (pooled and/or on-chain), then rescans.
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
	if err := p.client.CloseSession(sessionID); err != nil {
		_ = p.RehydrateForce()
		return err
	}
	return p.RehydrateForce()
}

// CloseAllResult summarizes a mass close attempt.
type CloseAllResult struct {
	Attempted int      `json:"attempted"`
	Closed    int      `json:"closed"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

// CloseAll closes every pooled session, then any remaining on-chain opens
// from the last scan (recovers unused stake sooner than waiting for expiry).
func (p *Pool) CloseAll() CloseAllResult {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]entry{}
	p.mu.Unlock()
	seen := map[string]bool{}
	res := CloseAllResult{}
	tryClose := func(sessionID, label string) {
		if sessionID == "" || seen[sessionID] {
			return
		}
		seen[sessionID] = true
		res.Attempted++
		if err := p.client.CloseSession(sessionID); err != nil {
			res.Failed++
			msg := label + ": " + err.Error()
			if len(res.Errors) < 5 {
				res.Errors = append(res.Errors, msg)
			}
			log.Printf("pool: close session %s: %v", sessionID, err)
			return
		}
		res.Closed++
		log.Printf("pool: closed session %s (%s)", sessionID, label)
	}
	for modelID, e := range entries {
		tryClose(e.sessionID, "pooled "+modelID)
	}
	for _, s := range p.ChainOpen() {
		tryClose(s.SessionID, "on-chain "+s.ModelID)
	}
	_ = p.RehydrateForce()
	return res
}
