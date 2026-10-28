package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// catalogServing returns ConfiguredModels backed by a real catalog fetching body.
func catalogServing(t *testing.T, body string) ConfiguredModels {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(ts.Close)
	cat := catalog.New(ts.URL)
	return ConfiguredFrom(func() ([]string, error) {
		ms, err := cat.List()
		var ids []string
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		return ids, err
	})
}

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
	got := decisions(PlanReconcile(open, now, cfg, true, nil))
	want := map[string]string{
		"a-long":   "adopt:usable",
		"a-short":  "close:duplicate",
		"a-buffer": "close:expired",
		"b-past":   "close:expired",
		"x-live":   "skip:unconfigured",
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
	got := decisions(PlanReconcile(open, now, nil, false, nil))
	if got["x-live"] != "adopt:usable" || got["x-past"] != "close:expired" {
		t.Fatalf("got %v", got)
	}
}

func TestPlanReconcileEmpty(t *testing.T) {
	if len(PlanReconcile(nil, time.Now(), nil, true, nil)) != 0 {
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
	failPage atomic.Int32 // >0: the page at this offset fails (midway)
	closes   atomic.Int32
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
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if lim <= 0 {
			lim = 100
		}
		if fp := int(f.failPage.Load()); fp > 0 && off >= fp {
			http.Error(w, `{"error":"page unavailable"}`, http.StatusBadGateway)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var open []map[string]any
		for _, s := range f.sessions {
			if f.closed[s["Id"].(string)] == 0 {
				open = append(open, s)
			}
		}
		page := []map[string]any{}
		if off < len(open) {
			page = open[off:min(off+lim, len(open))]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": page})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		f.closes.Add(1)
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
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var got ReconcileResult
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{OnReconcile: func(r ReconcileResult) { got = r }})
	if got.Open != 4 || got.Adopted != 1 || got.Closed != 2 || got.Skipped != 1 || got.Failed != 0 || got.Attempts != 1 {
		t.Fatalf("result %+v", got)
	}
	for _, id := range []string{"0xa-dup", "0xb-past"} {
		if f.closed[id] != 1 {
			t.Errorf("%s closed %d times", id, f.closed[id])
		}
	}
	if f.closed["0xa-long"] != 0 || f.closed["0xx-live"] != 0 {
		t.Fatal("adoptable or unconfigured session was closed")
	}
	res, err := acquireDone(p, modelA, 600)
	if err != nil || res.Opened || res.SessionID != "0xa-long" {
		t.Fatalf("ensure %+v %v", res, err)
	}
	if f.opened.Load() != 0 {
		t.Fatalf("opened %d new sessions for a model with an adoptable one", f.opened.Load())
	}
	// Idempotent: a second run adopts the same session and closes nothing.
	r2, err := p.reconcileOnce(configuredAB)
	if err != nil || r2.Closed != 0 || r2.Adopted+r2.Skipped != 2 {
		t.Fatalf("second run %+v %v", r2, err)
	}
}

func TestGateBlocksOpenUntilReconcileDone(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{row("0xa-long", modelA, now.Add(2*time.Hour))}}
	f.listFail.Store(2)
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	done := make(chan AcquireResult, 1)
	go func() {
		r, _ := acquireDone(p, modelA, 600)
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
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var gaveUp atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.StartupReconcile(ctx, configuredAB, ReconcileConfig{
		Backoff: 5 * time.Millisecond, BackoffMax: 10 * time.Millisecond, GateFor: 40 * time.Millisecond,
		RetryEvery: 10 * time.Millisecond, OnGiveUp: func(error) { gaveUp.Store(true) },
	})
	_, err := acquireDone(p, modelA, 600)
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
	r, err := acquireDone(p, modelA, 600)
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
	p := New(router.New(bad.URL, "u", "p"), 600)
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

// liveRows returns n live sessions for models not (or not exactly) in the catalog.
func liveRows(now time.Time, models ...string) []map[string]any {
	var out []map[string]any
	for i, m := range models {
		out = append(out, row("0xlive"+strconv.Itoa(i), m, now.Add(time.Hour)))
	}
	return out
}

func runReconcile(t *testing.T, f *fakeRouter, configured ConfiguredModels) (ReconcileResult, *Pool) {
	t.Helper()
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var got ReconcileResult
	p.StartupReconcile(context.Background(), configured, ReconcileConfig{OnReconcile: func(r ReconcileResult) { got = r }})
	return got, p
}

func TestReconcileEmptyCatalogClosesNoLiveSession(t *testing.T) {
	f := &fakeRouter{closed: map[string]int{}, sessions: liveRows(time.Now(), modelA, modelX)}
	got, _ := runReconcile(t, f, catalogServing(t, `{"models":[]}`))
	if got.Closed != 0 || f.closes.Load() != 0 {
		t.Fatalf("empty catalog closed live sessions: %+v", got)
	}
	if got.Adopted != 2 {
		t.Fatalf("empty catalog is unknown: both usable sessions adopted, got %+v", got)
	}
}

func TestReconcileBracesCatalogClosesNoLiveSession(t *testing.T) {
	f := &fakeRouter{closed: map[string]int{}, sessions: liveRows(time.Now(), modelA, modelX)}
	got, _ := runReconcile(t, f, catalogServing(t, `{}`))
	if got.Closed != 0 || f.closes.Load() != 0 {
		t.Fatalf("{} catalog closed live sessions: %+v", got)
	}
}

func TestReconcileCaseMismatchedCatalogClosesNoLiveSession(t *testing.T) {
	upper := "0xAAAA000000000000000000000000000000000000000000000000000000000000"
	f := &fakeRouter{closed: map[string]int{}, sessions: liveRows(time.Now(), upper, modelX)}
	got, p := runReconcile(t, f, catalogServing(t, `{"models":[{"Name":"a","Id":"`+modelA+`"}]}`))
	if got.Closed != 0 || f.closes.Load() != 0 {
		t.Fatalf("case-mismatched catalog closed live sessions: %+v", got)
	}
	if got.Adopted != 1 || got.Skipped != 1 {
		t.Fatalf("want upper-case session adopted (case-insensitive match), unconfigured skipped: %+v", got)
	}
	if len(p.Snapshot()) != 1 {
		t.Fatalf("snapshot %+v", p.Snapshot())
	}
	// The catalog id (lowercase) must find the adopted session: no new open.
	res, err := acquireDone(p, modelA, 600)
	if err != nil || res.Opened || res.SessionID != "0xlive0" || f.opened.Load() != 0 {
		t.Fatalf("case-mismatched adopt not reused: %+v err=%v opened=%d", res, err, f.opened.Load())
	}
	// A second pass sees one session for the model: nothing to close.
	r2, err := p.reconcileOnce(catalogServing(t, `{"models":[{"Name":"a","Id":"`+modelA+`"}]}`))
	if err != nil || r2.Closed != 0 || f.closes.Load() != 0 {
		t.Fatalf("second pass closed a case-only match: %+v %v", r2, err)
	}
}

func TestRescanKeysByLowercaseModel(t *testing.T) {
	upper := "0xAAAA000000000000000000000000000000000000000000000000000000000000"
	f := &fakeRouter{closed: map[string]int{}, sessions: liveRows(time.Now(), upper)}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	res, err := acquireDone(p, modelA, 600)
	if err != nil || res.Opened || f.opened.Load() != 0 {
		t.Fatalf("rescan adopt not found by lowercase id: %+v %v opened=%d", res, err, f.opened.Load())
	}
	p.Invalidate(upper) // invalidate by either case
	if len(p.Snapshot()) != 0 {
		t.Fatalf("invalidate missed: %+v", p.Snapshot())
	}
}

func TestRescanAuditsExpiredClose(t *testing.T) {
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{row("0xexp-rescan-0000", modelA, time.Now().Add(-time.Minute))}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	var evs []ReconcileEvent
	p.SetOnRescanEvent(func(ev ReconcileEvent) { evs = append(evs, ev) })
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "session_reconcile_close" || evs[0].Reason != "expired" ||
		evs[0].Source != "rescan" || evs[0].Result != "ok" || evs[0].Session != "0xexp-resc" {
		t.Fatalf("rescan events %+v", evs)
	}
}

func TestReconcileSummaryCarriesCatalogSize(t *testing.T) {
	f := &fakeRouter{closed: map[string]int{}}
	got, _ := runReconcile(t, f, configuredAB)
	if got.Catalog != 2 {
		t.Fatalf("catalog size %d, want 2", got.Catalog)
	}
	got, _ = runReconcile(t, &fakeRouter{closed: map[string]int{}}, catalogServing(t, `{}`))
	if got.Catalog != -1 {
		t.Fatalf("unknown catalog size %d, want -1", got.Catalog)
	}
}

func TestShutdownCloseAllWithinTimesOut(t *testing.T) {
	now := time.Now()
	hang := make(chan struct{})
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xa-fast", modelA, now.Add(time.Hour)), row("0xb-slow", modelB, now.Add(time.Hour)),
	}}
	inner := f.server(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "0xb-slow" {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		proxyTo(t, inner.URL, w, r)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyTo(t, inner.URL, w, r) }))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(hang) }) // runs before ts.Close
	p := New(router.New(ts.URL, "u", "p"), 600)
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := p.CloseAllWithin(start.Add(5*time.Second), 200*time.Millisecond)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %s", el)
	}
	if r.Closed != 1 || r.Left != 1 || f.closed["0xa-fast"] != 1 {
		t.Fatalf("result %+v closed=%v", r, f.closed)
	}
	// Past the deadline nothing is attempted; everything is left.
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	r2 := p.CloseAllWithin(time.Now().Add(-time.Second), time.Second)
	if r2.Attempted != 0 || r2.Left != 1 {
		t.Fatalf("past deadline: %+v", r2)
	}
}

