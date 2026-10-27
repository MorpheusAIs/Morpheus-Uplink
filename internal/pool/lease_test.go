package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// fakeClock is a settable pool clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type leaseSess struct {
	model, prov    string
	opened, ends   int64
	closed         bool
	durationOpened int
}

// leaseRouter is a proxy-router for lease tests: opens by model on the
// fake clock, lists, details and closes sessions; closes can fail.
type leaseRouter struct {
	mu        sync.Mutex
	clock     *fakeClock
	sessions  map[string]*leaseSess
	order     []string
	opens     []int // durations
	closes    []string
	closeFail atomic.Bool
	n         int
}

func newLeaseRouter(t *testing.T, c *fakeClock) (*leaseRouter, *Pool) {
	lr := &leaseRouter{clock: c, sessions: map[string]*leaseSess{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	})
	row := func(id string, s *leaseSess) map[string]any {
		closedAt := int64(0)
		if s.closed {
			closedAt = s.ends
		}
		return map[string]any{"Id": id, "ModelAgentId": s.model, "Stake": "1000", "OpenedAt": s.opened, "EndsAt": s.ends, "ClosedAt": closedAt,
			"Provider": s.prov}
	}
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		rows := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			for _, id := range lr.order {
				if s := lr.sessions[id]; !s.closed {
					rows = append(rows, row(id, s))
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": rows})
	})
	mux.HandleFunc("GET /blockchain/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		s := lr.sessions[r.PathValue("id")]
		if s == nil {
			http.Error(w, "not found", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session": row(r.PathValue("id"), s)})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		id := r.PathValue("id")
		lr.closes = append(lr.closes, id)
		if lr.closeFail.Load() {
			http.Error(w, `{"error":"close failed"}`, 500)
			return
		}
		if s := lr.sessions[id]; s != nil {
			s.closed = true
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", func(w http.ResponseWriter, r *http.Request) {
		m := r.PathValue("id")
		bids := []map[string]any{}
		for i, pv := range []string{leaseProv1, leaseProv2} {
			bid := m + "|" + pv
			bids = append(bids, map[string]any{"ID": bid, "Bid": map[string]any{"Id": bid, "Provider": pv}, "Score": float64(10 - i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bids": bids})
	})
	open := func(w http.ResponseWriter, r *http.Request, model, prov string) {
		var body struct {
			SessionDuration int `json:"sessionDuration"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		lr.mu.Lock()
		defer lr.mu.Unlock()
		lr.n++
		id := fmt.Sprintf("0xs%03d", lr.n)
		now := lr.clock.Now().Unix()
		lr.sessions[id] = &leaseSess{model: model, prov: prov, opened: now, ends: now + int64(body.SessionDuration), durationOpened: body.SessionDuration}
		lr.order = append(lr.order, id)
		lr.opens = append(lr.opens, body.SessionDuration)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": id})
	}
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		open(w, r, r.PathValue("id"), leaseProv1)
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		m, pv, _ := strings.Cut(r.PathValue("id"), "|")
		open(w, r, m, pv)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.SetClock(c.Now)
	return lr, p
}

// addChain puts a session on chain that the pool did not open (boot adoption).
func (lr *leaseRouter) addChain(id, model string, opened, ends time.Time) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.sessions[id] = &leaseSess{model: model, prov: leaseProv1, opened: opened.Unix(), ends: ends.Unix()}
	lr.order = append(lr.order, id)
}

func (lr *leaseRouter) snapshot() (opens []int, closes []string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return append([]int(nil), lr.opens...), append([]string(nil), lr.closes...)
}

func (lr *leaseRouter) closedCount(id string) int {
	_, cl := lr.snapshot()
	n := 0
	for _, c := range cl {
		if c == id {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

const (
	leaseProv1 = "0x2222222222222222222222222222222222222222"
	leaseProv2 = "0x3333333333333333333333333333333333333333"
)

const modelC = "0xcccc000000000000000000000000000000000000000000000000000000000000"
const modelD = "0xdddd000000000000000000000000000000000000000000000000000000000000"

func mustAcquire(t *testing.T, p *Pool, model string, o AcquireOpts) (*Lease, AcquireResult) {
	t.Helper()
	l, r, err := p.Acquire(model, o)
	if err != nil {
		t.Fatalf("acquire %s %+v: %v", model[:6], o, err)
	}
	return l, r
}

// S5-6 and S5-15 (pool part): reuse by remaining life.
func TestS5ReuseByRemaining(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l1, r1 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	if !r1.Opened || r1.DurationSec != 600 {
		t.Fatalf("first %+v", r1)
	}
	c.Advance(400 * time.Second) // 200 s of usable life left
	l2, r2 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", TotalSec: 600})
	if !r2.Opened || !r2.Reopened || r2.SessionID == r1.SessionID || r2.DurationSec != 720 {
		t.Fatalf("reopen %+v", r2)
	}
	if lr.closedCount(r1.SessionID) != 0 {
		t.Fatal("old session closed while a request was in flight on it")
	}
	l1.Done()
	waitUntil(t, "old close", func() bool { return lr.closedCount(r1.SessionID) == 1 })
	l2.Done()
	// Reused when it has the life: 720 s session, 100 s later, total 600 → reuse.
	c.Advance(100 * time.Second)
	l3, r3 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", TotalSec: 500})
	defer l3.Done()
	if r3.Opened || r3.SessionID != r2.SessionID || r3.Remaining != 620*time.Second {
		t.Fatalf("reuse %+v", r3)
	}
}

// S5-15: a reused session with 900 s left and a request of 600 is reused;
// a reopen never exceeds the maximum.
func TestS5ReuseDecisionBeforeRemainingClamp(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	l.Done()
	c.Advance(300 * time.Second) // 900 s left
	l2, r2 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", TotalSec: 600})
	l2.Done()
	if r2.Opened || r2.SessionID != r.SessionID || r2.Remaining != 900*time.Second {
		t.Fatalf("want reuse with 900 s, got %+v", r2)
	}
	c.Advance(600 * time.Second) // 300 s left: reopen, capped at the max
	l3, r3 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", TotalSec: 1680})
	l3.Done()
	if !r3.Opened || r3.DurationSec != 1800 {
		t.Fatalf("reopen %+v", r3)
	}
	for _, d := range func() []int { o, _ := lr.snapshot(); return o }() {
		if d > 1800 {
			t.Fatalf("opened %d s > max", d)
		}
	}
}

// S5-8: the idle sweep closes after the grace, never with a request in flight.
func TestS5IdleSweep(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	c.Advance(10 * time.Minute)
	if n := p.SweepIdle(5 * time.Minute); n != 0 || lr.closedCount(r.SessionID) != 0 {
		t.Fatal("swept a session with a request in flight")
	}
	l.Done()
	c.Advance(4 * time.Minute)
	if n := p.SweepIdle(5 * time.Minute); n != 0 {
		t.Fatal("swept before the grace")
	}
	c.Advance(time.Minute)
	if n := p.SweepIdle(5 * time.Minute); n != 1 || lr.closedCount(r.SessionID) != 1 {
		t.Fatalf("sweep closed %d", n)
	}
	if len(p.Snapshot()) != 0 {
		t.Fatal("swept session still pooled")
	}
}

// S5-8: a failed close is retried by the sweep at most 3 times.
func TestS5SweepRetriesFailedClose(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	lr.closeFail.Store(true)
	c.Advance(6 * time.Minute)
	for i := 0; i < 6; i++ {
		p.SweepIdle(5 * time.Minute)
	}
	if n := lr.closedCount(r.SessionID); n != 3 {
		t.Fatalf("close attempts %d, want 3", n)
	}
}

// S5-7 (pool part): release outcomes.
func TestS5ReleaseOutcomes(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	if out := p.Release(modelA, "k1"); out != (ReleaseOutcome{Reason: "not_held"}) {
		t.Fatalf("no session: %+v", out)
	}
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	if out := p.Release(modelA, "k2"); out != (ReleaseOutcome{Reason: "not_held"}) {
		t.Fatalf("other key: %+v", out)
	}
	l2, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k2"})
	l2.Done()
	if out := p.Release(modelA, "k1"); out != (ReleaseOutcome{Released: true, Reason: "shared"}) {
		t.Fatalf("shared: %+v", out)
	}
	if out := p.Release(modelA, "k2"); out != (ReleaseOutcome{Released: true, Reason: "in_flight"}) {
		t.Fatalf("in flight: %+v", out)
	}
	l.Done()
	l3, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k3"})
	l3.Done()
	if out := p.Release(modelA, "k3"); out != (ReleaseOutcome{Released: true, Closed: true}) {
		t.Fatalf("sole holder: %+v", out)
	}
	if lr.closedCount(r.SessionID) != 1 || len(p.Snapshot()) != 0 {
		t.Fatal("sole-holder release did not close")
	}
	if out := p.Release(modelA, "k3"); out != (ReleaseOutcome{Reason: "not_held"}) {
		t.Fatalf("after close: %+v", out)
	}
}

// S5-9: failover on a session another request is streaming on retires it;
// it closes when that request ends.
func TestS5FailoverWithInflightRetires(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	other, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k2"})
	fr, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k1", DurationSec: 600, Reopen: true})
	defer fr.Lease.Done()
	if err != nil && !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("failover: %v", err)
	}
	if !fr.CloseDeferred || lr.closedCount(r.SessionID) != 0 {
		t.Fatalf("closed under another request's stream: %+v", fr)
	}
	if p.LeaseSession(r.SessionID, "k3") != nil {
		t.Fatal("lease granted on a retired session")
	}
	other.Done()
	waitUntil(t, "deferred close", func() bool { return lr.closedCount(r.SessionID) == 1 })
}

// S5-11: per-key held limits (1 per model, 2 in total).
func TestS5PerKeyHeldLimits(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	p.SetLimits(Limits{PerKeyModel: 1, PerKey: 2, Global: 10, OpensPerHour: 10})
	for _, m := range []string{modelA, modelB} {
		l, _ := mustAcquire(t, p, m, AcquireOpts{KeyID: "k1", DurationSec: 1200})
		l.Done()
	}
	if _, _, err := p.Acquire(modelC, AcquireOpts{KeyID: "k1", DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("third model: %v", err)
	}
	// Another key is not affected.
	l, _ := mustAcquire(t, p, modelC, AcquireOpts{KeyID: "k2", DurationSec: 1200})
	l.Done()
	// Per model: k1 still holds its long session on A (displaced from the
	// pool entry by an adopted one); a second long open on A is refused.
	p.mu.Lock()
	p.entries[modelA] = entry{sessionID: "0xadopted", expiresAt: p.now().Add(100 * time.Second)}
	p.mu.Unlock()
	p.SetLimits(Limits{PerKeyModel: 1, PerKey: 5, Global: 10, OpensPerHour: 10})
	if _, _, err := p.Acquire(modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200, TotalSec: 600}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("second long on one model: %v", err)
	}
}

// S5-16: the third counted open within an hour → refused; release does not refund.
func TestS5HourlyOpenLimit(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	p.SetLimits(Limits{PerKeyModel: 1, PerKey: 2, Global: 3, OpensPerHour: 2})
	// open → release → open loop.
	for i := 0; i < 2; i++ {
		l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
		if !r.Opened || !r.Long {
			t.Fatalf("loop %d: %+v", i, r)
		}
		l.Done()
		if out := p.Release(modelA, "k1"); !out.Closed {
			t.Fatalf("release %+v", out)
		}
	}
	if _, _, err := p.Acquire(modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("third open in the hour: %v", err)
	}
	// A long header on every call (total 1680 → reopen whenever under 1710 s left).
	l, _ := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k2", TotalSec: 1680})
	l.Done()
	c.Advance(100 * time.Second)
	l, _ = mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k2", TotalSec: 1680})
	l.Done()
	c.Advance(100 * time.Second)
	if _, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k2", TotalSec: 1680}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("third long-header reopen: %v", err)
	}
	// After the hour the count is free again.
	c.Advance(time.Hour)
	l, _ = mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	l.Done()
}

// S5-25: reopen-by-remaining at normal length counts too.
func TestS5NormalLengthReopenCounted(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	l, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"}) // plain open: not counted
	l.Done()
	for i := 0; i < 2; i++ {
		c.Advance(200 * time.Second) // 400 s left → 590+30 does not fit
		l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", TotalSec: 590})
		if !r.Reopened || r.DurationSec != 710 {
			t.Fatalf("reopen %d: %+v", i, r)
		}
		l.Done()
		c.Advance(320 * time.Second) // 710-s session: 390 s left next round
	}
	c.Advance(200 * time.Second)
	if _, _, err := p.Acquire(modelA, AcquireOpts{KeyID: "k1", TotalSec: 590}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("third reopen: %v", err)
	}
}

// S5-17: a fourth concurrent long session across keys → refused; shared
// keys never open long sessions.
func TestS5GlobalLongCapAndSharedKeys(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	for i, m := range []string{modelA, modelB, modelC} {
		l, _ := mustAcquire(t, p, m, AcquireOpts{KeyID: fmt.Sprintf("k%d", i), DurationSec: 1200})
		l.Done()
	}
	if _, _, err := p.Acquire(modelD, AcquireOpts{KeyID: "k9", DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("fourth long: %v", err)
	}
	if _, _, err := p.Acquire(modelD, AcquireOpts{KeyID: "master", Shared: true, DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("shared key long: %v", err)
	}
	// A default-size open is unaffected.
	l, _ := mustAcquire(t, p, modelD, AcquireOpts{KeyID: "master", Shared: true})
	l.Done()
}

// S5-26: a retired session that is draining does not count toward its
// opener's held caps, but does toward the global cap.
func TestS5DrainingRetiredCountsGlobalOnly(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetLimits(Limits{PerKeyModel: 1, PerKey: 1, Global: 2, OpensPerHour: 10})
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	if _, err := p.Failover(modelA, r.SessionID, FailoverOpts{DurationSec: 600}); err != nil && !errors.Is(err, ErrNoAlternate) {
		t.Fatal(err)
	}
	// k1's long session is retired and draining (l still in flight).
	l2, r2 := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	if !r2.Long {
		t.Fatalf("%+v", r2)
	}
	l2.Done()
	if p.LongOpen() != 2 {
		t.Fatalf("long open %d", p.LongOpen())
	}
	if _, _, err := p.Acquire(modelC, AcquireOpts{KeyID: "k2", DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("global cap with a draining session: %v", err)
	}
	l.Done()
	waitUntil(t, "drained close", func() bool { return lr.closedCount(r.SessionID) == 1 })
	l3, _ := mustAcquire(t, p, modelC, AcquireOpts{KeyID: "k2", DurationSec: 1200})
	l3.Done()
}

// S5-27: long sessions adopted at boot count toward the global cap; a new
// pool (restart) starts with zero hourly counters.
func TestS5AdoptedLongCountsGlobalAndRestartResets(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	now := c.Now()
	lr.addChain("0xboot1", modelA, now.Add(-time.Minute), now.Add(30*time.Minute))
	lr.addChain("0xboot2", modelB, now.Add(-time.Minute), now.Add(30*time.Minute))
	lr.addChain("0xboot3", modelC, now.Add(-time.Minute), now.Add(30*time.Minute))
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	if p.LongOpen() != 3 {
		t.Fatalf("adopted long %d", p.LongOpen())
	}
	if _, _, err := p.Acquire(modelD, AcquireOpts{KeyID: "k1", DurationSec: 1200}); !errors.Is(err, ErrLongSessionLimit) {
		t.Fatalf("over global with adopted: %v", err)
	}
	// Restart: a fresh pool has no hourly history.
	_, p2 := newLeaseRouter(t, c)
	p2.SetLimits(Limits{PerKeyModel: 1, PerKey: 2, Global: 3, OpensPerHour: 1})
	l, _ := mustAcquire(t, p2, modelD, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	l.Done()
	p2.mu.Lock()
	n := len(p2.opensLog)
	p2.mu.Unlock()
	if n != 1 {
		t.Fatalf("fresh pool counters %d", n)
	}
}

// S5-24: ten default-size calls (no request-timeout header) spread over
// the session's usable life on one model open one session.
func TestS5DefaultCallsOpenOneSession(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	for i := 0; i < 10; i++ {
		l, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
		l.Done()
		p.SweepIdle(5 * time.Minute)
		c.Advance(56 * time.Second) // 10 calls over about 8.5 minutes
	}
	if o, _ := lr.snapshot(); len(o) != 1 {
		t.Fatalf("opens %d", len(o))
	}
}

// Security gate N-1: the same calls spread over more than 9 minutes (past the
// 510 s usable life of a 600 s session) give at most one replacement, and
// at most one open counted in the hourly log.
func TestS5DefaultCallsOverNineMinutes(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	start := c.Now()
	for i := 0; i < 10; i++ {
		if i > 0 {
			c.Advance(61 * time.Second) // last call at 549 s
		}
		l, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
		l.Done()
		p.SweepIdle(5 * time.Minute)
	}
	if span := c.Now().Sub(start); span <= 9*time.Minute {
		t.Fatalf("span %v", span)
	}
	o, _ := lr.snapshot()
	p.mu.Lock()
	counted := len(p.opensLog["k1|"+normModel(modelA)])
	p.mu.Unlock()
	if len(o) > 2 || counted > 1 {
		t.Fatalf("opens %d counted %d", len(o), counted)
	}
}

// S5-13: the pooled fast path takes its lease under the pool lock.
func TestS5FastPathLeaseUnderLock(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	l, _ := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	var checked atomic.Int32
	p.SetTestHookLease(func(id string, inflight int) {
		// Runs with p.mu held: the lease is visible in the state map.
		if st := p.sess[id]; st == nil || st.inflight != inflight || inflight < 1 || p.mu.TryLock() {
			t.Error("lease not taken under the pool lock")
		}
		checked.Add(1)
	})
	l2, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	if r.Opened || checked.Load() != 1 {
		t.Fatalf("fast path %+v checked=%d", r, checked.Load())
	}
	l2.Done()
	l2.Done() // idempotent
	if p.InFlight(r.SessionID) != 0 {
		t.Fatal("double Done went negative or kept a lease")
	}
}

// S5-12: N goroutines request while others release and sweep the same
// model: no lease lands on a session after it entered CLOSING; in-flight
// ends at 0.
func TestS5AtomicLeaseRace(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetTestHookLease(func(id string, _ int) {
		// Under the pool lock: CLOSING and RETIRED are set under it too.
		if st := p.sess[id]; st.closing || st.retired {
			t.Error("lease on a closing or retired session")
		}
		lr.mu.Lock()
		closed := lr.sessions[id] != nil && lr.sessions[id].closed
		lr.mu.Unlock()
		if closed {
			t.Error("lease on a session already closed on chain")
		}
	})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", g%3)
			for i := 0; i < 60; i++ {
				l, _, err := p.Acquire(modelA, AcquireOpts{KeyID: key})
				if err != nil {
					t.Error(err)
					return
				}
				time.Sleep(time.Duration(i%3) * 100 * time.Microsecond)
				l.Done()
			}
		}(g)
	}
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, k := range []string{"k0", "k1", "k2"} {
				p.Release(modelA, k)
			}
			p.SweepIdle(0)
		}
	}()
	wg.Wait()
	close(stop)
	bg.Wait()
	if n := p.TotalInFlight(); n != 0 {
		t.Fatalf("in-flight %d after all requests ended", n)
	}
}

// MUST-1: every open is capped at the max, with or without the headers:
// a default above the max (or an explicit duration) never opens longer.
func TestCapAppliesToNoHeaderOpen(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetDurationMax(1800)
	p.SetDuration(3600) // a default above the cap must not open longer
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	if o, _ := lr.snapshot(); len(o) != 1 || o[0] != 1800 || r.DurationSec != 1800 {
		t.Fatalf("no-header open %v %+v, want 1800", o, r)
	}
	l, r = mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1", DurationSec: 7200})
	l.Done()
	if r.DurationSec != 1800 {
		t.Fatalf("explicit duration open %d, want 1800", r.DurationSec)
	}
}

// acquireDone takes and immediately ends a lease (the reuse-order tests
// only need which session Acquire picked).
func acquireDone(p *Pool, modelID string, dur int) (AcquireResult, error) {
	l, res, err := p.Acquire(modelID, AcquireOpts{DurationSec: dur})
	l.Done()
	return res, err
}

// Conformance SHOULD-2: the failover replacement comes back leased, taken in the
// critical section that pooled it (no window for the sweep to close it).
func TestS5FailoverReplacementLeased(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	var leasedUnderLock []string
	p.SetTestHookLease(func(id string, inflight int) { leasedUnderLock = append(leasedUnderLock, id) })
	fr, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true})
	if err != nil || !fr.Opened || fr.Lease == nil || fr.Lease.SessionID != fr.SessionID {
		t.Fatalf("failover %+v %v", fr, err)
	}
	p.mu.Lock()
	inflight, holder := p.sess[fr.SessionID].inflight, p.sess[fr.SessionID].holders["k1"]
	p.mu.Unlock()
	if inflight != 1 || holder.IsZero() || len(leasedUnderLock) != 1 || leasedUnderLock[0] != fr.SessionID {
		t.Fatalf("replacement not leased at open: inflight=%d hooks=%v", inflight, leasedUnderLock)
	}
	fr.Lease.Done()
	// A second failure on the old id finds the replacement and leases it too.
	fr2, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k2", Reopen: true})
	if err != nil || !fr2.Replaced || fr2.Lease == nil || fr2.SessionID != fr.SessionID {
		t.Fatalf("second failover %+v %v", fr2, err)
	}
	fr2.Lease.Done()
}

// Conformance SHOULD-4 / security gate S-4: a failover reopen at a long duration counts
// toward the long-session limits (the failed session is not counted
// against its own replacement), and a refused one closes nothing.
func TestS5FailoverLongCountsLimits(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetLimits(Limits{PerKeyModel: 1, PerKey: 2, Global: 2, OpensPerHour: 10})
	l1, r1 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	l1.Done()
	// Replacing its own long session is allowed and stays long.
	fr, err := p.Failover(modelA, r1.SessionID, FailoverOpts{KeyID: "k1", DurationSec: 1200, Reopen: true})
	if err != nil || !fr.Long || fr.Lease == nil {
		t.Fatalf("own long failover %+v %v", fr, err)
	}
	fr.Lease.Done()
	p.mu.Lock()
	st := p.sess[fr.SessionID]
	long, opener := st.long, st.opener
	p.mu.Unlock()
	if !long || opener != "k1" {
		t.Fatalf("replacement long=%v opener=%q", long, opener)
	}
	// k2 fills the global limit; a long failover for k3 is then refused
	// before anything is closed.
	l2, _ := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k2", DurationSec: 1200})
	defer l2.Done()
	l3, r3 := mustAcquire(t, p, modelC, AcquireOpts{KeyID: "k3"})
	l3.Done()
	_, before := lr.snapshot()
	fr3, err := p.Failover(modelC, r3.SessionID, FailoverOpts{KeyID: "k3", DurationSec: 1200, Reopen: true})
	if !errors.Is(err, ErrLongSessionLimit) || fr3.Lease != nil || fr3.Opened {
		t.Fatalf("long failover over the limit: %+v %v", fr3, err)
	}
	if _, after := lr.snapshot(); len(after) != len(before) {
		t.Fatalf("refused failover closed a session: %v", after)
	}
	// The same failover at the default duration is not long and proceeds.
	fr4, err := p.Failover(modelC, r3.SessionID, FailoverOpts{KeyID: "k3", Reopen: true})
	if err != nil || fr4.Long || fr4.Lease == nil {
		t.Fatalf("default failover %+v %v", fr4, err)
	}
	fr4.Lease.Done()
	// A failover never opens above the duration max.
	p.SetDurationMax(1800)
	l5, r5 := mustAcquire(t, p, modelD, AcquireOpts{KeyID: "k4"})
	l5.Done()
	p.SetLimits(Limits{PerKeyModel: 5, PerKey: 5, Global: 5, OpensPerHour: 10})
	fr5, err := p.Failover(modelD, r5.SessionID, FailoverOpts{KeyID: "k4", DurationSec: 7200, Reopen: true})
	if err != nil || fr5.DurationSec != 1800 {
		t.Fatalf("capped failover %+v %v", fr5, err)
	}
	fr5.Lease.Done()
	if opens, _ := lr.snapshot(); opens[len(opens)-1] != 1800 {
		t.Fatalf("opened at %d", opens[len(opens)-1])
	}
}

// Security gate SHOULD-1 (pool side): with name-keyed exclusions and no
// catalog name for the model, a new open is refused and a failover keeps
// the session (closes nothing).
func TestNameKeyedExclusionUnresolved(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l.Done()
	p.SetProviderExclude(map[string][]string{"some-model": {leaseProv1}}, func(string) string { return "" })
	if _, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"}); !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("open with unresolved name: %v", err)
	}
	if _, err := p.Failover(modelA, r.SessionID, FailoverOpts{KeyID: "k1", Reopen: true}); !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("failover with unresolved name: %v", err)
	}
	if lr.closedCount(r.SessionID) != 0 {
		t.Fatal("failover closed the session")
	}
	if n := p.UnresolvedExclusionRefusals(); n != 2 {
		t.Fatalf("refusals while unresolved = %d, want 2", n)
	}
	p.SetProviderExclude(map[string][]string{"some-model": {leaseProv1}}, func(string) string { return "some-model" })
	if n := p.UnresolvedExclusionRefusals(); n != 0 {
		t.Fatalf("refusals after resolving = %d, want 0", n)
	}
	// Id-keyed exclusions alone never need the name.
	p.SetProviderExclude(map[string][]string{modelC: {leaseProv2}}, func(string) string { return "" })
	if l, _, err := p.Acquire(modelC, AcquireOpts{KeyID: "k1"}); err != nil {
		t.Fatalf("id-keyed: %v", err)
	} else {
		l.Done()
	}
}
