package pool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	// visibleAt: the session is listed only from this fake time (a late
	// mint after a cut open); closeAt: it is closed from this fake time
	// (a close that finished after the client gave up). 0: unset.
	visibleAt, closeAt int64
	bid                string // bid id when not bidID(model, prov)
}

// leaseRouter is a proxy-router for lease tests: rates bids, opens by bid
// on the fake clock, lists, details and closes sessions; closes can fail.
// Its by-model open only counts (Uplink never uses it).
type leaseRouter struct {
	mu        sync.Mutex
	clock     *fakeClock
	sessions  map[string]*leaseSess
	order     []string
	opens     []int // durations
	closes    []string
	closeFail atomic.Bool
	n         int
	// bidders overrides the providers bidding on a model (default: both
	// lease providers), in rated order.
	bidders map[string][]string
	// rows, when set for a model, is its rated-bids answer verbatim.
	rows    map[string][]map[string]any
	byModel int      // opens by model (must stay 0: every open is by bid)
	byBid   []string // bid ids opened by bid
	// Health source (Pool.SetHealthyBids): by default every mock bid of a
	// model is healthy; unhealthy drops bid ids, health overrides a
	// model's set verbatim, healthErr makes the source fail.
	unhealthy map[string]bool
	health    map[string][]string
	healthErr error
	// openFault, when set, is asked before every by-bid open: a non-empty
	// body answers status 500 with it (the router's error shape); landed
	// creates the session on chain first (tx sent, receipt wait failed).
	openFault func(bid string) (body string, landed bool)
	// ratedFault, when set, is asked before every rated-bids answer: a
	// non-empty body answers status 500 with it.
	ratedFault func(model string) string
	// Wallet-serialization probes. Every by-bid open counts itself in
	// inflight (peak: maxInflight) and, when arrivals is set, reports its
	// bid there on arrival; when gate is set it then waits until gate is
	// closed or the request is cancelled (gateCancels counts those).
	arrivals    chan string
	gate        chan struct{}
	gateCancels int
	inflight    int
	maxInflight int
	// closeOverlap counts closes that reached the router while an open
	// was in flight.
	closeOverlap int
	// hideLanded: a session landed by openFault stays out of the next
	// hideLanded session-list answers (the router's visibility lag).
	hideLanded int
	hideLeft   int
	landed     map[string]bool
	// lists counts session-list answers (offset 0); listsAfterLand
	// counts those after a session landed through openFault.
	lists, listsAfterLand int

	// The router keeps going after a disconnect. openHang: a by-bid open
	// answers nothing until the client gives up, then the session mints
	// mintDelay later on the fake clock. openTake advances the fake clock
	// by that much inside every open before it answers (a slow open).
	// closeHang: a close answers nothing until the client gives up, then
	// the session closes closeDelay later; closeTake: like openTake.
	openHang, closeHang bool
	mintDelay           time.Duration
	closeDelay          time.Duration
	openTake, closeTake time.Duration
	// Write gauges (own lock: an answer may be written under mu).
	gmu               sync.Mutex
	closeInflight     int
	maxCloseInflight  int
	writesInflight    int // opens plus closes in flight
	maxWritesInflight int
	// extraMint, when set, is asked after each by-bid open that minted:
	// true mints a second session on the same bid (an escalated extra tx
	// that did not revert, or a second router call).
	extraMint func(bid string) bool
	// openStatus, when set, mints the by-bid open (hidden from the next
	// hideLanded lists) and answers this status, like a proxy in front of
	// the router that lost the router's answer (502/503/504).
	openStatus int
	// calls counts every request (router calls per operation).
	calls atomic.Int64
	// closeHook, when set, runs as each close arrives (before closeTake).
	closeHook func(id string)
	// rejects records the rejectExisting flag of every by-bid open;
	// refused counts the opens the guard answered 409. Like the router,
	// a flagged open is refused while a session of the bid's model is
	// open and not ended (a pending late mint included), naming the
	// newest one.
	rejects []bool
	refused int
}