func TestReconcileUnconfiguredNeverClosed(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xx-1", modelX, now.Add(time.Hour)), row("0xx-2", modelX, now.Add(2*time.Hour)),
	}}
	got, _ := runReconcile(t, f, configuredAB)
	if f.closes.Load() != 0 || got.Skipped != 2 || got.Adopted != 0 {
		t.Fatalf("unconfigured sessions must be skipped, never closed: %+v closes=%d", got, f.closes.Load())
	}
}

func TestReconcileCloseCapDefersExcess(t *testing.T) {
	now := time.Now()
	var rows []map[string]any
	for i := 0; i < MaxBootCloses+7; i++ {
		rows = append(rows, row("0xexp"+strconv.Itoa(i), modelA, now.Add(-time.Minute)))
	}
	f := &fakeRouter{closed: map[string]int{}, sessions: rows}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var got ReconcileResult
	var caps []ReconcileEvent
	closeEv := 0
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{
		OnReconcile: func(r ReconcileResult) { got = r },
		OnEvent: func(ev ReconcileEvent) {
			switch ev.Kind {
			case "session_reconcile_cap":
				caps = append(caps, ev)
			case "session_reconcile_close":
				closeEv++
				if len(ev.Session) > 10 {
					t.Errorf("close event carries a long id %q", ev.Session)
				}
			}
		},
	})
	if got.Closed != MaxBootCloses || got.Deferred != 7 || int(f.closes.Load()) != MaxBootCloses {
		t.Fatalf("cap: %+v closes=%d", got, f.closes.Load())
	}
	if len(caps) != 1 || caps[0].Count != 7 || closeEv != MaxBootCloses {
		t.Fatalf("cap events %+v, close events %d", caps, closeEv)
	}
	if p.ReconcileStatus().State != "partial" || p.ReconcileStatus().Deferred != 7 {
		t.Fatalf("state %+v", p.ReconcileStatus())
	}
}

