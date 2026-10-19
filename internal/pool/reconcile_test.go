package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const (
	modelA = "0xaaaa000000000000000000000000000000000000000000000000000000000000"
	modelB = "0xbbbb000000000000000000000000000000000000000000000000000000000000"
	modelX = "0xeeee000000000000000000000000000000000000000000000000000000000000"
)

func sess(id, model string, ends time.Time) router.Session {
	return router.Session{ID: id, ModelAgentID: model, EndsAt: ends.Unix()}
}

func decisions(ds []ReconcileDecision) map[string]string {
	out := map[string]string{}
	for _, d := range ds {
		out[d.Session.ID] = d.Action + ":" + d.Reason
	}
	return out
}

func TestPlanReconcileDecisions(t *testing.T) {
	now := time.Now()
	open := []router.Session{
		sess("a-long", modelA, now.Add(2*time.Hour)),
		sess("a-short", modelA, now.Add(30*time.Minute)),            // duplicate of a-long
		sess("a-buffer", modelA, now.Add(expiryBuffer-time.Second)), // inside buffer
		sess("b-past", modelB, now.Add(-time.Minute)),
		sess("x-live", modelX, now.Add(time.Hour)), // model no longer configured
	}
	cfg := map[string]bool{modelA: true, modelB: true}
	got := decisions(PlanReconcile(open, now, cfg, true))
	want := map[string]string{
		"a-long":   "adopt:usable",
		"a-short":  "close:duplicate",
		"a-buffer": "close:expired",
		"b-past":   "close:expired",
		"x-live":   "close:unconfigured",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q want %q", id, got[id], w)
		}
	}
}

func TestPlanReconcileUnknownCatalogNeverClosesForModel(t *testing.T) {
	now := time.Now()
	open := []router.Session{sess("x-live", modelX, now.Add(time.Hour)), sess("x-past", modelX, now.Add(-time.Hour))}
	got := decisions(PlanReconcile(open, now, nil, false))
	if got["x-live"] != "adopt:unknown-catalog" || got["x-past"] != "close:expired" {
		t.Fatalf("got %v", got)
	}
}

func TestPlanReconcileEmpty(t *testing.T) {
	if len(PlanReconcile(nil, time.Now(), nil, true)) != 0 {
		t.Fatal("expected no decisions")
	}
}

type fakeRouter struct {
	mu       sync.Mutex
	sessions []map[string]any
	closed   map[string]int
	opened   atomic.Int32
	listFail atomic.Int32 // fail this many list calls
	lists    atomic.Int32
}

func (f *fakeRouter) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		f.lists.Add(1)
		if f.listFail.Load() > 0 {
			f.listFail.Add(-1)
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var open []map[string]any
		if r.URL.Query().Get("offset") == "" || r.URL.Query().Get("offset") == "0" {
			for _, s := range f.sessions {
				if f.closed[s["Id"].(string)] == 0 {
					open = append(open, s)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": open})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.closed[r.PathValue("id")]++
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		f.opened.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": "0xnew"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func row(id, model string, ends time.Time) map[string]any {
	return map[string]any{"Id": id, "ModelAgentId": model, "Stake": "1000", "EndsAt": ends.Unix(), "ClosedAt": 0}
}

func configuredAB() (map[string]bool, bool) { return map[string]bool{modelA: true, modelB: true}, true }

func TestStartupReconcileAdoptsAndClosesThenNoDoubleOpen(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xa-long", modelA, now.Add(2*time.Hour)),
		row("0xa-dup", modelA, now.Add(20*time.Minute)),
		row("0xb-past", modelB, now.Add(-time.Minute)),
		row("0xx-live", modelX, now.Add(time.Hour)),
	}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600, true)
	p.Gate()
	var got ReconcileResult
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{OnReconcile: func(r ReconcileResult) { got = r }})
	if got.Open != 4 || got.Adopted != 1 || got.Closed != 3 || got.Failed != 0 || got.Attempts != 1 {
		t.Fatalf("result %+v", got)
	}
	for _, id := range []string{"0xa-dup", "0xb-past", "0xx-live"} {
		if f.closed[id] != 1 {
			t.Errorf("%s closed %d times", id, f.closed[id])
		}
	}
	if f.closed["0xa-long"] != 0 {
		t.Fatal("adoptable session was closed")
	}
	res, err := p.EnsureDuration(modelA, 600)
	if err != nil || res.Opened || res.SessionID != "0xa-long" {
		t.Fatalf("ensure %+v %v", res, err)
	}
	if f.opened.Load() != 0 {
		t.Fatalf("opened %d new sessions for a model with an adoptable one", f.opened.Load())
	}
	// Idempotent: a second run adopts the same session and closes nothing.
	r2, err := p.reconcileOnce(configuredAB)
	if err != nil || r2.Closed != 0 || r2.Adopted+r2.Skipped != 1 {
		t.Fatalf("second run %+v %v", r2, err)
	}
}

func TestGateBlocksOpenUntilReconcileDone(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{row("0xa-long", modelA, now.Add(2*time.Hour))}}
	f.listFail.Store(2)
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600, true)
	p.Gate()
	done := make(chan EnsureResult, 1)
	go func() {
		r, _ := p.EnsureDuration(modelA, 600)
		done <- r
	}()
	select {
	case <-done:
		t.Fatal("open proceeded before reconcile")
	case <-time.After(50 * time.Millisecond):
	}
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{Backoff: 5 * time.Millisecond, GateFor: time.Second})
	r := <-done
	if r.Opened || r.SessionID != "0xa-long" || f.opened.Load() != 0 {
		t.Fatalf("got %+v opened=%d", r, f.opened.Load())
	}
}

func TestListFailureRefusesOpenUntilStateKnown(t *testing.T) {
	f := &fakeRouter{closed: map[string]int{}}
	f.listFail.Store(1 << 20)
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600, true)
	p.Gate()
	var gaveUp atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.StartupReconcile(ctx, configuredAB, ReconcileConfig{
		Backoff: 5 * time.Millisecond, BackoffMax: 10 * time.Millisecond, GateFor: 40 * time.Millisecond,
		RetryEvery: 10 * time.Millisecond, OnGiveUp: func(error) { gaveUp.Store(true) },
	})
	_, err := p.EnsureDuration(modelA, 600)
	if err != ErrSessionStateUnknown {
		t.Fatalf("err = %v, want ErrSessionStateUnknown", err)
	}
	if !gaveUp.Load() || f.opened.Load() != 0 {
		t.Fatalf("gaveUp=%v opened=%d", gaveUp.Load(), f.opened.Load())
	}
	// Router recovers: the background retry succeeds and opens proceed.
	f.listFail.Store(0)
	deadline := time.Now().Add(2 * time.Second)
	for !p.stateKnown.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r, err := p.EnsureDuration(modelA, 600)
	if err != nil || !r.Opened || f.opened.Load() != 1 {
		t.Fatalf("after recovery %+v %v opened=%d", r, err, f.opened.Load())
	}
}

func TestCloseFailureCountedAndRetrySafe(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{row("0xb-past", modelB, now.Add(-time.Minute))}}
	ts := f.server(t)
	// Router that fails closes.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(t, ts.URL, w, r)
	}))
	bad := httptest.NewServer(mux)
	t.Cleanup(bad.Close)
	p := New(router.New(bad.URL, "u", "p"), 600, true)
	r, err := p.reconcileOnce(configuredAB)
	if err != nil || r.Failed != 1 || r.Closed != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func proxyTo(t *testing.T, base string, w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequest(r.Method, base+r.URL.RequestURI(), r.Body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	_, _ = w.Write(buf[:n])
}