// existingLocked is the router's existing-session guard: the newest open,
// not-ended session of model ("" when none).
func (lr *leaseRouter) existingLocked(model string) string {
	now := lr.clock.Now().Unix()
	for i := len(lr.order) - 1; i >= 0; i-- {
		s := lr.sessions[lr.order[i]]
		if s.model == model && !s.closed && s.ends > now && (s.closeAt == 0 || s.closeAt > now) {
			return lr.order[i]
		}
	}
	return ""
}

// rejectsSeen returns the rejectExisting flags sent so far.
func (lr *leaseRouter) rejectsSeen() []bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return append([]bool(nil), lr.rejects...)
}

// endOnce ends a write gauge when the answer starts (the client cannot
// see it before), not when the handler returns: the next slot holder may
// already be sending by then.
type endOnce struct {
	http.ResponseWriter
	once sync.Once
	end  func()
}

func (e *endOnce) WriteHeader(code int) { e.once.Do(e.end); e.ResponseWriter.WriteHeader(code) }
func (e *endOnce) Write(b []byte) (int, error) {
	e.once.Do(e.end)
	return e.ResponseWriter.Write(b)
}

// visibleLocked: s is on chain at the mock's fake time (lr.mu held).
func (lr *leaseRouter) visibleLocked(s *leaseSess) bool {
	now := lr.clock.Now().Unix()
	if s.closeAt != 0 && now >= s.closeAt {
		s.closed = true
	}
	return s.visibleAt == 0 || now >= s.visibleAt
}

// writeBegin and writeEnd count wallet writes (opens and closes) in flight.
func (lr *leaseRouter) writeBegin(close bool) {
	lr.gmu.Lock()
	defer lr.gmu.Unlock()
	lr.writesInflight++
	lr.maxWritesInflight = max(lr.maxWritesInflight, lr.writesInflight)
	if close {
		lr.closeInflight++
		lr.maxCloseInflight = max(lr.maxCloseInflight, lr.closeInflight)
	}
}

func (lr *leaseRouter) writeEnd(close bool) {
	lr.gmu.Lock()
	defer lr.gmu.Unlock()
	lr.writesInflight--
	if close {
		lr.closeInflight--
	}
}

// writePeaks is the most wallet writes (and closes) ever in flight at once.
func (lr *leaseRouter) writePeaks() (writes, closes int) {
	lr.gmu.Lock()
	defer lr.gmu.Unlock()
	return lr.maxWritesInflight, lr.maxCloseInflight
}