func TestReconcileCloseCapExpiredFirst(t *testing.T) {
	now := time.Now()
	var rows []map[string]any
	for i := 0; i < MaxBootCloses; i++ {
		rows = append(rows, row("0xdup"+strconv.Itoa(i), modelA, now.Add(time.Duration(10+i)*time.Minute)))
	}
	rows = append(rows, row("0xkeep", modelA, now.Add(5*time.Hour)), row("0xexp", modelB, now.Add(-time.Minute)))
	f := &fakeRouter{closed: map[string]int{}, sessions: rows}
	got, _ := runReconcile(t, f, configuredAB)
	if f.closed["0xexp"] != 1 || f.closed["0xkeep"] != 0 || got.Deferred != 1 {
		t.Fatalf("expired must be closed first: %+v", got)
	}
}

func TestReconcilePartialListFailureClosesNothing(t *testing.T) {
	now := time.Now()
	var rows []map[string]any
	for i := 0; i < 150; i++ { // two pages of 100; the second page fails
		rows = append(rows, row("0xexp"+strconv.Itoa(i), modelA, now.Add(-time.Minute)))
	}
	f := &fakeRouter{closed: map[string]int{}, sessions: rows}
	f.failPage.Store(100)
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gaveUp atomic.Bool
	go p.StartupReconcile(ctx, configuredAB, ReconcileConfig{
		Backoff: 5 * time.Millisecond, BackoffMax: 10 * time.Millisecond, GateFor: 40 * time.Millisecond,
		RetryEvery: 10 * time.Millisecond, OnGiveUp: func(error) { gaveUp.Store(true) },
	})
	if _, err := acquireDone(p, modelA, 600); err != ErrSessionStateUnknown {
		t.Fatalf("err = %v, want ErrSessionStateUnknown", err)
	}
	time.Sleep(50 * time.Millisecond)
	if f.closes.Load() != 0 || f.opened.Load() != 0 {
		t.Fatalf("partial list: closes=%d opens=%d, want 0/0", f.closes.Load(), f.opened.Load())
	}
	st := p.ReconcileStatus()
	if !gaveUp.Load() || !st.OpensHeld || (st.State != "deferred" && st.State != "retrying") {
		t.Fatalf("state %+v", st)
	}
}