// openSet is the mock's on-chain open session ids at its fake time.
func (lr *leaseRouter) openSet() []string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var out []string
	for _, id := range lr.order {
		if s := lr.sessions[id]; lr.visibleLocked(s) && !s.closed {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// healthy is the mock's healthy-bid source.
func (lr *leaseRouter) healthy(model string) (map[string]struct{}, error) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.healthErr != nil {
		return nil, lr.healthErr
	}
	out := map[string]struct{}{}
	if ids, ok := lr.health[model]; ok {
		for _, id := range ids {
			out[strings.ToLower(id)] = struct{}{}
		}
		return out, nil
	}
	for _, pv := range lr.providersOf(model) {
		if id := bidID(model, pv); !lr.unhealthy[id] {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// setUnhealthy marks a provider's bid on a model not healthy.
func (lr *leaseRouter) setUnhealthy(model, prov string) {
	lr.mu.Lock()
	lr.unhealthy[bidID(model, prov)] = true
	lr.mu.Unlock()
}

// mixedCase writes an address the way the router does (EIP-55 style
// mixed case): the pool must match it case-insensitively.
func mixedCase(addr string) string {
	b := []byte(addr)
	for i := 2; i < len(b); i++ {
		if i%2 == 0 && b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// ratedRow is one router rated-bids row in the real shape.
func ratedRow(model, bid, prov string, score float64) map[string]any {
	return map[string]any{"ID": zeroHash, "Bid": map[string]any{
		"Id": bid, "Provider": mixedCase(prov), "ModelAgentId": model,
		"PricePerSecond": "1000", "Nonce": "0", "CreatedAt": "1700000000", "DeletedAt": "0",
	}, "Score": score}
}

// zeroHash is the outer ID of every rated-bid row: the router leaves it
// unset; the bid id is Bid.Id.
const zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"

// bidID is the mock's bid id for a model and provider (0x + 64 hex).
func bidID(model, prov string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(model) + "|" + prov))
	return "0x" + hex.EncodeToString(sum[:])
}

// bidOwner maps a bid id back to its model and provider.
func (lr *leaseRouter) bidOwner(id string) (string, string, bool) {
	for _, m := range append([]string{modelA, modelB, modelC, modelD}, extraModels...) {
		for _, pv := range []string{leaseProv1, leaseProv2, leaseProv3} {
			if bidID(m, pv) == id {
				return m, pv, true
			}
		}
	}
	return "", "", false
}

func (lr *leaseRouter) providersOf(model string) []string {
	if ps, ok := lr.bidders[model]; ok {
		return ps
	}
	return []string{leaseProv1, leaseProv2}
}

func newLeaseRouter(t *testing.T, c *fakeClock) (*leaseRouter, *Pool) {
	lr := &leaseRouter{clock: c, sessions: map[string]*leaseSess{}, bidders: map[string][]string{}, rows: map[string][]map[string]any{},
		unhealthy: map[string]bool{}, health: map[string][]string{}, landed: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	})
	row := func(id string, s *leaseSess) map[string]any {
		closedAt := int64(0)
		if s.closed {
			closedAt = s.ends
		}
		bid := s.bid
		if bid == "" {
			bid = bidID(s.model, s.prov)
		}
		return map[string]any{"Id": id, "ModelAgentId": s.model, "Stake": "1000", "OpenedAt": s.opened, "EndsAt": s.ends, "ClosedAt": closedAt,
			"Provider": s.prov, "BidID": bid}
	}
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		rows := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			lr.lists++
			if len(lr.landed) > 0 {
				lr.listsAfterLand++
			}
			hide := lr.hideLeft > 0
			if hide {
				lr.hideLeft--
			}
			for _, id := range lr.order {
				if s := lr.sessions[id]; lr.visibleLocked(s) && !s.closed && !(hide && lr.landed[id]) {
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
		if s == nil || !lr.visibleLocked(s) {
			http.Error(w, `{"error":"session not found"}`, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session": row(r.PathValue("id"), s)})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		hang, take, hook := lr.closeHang, lr.closeTake, lr.closeHook
		lr.mu.Unlock()
		if hook != nil {
			hook(r.PathValue("id"))
		}
		if take > 0 {
			lr.clock.Advance(take)
		}
		if hang {
			// Like the real router: the close goes on after the client gave
			// up and lands closeDelay later.
			// The server notices a disconnect only once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			lr.mu.Lock()
			lr.closes = append(lr.closes, r.PathValue("id"))
			lr.mu.Unlock()
			<-r.Context().Done()
			lr.mu.Lock()
			if s := lr.sessions[r.PathValue("id")]; s != nil {
				s.closeAt = lr.clock.Now().Add(lr.closeDelay).Unix()
			}
			lr.mu.Unlock()
			return
		}
		lr.mu.Lock()
		defer lr.mu.Unlock()
		id := r.PathValue("id")
		lr.closes = append(lr.closes, id)
		if lr.inflight > 0 {
			lr.closeOverlap++
		}
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
		lr.mu.Lock()
		provs := lr.providersOf(m)
		raw, isRaw := lr.rows[m]
		fault := lr.ratedFault
		lr.mu.Unlock()
		if fault != nil {
			if body := fault(m); body != "" {
				http.Error(w, body, 500)
				return
			}
		}
		if isRaw {
			_ = json.NewEncoder(w).Encode(map[string]any{"bids": raw})
			return
		}
		for i, pv := range provs {
			bids = append(bids, ratedRow(m, bidID(m, pv), pv, float64(10-i)))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bids": bids})
	})
	open := func(w http.ResponseWriter, r *http.Request, model, prov string) string {
		var body struct {
			SessionDuration int `json:"sessionDuration"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		lr.mu.Lock()
		defer lr.mu.Unlock()
		lr.n++
		id := fmt.Sprintf("0x%064x", lr.n)
		now := lr.clock.Now().Unix()
		lr.sessions[id] = &leaseSess{model: model, prov: prov, opened: now, ends: now + int64(body.SessionDuration), durationOpened: body.SessionDuration}
		lr.order = append(lr.order, id)
		lr.opens = append(lr.opens, body.SessionDuration)
		if w != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": id})
		}
		return id
	}
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		m := r.PathValue("id")
		lr.mu.Lock()
		lr.byModel++
		ps := lr.providersOf(m)
		lr.mu.Unlock()
		if len(ps) == 0 { // like the router: no bid, no session
			http.Error(w, `{"error":"no provider accepting session"}`, 500)
			return
		}
		open(w, r, m, ps[0])
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var flag struct {
			RejectExisting bool `json:"rejectExisting"`
		}
		_ = json.Unmarshal(raw, &flag)
		lr.mu.Lock()
		lr.byBid = append(lr.byBid, r.PathValue("id"))
		lr.rejects = append(lr.rejects, flag.RejectExisting)
		hang, take, extra := lr.openHang, lr.openTake, lr.extraMint
		lr.mu.Unlock()
		if flag.RejectExisting {
			if m, _, ok := lr.bidOwner(r.PathValue("id")); ok {
				lr.mu.Lock()
				ex := lr.existingLocked(m)
				if ex != "" {
					lr.refused++
				}
				lr.mu.Unlock()
				if ex != "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]string{"existingSessionID": ex})
					return
				}
			}
		}
		if take > 0 {
			lr.clock.Advance(take)
		}
		if hang {
			// Like the real router: the open goes on after the client gave
			// up; the session mints mintDelay later.
			<-r.Context().Done()
			m, pv, ok := lr.bidOwner(r.PathValue("id"))
			if ok {
				id := open(nil, r, m, pv)
				lr.mu.Lock()
				s := lr.sessions[id]
				s.opened = lr.clock.Now().Add(lr.mintDelay).Unix()
				s.ends = s.opened + int64(s.durationOpened)
				s.visibleAt = s.opened
				lr.mu.Unlock()
			}
			return
		}
		if extra != nil {
			defer func() {
				if extra(r.PathValue("id")) {
					if m, pv, ok := lr.bidOwner(r.PathValue("id")); ok {
						open(nil, r, m, pv)
					}
				}
			}()
		}
		lr.mu.Lock()
		fault, arrivals, gate := lr.openFault, lr.arrivals, lr.gate
		lr.inflight++
		lr.maxInflight = max(lr.maxInflight, lr.inflight)
		lr.mu.Unlock()
		defer func() {
			lr.mu.Lock()
			lr.inflight--
			lr.mu.Unlock()
		}()
		if arrivals != nil {
			arrivals <- r.PathValue("id")
		}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				lr.mu.Lock()
				lr.gateCancels++
				lr.mu.Unlock()
				return
			}
		}
		m, pv, ok := lr.bidOwner(r.PathValue("id"))
		if !ok {
			http.Error(w, `{"error":"provider healthcheck ping failed: dial tcp: connect: connection refused"}`, 500)
			return
		}
		lr.mu.Lock()
		status := lr.openStatus
		lr.mu.Unlock()
		if status != 0 {
			id := open(nil, r, m, pv)
			lr.mu.Lock()
			lr.landed[id] = true
			lr.hideLeft = lr.hideLanded
			lr.mu.Unlock()
			http.Error(w, http.StatusText(status), status)
			return
		}
		if fault != nil {
			if body, landed := fault(r.PathValue("id")); body != "" {
				if landed {
					id := open(nil, r, m, pv)
					lr.mu.Lock()
					lr.landed[id] = true
					lr.hideLeft = lr.hideLanded
					lr.mu.Unlock()
				}
				http.Error(w, body, 500)
				return
			}
		}
		open(w, r, m, pv)
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lr.calls.Add(1)
		if r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/blockchain/bids/") || strings.HasSuffix(r.URL.Path, "/close")) {
			isClose := strings.HasSuffix(r.URL.Path, "/close")
			lr.writeBegin(isClose)
			e := &endOnce{ResponseWriter: w, end: func() { lr.writeEnd(isClose) }}
			defer e.once.Do(e.end)
			w = e
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.SetProvidedRPC(true) // the operator set the router's RPC (blank-RPC tests turn it off)
	p.SetClock(c.Now)
	p.SetHealthyBids(lr.healthy)
	return lr, p
}

// addChain puts a session on chain that the pool did not open (boot adoption).
func (lr *leaseRouter) addChain(id, model string, opened, ends time.Time) {
	lr.addChainOn(id, model, leaseProv1, opened, ends)
}

// addChainOn is addChain on a given provider's bid.
func (lr *leaseRouter) addChainOn(id, model, prov string, opened, ends time.Time) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.sessions[id] = &leaseSess{model: model, prov: prov, opened: opened.Unix(), ends: ends.Unix()}
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

// A pooled session is reused until its usable life ends, however little
// is left; the next call after that opens a new one. Every open is capped
// at the duration max.
func TestS5SessionUsedUntilExpiry(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l1, r1 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	if !r1.Opened || r1.DurationSec != 600 {
		t.Fatalf("first %+v", r1)
	}
	c.Advance(500 * time.Second) // 10 s of usable life left (600 − 90 buffer)
	l2, r2 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1800})
	if r2.Opened || r2.SessionID != r1.SessionID {
		t.Fatalf("want reuse of the pooled session, got %+v", r2)
	}
	l1.Done()
	l2.Done()
	c.Advance(20 * time.Second) // past the usable life
	l3, r3 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1"})
	l3.Done()
	if !r3.Opened || r3.SessionID == r1.SessionID || r3.DurationSec != 600 {
		t.Fatalf("want a new open after expiry, got %+v", r3)
	}
	l4, r4 := mustAcquire(t, p, modelB, AcquireOpts{KeyID: "k1", DurationSec: 86400})
	l4.Done()
	if !r4.Opened || r4.DurationSec != 1800 {
		t.Fatalf("open not capped at the max: %+v", r4)
	}
	if o, _ := lr.snapshot(); len(o) != 3 {
		t.Fatalf("opens %v, want 3", o)
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
	// The last holder's release retires the session; the sweep closes it
	// (never the request path).
	if out := p.Release(modelA, "k3"); out != (ReleaseOutcome{Released: true, Reason: "closing"}) {
		t.Fatalf("sole holder: %+v", out)
	}
	if lr.closedCount(r.SessionID) != 0 {
		t.Fatal("release closed on the request path")
	}
	p.SweepIdle(time.Hour)
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
	p.SweepIdle(time.Hour) // the sweep closes the retired session
	waitUntil(t, "deferred close", func() bool { return lr.closedCount(r.SessionID) == 1 })
}

// Long sessions have no per-key, global or hourly limits: any key (the
// shared ones included) opens sessions up to the duration max.
func TestS5NoLongSessionLimits(t *testing.T) {
	c := newFakeClock()
	_, p := newLeaseRouter(t, c)
	for i, m := range []string{modelA, modelB, modelC, modelD} {
		for _, k := range []string{"k1", "master"} {
			l, r := mustAcquire(t, p, m, AcquireOpts{KeyID: k, DurationSec: 1200})
			if i == 0 && k == "k1" && (!r.Opened || r.DurationSec != 1200) {
				t.Fatalf("long open %+v", r)
			}
			l.Done()
		}
		if out := p.Release(m, "k1"); !out.Released {
			t.Fatalf("release %+v", out)
		}
	}
	for i := 0; i < 5; i++ { // open → release loop on one model
		l, r := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k2", DurationSec: 1200})
		l.Done()
		p.Release(modelA, "k2")
		_ = r
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
// 510 s usable life of a 600 s session) open at most one new session.
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
	if o, _ := lr.snapshot(); len(o) > 2 {
		t.Fatalf("opens %d", len(o))
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

// A failover replacement keeps the opener, is leased for the caller and
// never opens above the duration max.
func TestS5FailoverReplacementCapped(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	l1, r1 := mustAcquire(t, p, modelA, AcquireOpts{KeyID: "k1", DurationSec: 1200})
	l1.Done()
	fr, err := p.Failover(modelA, r1.SessionID, FailoverOpts{KeyID: "k1", DurationSec: 1200, Reopen: true})
	if err != nil || fr.Lease == nil || fr.DurationSec != 1200 {
		t.Fatalf("failover %+v %v", fr, err)
	}
	fr.Lease.Done()
	p.mu.Lock()
	opener := p.sess[fr.SessionID].opener
	p.mu.Unlock()
	if opener != "k1" {
		t.Fatalf("replacement opener=%q", opener)
	}
	p.SetDurationMax(1800)
	l5, r5 := mustAcquire(t, p, modelD, AcquireOpts{KeyID: "k4"})
	l5.Done()
	fr5, err := p.Failover(modelD, r5.SessionID, FailoverOpts{KeyID: "k4", DurationSec: 7200, Reopen: true})
	if err != nil || fr5.DurationSec != 1800 {
		t.Fatalf("capped failover %+v %v", fr5, err)
	}
	fr5.Lease.Done()
	if opens, _ := lr.snapshot(); opens[len(opens)-1] != 1800 {
		t.Fatalf("opened at %d", opens[len(opens)-1])
	}
}

// The providerExclude setting is one address list for every model: opens
// for a model that a listed provider bids on go by bid around it (case and
// spaces do not matter), and a model served only by listed providers gets
// ErrNoEligibleProvider with nothing opened.
func TestProviderExcludeAppliesToEveryModel(t *testing.T) {
	c := newFakeClock()
	lr, p := newLeaseRouter(t, c)
	p.SetProviderExclude([]string{" 0X" + strings.ToUpper(leaseProv1[2:]) + " "})
	if !p.IsProviderExcluded(leaseProv1) || p.IsProviderExcluded(leaseProv2) || p.IsProviderExcluded("") {
		t.Fatalf("lookup: %v %v", p.IsProviderExcluded(leaseProv1), p.IsProviderExcluded(leaseProv2))
	}
	for _, m := range []string{modelA, modelB, modelC} {
		l, r := mustAcquire(t, p, m, AcquireOpts{KeyID: "k1"})
		l.Done()
		lr.mu.Lock()
		prov := lr.sessions[r.SessionID].prov
		lr.mu.Unlock()
		if prov != leaseProv2 {
			t.Fatalf("model %s opened on %s, want %s", short(m), prov, leaseProv2)
		}
	}
	p.SetProviderExclude([]string{leaseProv1, leaseProv2})
	before, _ := lr.snapshot()
	if _, _, err := p.Acquire(modelD, AcquireOpts{KeyID: "k1"}); !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("only listed providers: %v", err)
	}
	if after, _ := lr.snapshot(); len(after) != len(before) {
		t.Fatal("opened a session on a listed provider")
	}
	if got := p.excludedProviders(); len(got) != 2 || !got[leaseProv1] || !got[leaseProv2] {
		t.Fatalf("excluded set %v", got)
	}
}