func TestReconcileUnverifiableRowClosesNothing(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xexp", modelA, now.Add(-time.Minute)),
		{"Id": "", "ModelAgentId": modelA, "Stake": "1", "EndsAt": now.Add(time.Hour).Unix(), "ClosedAt": 0},
	}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	if _, err := p.reconcileOnce(configuredAB); err == nil {
		t.Fatal("want error for a row without an id")
	}
	if f.closes.Load() != 0 || len(p.Snapshot()) != 0 {
		t.Fatalf("unverifiable list acted on: closes=%d snap=%v", f.closes.Load(), p.Snapshot())
	}
}

func TestReconcileGateReleasedBeforeSlowCloses(t *testing.T) {
	now := time.Now()
	release := make(chan struct{})
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{row("0xexp", modelB, now.Add(-time.Minute))}}
	inner := f.server(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		<-release
		proxyTo(t, inner.URL, w, r)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyTo(t, inner.URL, w, r) }))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	done := make(chan struct{})
	go func() {
		p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{})
		close(done)
	}()
	res := make(chan error, 1)
	go func() { _, err := acquireDone(p, modelA, 600); res <- err }()
	select {
	case err := <-res:
		if err != nil || f.opened.Load() != 1 {
			t.Fatalf("open during slow close: err=%v opened=%d", err, f.opened.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("open blocked by a slow close")
	}
	if st := p.ReconcileStatus().State; st != "closing" {
		t.Fatalf("state %q, want closing", st)
	}
	close(release)
	<-done
	if f.closed["0xexp"] != 1 || p.ReconcileStatus().State != "ok" {
		t.Fatalf("close not finished: %v %+v", f.closed, p.ReconcileStatus())
	}
}

func TestLateRecoveryClosesExpiredOnly(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xa-long", modelA, now.Add(2*time.Hour)), row("0xa-dup", modelA, now.Add(time.Hour)),
		row("0xb-past", modelB, now.Add(-time.Minute)),
	}}
	f.listFail.Store(3)
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var got ReconcileResult
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{
		Backoff: 5 * time.Millisecond, GateFor: 8 * time.Millisecond, RetryEvery: 5 * time.Millisecond,
		OnReconcile: func(r ReconcileResult) { got = r },
	})
	if !got.Late || f.closed["0xb-past"] != 1 || f.closed["0xa-dup"] != 0 || got.Closed != 1 {
		t.Fatalf("late recovery must close expired only: %+v %v", got, f.closed)
	}
}

func TestReconcileAuditsEveryAdoptAndClose(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		row("0xa-long-session-id-0000", modelA, now.Add(2*time.Hour)),
		row("0xa-dup-session-id-00000", modelA, now.Add(20*time.Minute)),
		row("0xb-past-session-id-0000", modelB, now.Add(-time.Minute)),
	}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.Gate()
	var evs []ReconcileEvent
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{OnEvent: func(ev ReconcileEvent) { evs = append(evs, ev) }})
	kinds := map[string]int{}
	reasons := map[string]bool{}
	for _, ev := range evs {
		kinds[ev.Kind]++
		reasons[ev.Reason] = true
		if len(ev.Session) != 10 {
			t.Errorf("session prefix %q is not 10 chars", ev.Session)
		}
	}
	if kinds["session_reconcile_adopt"] != 1 || kinds["session_reconcile_close"] != 2 || !reasons["expired"] || !reasons["duplicate"] {
		t.Fatalf("events %+v", evs)
	}
}

const (
	recProvEx = "0x5555555555555555555555555555555555555555"
	recProvOK = "0x6666666666666666666666666666666666666666"
)

func provRow(id, model, prov string, ends time.Time) map[string]any {
	r := row(id, model, ends)
	r["Provider"] = prov
	return r
}

// A usable session on an excluded provider is closed with its own reason
// and never adopted, and does not take part in the model's adopt choice:
// a shorter session on another provider is adopted instead of being closed
// as a duplicate. An expired one is closed as expired.
func TestPlanReconcileExcludedProvider(t *testing.T) {
	now := time.Now()
	withProv := func(s router.Session, prov string) router.Session { s.Provider = prov; return s }
	open := []router.Session{
		withProv(sess("a-ex", modelA, now.Add(3*time.Hour)), recProvEx), // longest, excluded
		withProv(sess("a-ok", modelA, now.Add(time.Hour)), recProvOK),
		withProv(sess("a-ok-short", modelA, now.Add(30*time.Minute)), recProvOK),
		withProv(sess("b-ex", modelB, now.Add(2*time.Hour)), "0x"+strings.ToUpper(recProvEx[2:])),
		withProv(sess("a-ex-past", modelA, now.Add(-time.Minute)), recProvEx),
	}
	cfg := map[string]bool{modelA: true, modelB: true}
	got := decisions(PlanReconcile(open, now, cfg, true, map[string]bool{recProvEx: true}))
	want := map[string]string{
		"a-ex":       "close:provider_excluded",
		"a-ok":       "adopt:usable",
		"a-ok-short": "close:duplicate",
		"b-ex":       "close:provider_excluded",
		"a-ex-past":  "close:expired",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q want %q", id, got[id], w)
		}
	}
	// Without exclusions the same sessions plan as before.
	if got := decisions(PlanReconcile(open, now, cfg, true, nil)); got["a-ex"] != "adopt:usable" || got["a-ok"] != "close:duplicate" {
		t.Fatalf("no exclusions: %v", got)
	}
}

// A model hidden by the catalog because all its providers are excluded is
// not in the configured list; its sessions still close as provider_excluded
// (an expired one once, as expired), while an unconfigured model on a
// non-excluded provider is still skipped.
func TestPlanReconcileExcludedUnconfiguredModel(t *testing.T) {
	now := time.Now()
	withProv := func(s router.Session, prov string) router.Session { s.Provider = prov; return s }
	open := []router.Session{
		withProv(sess("b-ex", modelB, now.Add(2*time.Hour)), recProvEx),
		withProv(sess("b-ex-past", modelB, now.Add(-time.Minute)), recProvEx),
		withProv(sess("b-ok", modelB, now.Add(time.Hour)), recProvOK),
		withProv(sess("a-ok", modelA, now.Add(time.Hour)), recProvOK),
	}
	cfg := map[string]bool{modelA: true} // modelB not configured
	plan := PlanReconcile(open, now, cfg, true, map[string]bool{recProvEx: true})
	got := decisions(plan)
	want := map[string]string{
		"b-ex":      "close:provider_excluded",
		"b-ex-past": "close:expired",
		"b-ok":      "skip:unconfigured",
		"a-ok":      "adopt:usable",
	}
	if len(got) != len(want) || len(plan) != len(open) {
		t.Fatalf("plan: %v", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q want %q", id, got[id], w)
		}
	}
}

// Boot reconcile with providerExclude loaded: the excluded provider's open
// sessions are closed on-chain (reason provider_excluded, audited), the
// non-excluded duplicate is adopted and served, and the model served only
// by an excluded session has no pooled session.
func TestReconcileExcludedProviderClosed(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		provRow("0xa-ex", modelA, recProvEx, now.Add(3*time.Hour)),
		provRow("0xa-ok", modelA, recProvOK, now.Add(time.Hour)),
		provRow("0xb-ex", modelB, recProvEx, now.Add(2*time.Hour)),
	}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.SetProviderExclude([]string{recProvEx})
	p.Gate()
	var got ReconcileResult
	var mu sync.Mutex
	closeReasons := map[string]string{}
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{
		OnReconcile: func(r ReconcileResult) { got = r },
		OnEvent: func(ev ReconcileEvent) {
			if ev.Kind == "session_reconcile_close" {
				mu.Lock()
				closeReasons[ev.Session] = ev.Reason + "/" + ev.Result
				mu.Unlock()
			}
		},
	})
	if got.Open != 3 || got.Adopted != 1 || got.Closed != 2 || got.Skipped != 0 || got.Failed != 0 {
		t.Fatalf("result %+v", got)
	}
	f.mu.Lock()
	closed := map[string]int{"0xa-ex": f.closed["0xa-ex"], "0xb-ex": f.closed["0xb-ex"], "0xa-ok": f.closed["0xa-ok"]}
	f.mu.Unlock()
	if closed["0xa-ex"] != 1 || closed["0xb-ex"] != 1 || closed["0xa-ok"] != 0 {
		t.Fatalf("closes %v", closed)
	}
	mu.Lock()
	for _, id := range []string{"0xa-ex", "0xb-ex"} {
		if closeReasons[short(id)] != "provider_excluded/ok" {
			t.Errorf("%s: close event %q", id, closeReasons[short(id)])
		}
	}
	mu.Unlock()
	p.mu.Lock()
	a, aOK := p.entries[normModel(modelA)]
	_, bOK := p.entries[normModel(modelB)]
	p.mu.Unlock()
	if !aOK || a.sessionID != "0xa-ok" || bOK {
		t.Fatalf("pooled: modelA %+v (%v), modelB pooled=%v", a, aOK, bOK)
	}
	res, err := acquireDone(p, modelA, 600)
	if err != nil || res.Opened || res.SessionID != "0xa-ok" {
		t.Fatalf("acquire %+v %v", res, err)
	}
	if f.opened.Load() != 0 {
		t.Fatalf("opened %d sessions", f.opened.Load())
	}
}

// Boot reconcile for a model this node does not serve (not in the
// configured list) whose session is on an excluded provider: closed
// on-chain with provider_excluded, not skipped.
func TestReconcileExcludedUnconfiguredModelClosed(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		provRow("0xb-ex", modelB, recProvEx, now.Add(2*time.Hour)),
		provRow("0xa-ok", modelA, recProvOK, now.Add(time.Hour)),
	}}
	ts := f.server(t)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.SetProviderExclude([]string{recProvEx})
	p.Gate()
	configuredA := func() (map[string]bool, bool) { return map[string]bool{modelA: true}, true }
	var got ReconcileResult
	var mu sync.Mutex
	closeReasons := map[string]string{}
	p.StartupReconcile(context.Background(), configuredA, ReconcileConfig{
		OnReconcile: func(r ReconcileResult) { got = r },
		OnEvent: func(ev ReconcileEvent) {
			if ev.Kind == "session_reconcile_close" {
				mu.Lock()
				closeReasons[ev.Session] = ev.Reason + "/" + ev.Result
				mu.Unlock()
			}
		},
	})
	if got.Open != 2 || got.Adopted != 1 || got.Closed != 1 || got.Skipped != 0 || got.Failed != 0 {
		t.Fatalf("result %+v", got)
	}
	f.mu.Lock()
	nb, na := f.closed["0xb-ex"], f.closed["0xa-ok"]
	f.mu.Unlock()
	if nb != 1 || na != 0 {
		t.Fatalf("closes b-ex=%d a-ok=%d", nb, na)
	}
	mu.Lock()
	if r := closeReasons[short("0xb-ex")]; r != "provider_excluded/ok" {
		t.Errorf("close event %q", r)
	}
	mu.Unlock()
	p.mu.Lock()
	_, bOK := p.entries[normModel(modelB)]
	p.mu.Unlock()
	if bOK {
		t.Fatal("modelB pooled")
	}
}

// A failed close of an excluded provider's session is counted and logged,
// the session is still not adopted, and boot goes on: the gate is released
// and the model's non-excluded session is served.
func TestReconcileExcludedCloseFailureDoesNotBlock(t *testing.T) {
	now := time.Now()
	f := &fakeRouter{closed: map[string]int{}, sessions: []map[string]any{
		provRow("0xa-ex", modelA, recProvEx, now.Add(3*time.Hour)),
		provRow("0xa-ok", modelA, recProvOK, now.Add(time.Hour)),
	}}
	ts := f.server(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyTo(t, ts.URL, w, r) }))
	bad := httptest.NewServer(mux)
	t.Cleanup(bad.Close)
	p := New(router.New(bad.URL, "u", "p"), 600)
	p.SetProviderExclude([]string{recProvEx})
	p.Gate()
	var got ReconcileResult
	p.StartupReconcile(context.Background(), configuredAB, ReconcileConfig{OnReconcile: func(r ReconcileResult) { got = r }})
	if got.Adopted != 1 || got.Failed != 1 || got.Closed != 0 {
		t.Fatalf("result %+v", got)
	}
	if p.gateClosed() {
		t.Fatal("gate still closed after a failed close")
	}
	res, err := acquireDone(p, modelA, 600)
	if err != nil || res.Opened || res.SessionID != "0xa-ok" {
		t.Fatalf("acquire %+v %v", res, err)
	}
}
