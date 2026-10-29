package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

func TestClassifyForwardError(t *testing.T) {
	se := func(code int, body string) error {
		return &router.StatusError{Method: "POST", Path: "/v1/chat/completions", Code: code, Body: body}
	}
	pme := func(msg string) string {
		return `{"providerModelError":{"error":{"message":"` + msg + `"}},"statusCode":0}`
	}
	cases := []struct {
		name string
		err  error
		want forwardDecision
	}{
		{"402 provider backend out of credit", se(402, pme("Insufficient balance")), decFailover},
		{"400 provider error, no pattern", se(400, pme("invalid parameter top_k")), decRelay},
		{"400 provider error, rate limit", se(400, pme("rate limit reached")), decFailover},
		{"400 provider error, statusCode 503 in body", se(400, `{"providerModelError":{"error":"x"},"statusCode": 503}`), decFailover},
		{"413 provider error, overloaded", se(413, pme("server overloaded")), decFailover},
		{"422 plain", se(422, `{"error":"bad input"}`), decRelay},
		{"422 impaired words but no provider error", se(422, `{"error":"rate limit"}`), decRelay},
		{"401 provider error", se(401, pme("invalid api key")), decFailover},
		{"403 provider error", se(403, pme("forbidden")), decFailover},
		{"404 provider error", se(404, pme("model not found")), decFailover},
		{"404 without provider error", se(404, `{"error":"route not found"}`), decRelay},
		{"429", se(429, `{"error":"too many"}`), decFailover},
		{"500", se(500, `{"error":"provider failed"}`), decFailover},
		{"502", se(502, "bad gateway"), decFailover},
		{"503", se(503, "unavailable"), decFailover},
		{"400 session expired", se(400, `{"error":"session expired"}`), decSessionDead},
		{"404 session not found", se(404, "session not found"), decSessionDead},
		{"TEE attestation", se(500, `{"error":"p-node tee attestation failed: quote"}`), decRelay},
		{"TEE verification", se(500, `{"error":"llm tee verification failed"}`), decRelay},
		{"queue cancelled", se(500, `{"error":"request cancelled while waiting in queue"}`), decRelay},
		{"transport", fmt.Errorf("POST /v1/chat/completions: %w", errors.New("connection refused")), decTransport},
	}
	for _, tc := range cases {
		if got := classifyForwardError(tc.err); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestRelayRouterError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
		wantType string
		wantPME  bool
	}{
		{"provider 400 reshaped", &router.StatusError{Code: 400, Body: `{"providerModelError":{"error":{"message":"top_k must be > 0"}},"statusCode":400}`}, 400, "top_k must be > 0", "provider_error", true},
		{"openai-shaped kept", &router.StatusError{Code: 422, Body: `{"error":{"message":"bad","type":"invalid_request_error"}}`}, 422, "bad", "invalid_request_error", false},
		{"plain router error", &router.StatusError{Code: 409, Body: `{"error":"conflict"}`}, 409, "conflict", "invalid_request_error", false},
		{"router credential is not the client's", &router.StatusError{Code: 401, Body: `{"error":"unauthorized"}`}, 502, "router rejected", "invalid_request_error", false},
		{"transport", errors.New("POST http://proxy-router:8082/v1/chat/completions: dial tcp: refused"), 502, "inference failed", "invalid_request_error", false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		relayRouterError(rec, tc.err)
		if rec.Code != tc.wantCode {
			t.Errorf("%s: code %d want %d", tc.name, rec.Code, tc.wantCode)
		}
		var out struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
			PME json.RawMessage `json:"providerModelError"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: body not JSON: %s", tc.name, rec.Body.String())
		}
		if strings.Contains(out.Error.Message, "proxy-router") || strings.Contains(out.Error.Message, "dial") {
			t.Errorf("%s: router detail reached the client: %q", tc.name, out.Error.Message)
		}
		if !strings.Contains(out.Error.Message, tc.wantMsg) || out.Error.Type != tc.wantType || (len(out.PME) > 0) != tc.wantPME {
			t.Errorf("%s: body %s", tc.name, rec.Body.String())
		}
	}
}

// fakeProvider is how one provider answers a prompt.
type fakeProvider func(n int, w http.ResponseWriter)

func pme402(n int, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(402)
	_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"Insufficient balance"}},"statusCode":402}`))
}

func ok200(n int, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
}

type openRec struct {
	bid, provider string
	body          map[string]any
}

type fakeSess struct {
	provider, bid string
	ends          int64
	closed        bool
}

// fakeMarket is a stateful proxy-router with several providers for one
// model: rated bids (real shape), opens by bid, closes, session detail and
// prompts. It also serves the gateway bids feed (/gateway_bids.json): every
// provider's bid is healthy unless listed in unhealthy. Its by-model open
// only counts (Uplink never uses it; asserted 0 at cleanup).
type fakeMarket struct {
	t           *testing.T
	mu          sync.Mutex
	providers   []string
	behave      map[string]fakeProvider
	behaveReq   map[string]func(n int, w http.ResponseWriter, r *http.Request) // sees the request (its context)
	prompts     map[string]int
	sessions    map[string]*fakeSess
	opens       []openRec
	closes      []string
	closeStatus int
	closeBody   string // router error text on a failed close
	next        int
	bidsFail    bool            // rated bids answer 500
	unhealthy   map[string]bool // provider whose bid the bids feed marks degraded
	byModel     int             // by-model opens (must stay 0)
	openFail    map[string]bool // provider whose opens answer 500
	// openErrBody, when set, answers every open with status 500 and this
	// body (the router's shape for a chain RPC fault); no session opens.
	openErrBody string
	// ratedErrBody, when set, answers rated bids with status 500 and this body.
	ratedErrBody string
	// openLandBody, when set, opens the session on chain and still
	// answers status 500 with this body (tx sent, receipt wait failed).
	openLandBody string
	// openArrived, when set, gets a signal when an open reaches the
	// market; openGate, when set, then holds the open until it is closed.
	openArrived chan struct{}
	openGate    chan struct{}
	// sessionLookup: "" normal; "fail" session detail answers 500;
	// "empty" session detail has no provider.
	sessionLookup string
}

func newFakeMarket(t *testing.T, providers ...string) *fakeMarket {
	return &fakeMarket{t: t, providers: providers, behave: map[string]fakeProvider{},
		behaveReq: map[string]func(int, http.ResponseWriter, *http.Request){}, prompts: map[string]int{}, sessions: map[string]*fakeSess{},
		unhealthy: map[string]bool{}}
}

// bidOf is the fake bid id of a provider (0x + 64 hex, like the router's).
func bidOf(p string) string { return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(p, "0x") }

func (m *fakeMarket) open(w http.ResponseWriter, r *http.Request, byBid string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if v, ok := body["failover"]; ok && v != false {
		m.t.Errorf("open body carries failover=%v", v)
	}
	if _, ok := body["failover"]; byBid != "" && !ok {
		m.t.Errorf("open-by-bid body has no explicit failover=false")
	}
	if v := body["directPayment"]; v != false {
		m.t.Errorf("open body directPayment=%v", v)
	}
	for k := range body {
		if k != "sessionDuration" && k != "failover" && k != "directPayment" {
			m.t.Errorf("open body carries %q (staking-only body; avoidance goes by bid)", k)
		}
	}
	m.mu.Lock()
	arrived, gate := m.openArrived, m.openGate
	m.mu.Unlock()
	if arrived != nil {
		arrived <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prov := ""
	for _, p := range m.providers {
		// An open by model takes the best rated provider, whatever Uplink
		// avoids: avoidance is only ever enforced by the bid walk.
		if byBid == "" || bidOf(p) == byBid {
			prov = p
			break
		}
	}
	m.opens = append(m.opens, openRec{bid: byBid, provider: prov, body: body})
	if m.openErrBody != "" {
		http.Error(w, m.openErrBody, http.StatusInternalServerError)
		return
	}
	if prov == "" || m.openFail[prov] {
		http.Error(w, `{"error":"no provider accepting session"}`, http.StatusInternalServerError)
		return
	}
	m.next++
	id := fmt.Sprintf("0xsess%02d%s", m.next, strings.TrimPrefix(prov, "0x"))
	m.sessions[id] = &fakeSess{provider: prov, bid: bidOf(prov), ends: time.Now().Add(time.Hour).Unix() + int64(m.next)}
	if m.openLandBody != "" {
		http.Error(w, m.openLandBody, http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": id})
}

func (m *fakeMarket) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.byModel++
		m.mu.Unlock()
		http.Error(w, `{"error":"by-model open not expected"}`, http.StatusInternalServerError)
	})
	m.t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.byModel != 0 {
			m.t.Errorf("by-model opens = %d, want 0", m.byModel)
		}
	})
	mux.HandleFunc("GET /gateway_bids.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bidsFeed(time.Now().Unix(), m.feedRows()...))
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) { m.open(w, r, r.PathValue("id")) })
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := r.PathValue("id")
		m.closes = append(m.closes, id)
		if m.closeStatus != 0 {
			body := `{"error":"close failed"}`
			if m.closeBody != "" {
				body = m.closeBody
			}
			http.Error(w, body, m.closeStatus)
			return
		}
		if s := m.sessions[id]; s != nil {
			s.closed = true
		}
		_, _ = w.Write([]byte(`{}`))
	})
	row := func(id string, s *fakeSess) map[string]any {
		closedAt := int64(0)
		if s.closed {
			closedAt = time.Now().Unix()
		}
		return map[string]any{"Id": id, "ModelAgentId": testModelID, "Provider": s.provider, "BidID": s.bid,
			"Stake": "1000", "OpenedAt": time.Now().Unix() - 10, "EndsAt": s.ends, "ClosedAt": closedAt}
	}
	mux.HandleFunc("GET /blockchain/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		s := m.sessions[r.PathValue("id")]
		if s == nil {
			http.Error(w, "not found", 404)
			return
		}
		switch m.sessionLookup {
		case "fail":
			http.Error(w, `{"error":"session detail unavailable"}`, http.StatusInternalServerError)
			return
		case "empty":
			rw := row(r.PathValue("id"), s)
			rw["Provider"] = ""
			_ = json.NewEncoder(w).Encode(map[string]any{"session": rw})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session": row(r.PathValue("id"), s)})
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		rows := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			for id, s := range m.sessions {
				rows = append(rows, row(id, s))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": rows})
	})
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.bidsFail {
			http.Error(w, `{"error":"bids unavailable"}`, http.StatusInternalServerError)
			return
		}
		if m.ratedErrBody != "" {
			http.Error(w, m.ratedErrBody, http.StatusInternalServerError)
			return
		}
		// Router shape: the outer ID is left unset (zero hash); the bid id is Bid.Id.
		_, _ = w.Write(ratedRows(m.feedRowsLocked()...))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		s := m.sessions[r.Header.Get("session_id")]
		if s == nil || s.closed {
			m.mu.Unlock()
			http.Error(w, `{"error":"session not found"}`, 400)
			return
		}
		m.prompts[s.provider]++
		n := m.prompts[s.provider]
		f := m.behave[s.provider]
		fr := m.behaveReq[s.provider]
		m.mu.Unlock()
		if fr != nil {
			fr(n, w, r)
			return
		}
		if f == nil {
			f = ok200
		}
		f(n, w)
	})
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /healthcheck", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"healthy"}`)) })
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"eth":1,"mor":1}`)) })
	ts := httptest.NewServer(mux)
	m.t.Cleanup(ts.Close)
	return ts
}

// feedRows is the market's bids, in rated order, with their health.
func (m *fakeMarket) feedRows() []feedRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.feedRowsLocked()
}

func (m *fakeMarket) feedRowsLocked() []feedRow {
	var out []feedRow
	for _, p := range m.providers {
		st := "healthy"
		if m.unhealthy[p] {
			st = "degraded"
		}
		out = append(out, feedRow{model: testModelID, bid: bidOf(p), prov: p, status: st})
	}
	return out
}

func (m *fakeMarket) snapshot() (opens []openRec, closes []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]openRec(nil), m.opens...), append([]string(nil), m.closes...)
}

type failoverEnv struct {
	ts  *httptest.Server
	srv *Server
	m   *fakeMarket
	key string
	// console is a logged-in admin cookie session (settingsAdmin).
	console *http.Client
}

func newFailoverEnv(t *testing.T, m *fakeMarket, edit func(*config.Config)) *failoverEnv {
	t.Helper()
	rt := m.server()
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)
	cfg := &config.Config{RouterURL: rt.URL, RouterUser: "admin", RouterPass: "routerpass", AdminPassword: "adminpass",
		APIKeySeed: "failover-seed", DataDir: t.TempDir(), ActiveModelsURL: cat.URL, GatewayBidsURL: rt.URL + "/gateway_bids.json",
		SessionDurationSec: 600, SessionFailover: true, ProviderCooldownSec: 600, SessionRelease: true}
	if edit != nil {
		edit(cfg)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := wiredServer(cfg, st, rc, cfg.SessionDurationSec)
	srv.pool.SetProviderCooldown(time.Duration(cfg.ProviderCooldownSec) * time.Second)
	ts := httptest.NewUnstartedServer(srv.Handler())
	if cfg.WebPublicURL == "" {
		cfg.WebPublicURL = "http://" + ts.Listener.Addr().String() // for the console's CSRF check
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return &failoverEnv{ts: ts, srv: srv, m: m, key: keymaker.MasterKey(cfg.APIKeySeed)}
}

func (e *failoverEnv) chat(t *testing.T, stream bool) (int, string) {
	t.Helper()
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]` + map[bool]string{true: `,"stream":true}`, false: `}`}[stream]
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (e *failoverEnv) failoverAudits() []string {
	var out []string
	for _, ev := range e.srv.audit.Recent(1000) {
		if ev.Kind == "provider_failover" {
			out = append(out, ev.Result)
		}
	}
	return out
}

func (e *failoverEnv) pooled() string {
	snap := e.srv.pool.Snapshot()
	if len(snap) != 1 {
		return ""
	}
	return snap[0].SessionID
}

const (
	provA = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	provB = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	provC = "0xcccccccccccccccccccccccccccccccccccccccc"
)

// 1. 402 on A, 200 on B: the client gets B's answer; one close of A, one
// open by B's bid with failover:false; audited; A cooling.
func TestFailoverProviderErrorMovesToOtherProvider(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = pme402
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, false)
	if code != 200 || !strings.Contains(body, `"hi"`) {
		t.Fatalf("got %d %s", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 2 || opens[0].provider != provA || opens[1].bid != bidOf(provB) || opens[1].provider != provB {
		t.Fatalf("opens %+v", opens)
	}
	if opens[1].body["failover"] != false {
		t.Fatalf("failover open body %v", opens[1].body)
	}
	if len(closes) != 1 || !strings.HasSuffix(closes[0], "aaaa") {
		t.Fatalf("closes %v", closes)
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "retried_ok" {
		t.Fatalf("audit %v", a)
	}
	if !e.srv.pool.IsCooling(testModelID, provA) || e.srv.pool.IsCooling(testModelID, provB) {
		t.Fatal("A should cool, B should not")
	}
	// The next request stays on B: no new open.
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("second request %d", code)
	}
	if opens, _ := m.snapshot(); len(opens) != 2 {
		t.Fatalf("second request opened again: %+v", opens)
	}
}

// 2. A real client 400 is relayed: no close, no open, no cool-down.
func TestFailoverClient400IsRelayed(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"max_tokens is too large"}},"statusCode":400}`))
	}
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, false)
	if code != 400 || !strings.Contains(body, "max_tokens is too large") {
		t.Fatalf("got %d %s", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 1 || len(closes) != 0 || e.srv.pool.IsCooling(testModelID, provA) || len(e.failoverAudits()) != 0 {
		t.Fatalf("opens %d closes %d", len(opens), len(closes))
	}
}

// 2b. A 400 provider error with an impaired pattern does fail over.
func TestFailoverImpaired400(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"Rate limit exceeded"}},"statusCode":400}`))
	}
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("got %d %s", code, body)
	}
}

// 3. Single provider: no close, no new open, 402 relayed, session kept.
// (Replaces TestProviderErrorDoesNotOpenSecondSession.)
func TestFailoverSingleProviderKeepsSession(t *testing.T) {
	m := newFakeMarket(t, provA)
	m.behave[provA] = pme402
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, false)
	if code != 402 || !strings.Contains(body, "providerModelError") {
		t.Fatalf("got %d %s", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 1 || len(closes) != 0 {
		t.Fatalf("opens %d closes %d", len(opens), len(closes))
	}
	if e.pooled() == "" {
		t.Fatal("session no longer pooled")
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "no_alternate" {
		t.Fatalf("audit %v", a)
	}
}

// 4. The retry fails too: relayed, one close and one open, no loop.
func TestFailoverRetryFailsNoLoop(t *testing.T) {
	m := newFakeMarket(t, provA, provB, provC)
	m.behave[provA] = pme402
	m.behave[provB] = func(n int, w http.ResponseWriter) { http.Error(w, `{"error":"upstream down"}`, 503) }
	e := newFailoverEnv(t, m, nil)
	code, _ := e.chat(t, false)
	if code != 503 {
		t.Fatalf("got %d", code)
	}
	opens, closes := m.snapshot()
	if len(opens) != 2 || len(closes) != 1 {
		t.Fatalf("opens %d closes %d", len(opens), len(closes))
	}
	if !e.srv.pool.IsCooling(testModelID, provB) {
		t.Fatal("B should cool after failing the retry")
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "retry_failed" {
		t.Fatalf("audit %v", a)
	}
}

// 5. A, B, C with B cooling and A failing: by-bid open on C. With only A
// and B: no_alternate, nothing closed.
func TestFailoverBidWalkSkipsCooling(t *testing.T) {
	m := newFakeMarket(t, provA, provB, provC)
	m.behave[provA] = pme402
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.MarkCooling(testModelID, provB)
	// The normal open avoids B too; A is first otherwise.
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("got %d %s", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 2 || opens[0].bid != bidOf(provA) || opens[0].provider != provA || opens[1].bid != bidOf(provC) || len(closes) != 1 {
		t.Fatalf("opens %+v closes %v", opens, closes)
	}

}

// 5b. Only A and B, B cooling, A failing: no_alternate, nothing closed.
func TestFailoverAllOthersCoolingNoAlternate(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = pme402
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.MarkCooling(testModelID, provB)
	// Normal open: B cooling, so by bid on A.
	code, body := e.chat(t, false)
	if code != 402 {
		t.Fatalf("got %d %s", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 1 || opens[0].provider != provA || len(closes) != 0 {
		t.Fatalf("opens %+v closes %v", opens, closes)
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "no_alternate" {
		t.Fatalf("audit %v", a)
	}
}

// 6. Cool-down 0: the immediate retry still avoids A (by bid on B), and a
// later B failure can go back to A.
func TestFailoverCooldownZero(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		if n == 1 {
			pme402(n, w)
			return
		}
		ok200(n, w)
	}
	m.behave[provB] = func(n int, w http.ResponseWriter) {
		if n == 2 {
			pme402(n, w)
			return
		}
		ok200(n, w)
	}
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ProviderCooldownSec = 0 })
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("first %d", code)
	}
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("second %d", code)
	}
	opens, _ := m.snapshot()
	if len(opens) != 3 || opens[1].bid != bidOf(provB) || opens[2].bid != bidOf(provA) || opens[2].provider != provA {
		t.Fatalf("opens %+v", opens)
	}
	if e.srv.pool.IsCooling(testModelID, provA) || e.srv.pool.IsCooling(testModelID, provB) {
		t.Fatal("cool-down 0 must not remember providers")
	}
}

// 7. Concurrent 402s on A: one close, one open, all finish.
func TestFailoverConcurrentSingleFlight(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		time.Sleep(20 * time.Millisecond)
		pme402(n, w)
	}
	e := newFailoverEnv(t, m, nil)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.chat(t, false)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	opens, closes := m.snapshot()
	// One open on A (shared by all four), one close of A, one open on B.
	if len(opens) != 2 || len(closes) != 1 {
		t.Fatalf("opens %d closes %d (want 2/1)", len(opens), len(closes))
	}
}

// 8. Close(A) fails: the retired session is not re-adopted by Rehydrate.
func TestFailoverCloseFailedNotReadopted(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = pme402
	m.closeStatus = 500
	e := newFailoverEnv(t, m, nil)
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("got %d", code)
	}
	a := e.failoverAudits()
	if len(a) != 2 || a[0] != "retried_ok" || a[1] != "close_failed" { // newest first
		t.Fatalf("audit %v", a)
	}
	// Make A's session the farthest-ending one, then rescan.
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.provider == provA {
			s.ends += 7200
		}
	}
	m.mu.Unlock()
	if err := e.srv.pool.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	if id := e.pooled(); !strings.HasSuffix(id, "bbbb") {
		t.Fatalf("pooled %q, want B's session", id)
	}
}

// 9. Mid-stream provider error: SSE error event, no reopen during the
// request, the next request opens on B.
func TestFailoverMidStream(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"Insufficient balance"}},"statusCode":402}`))
	}
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, true)
	if code != 200 || !strings.Contains(body, `"Hel"`) || !strings.Contains(body, `data: {"error":`) || strings.Contains(body, "providerModelError") {
		t.Fatalf("got %d %q", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 1 {
		t.Fatalf("reopened during the request: %+v", opens)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, closes := m.snapshot(); len(closes) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session was not retired after the stream error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("next request %d", code)
	}
	opens, _ := m.snapshot()
	// S4 [F3]: the open after a cut walks the bids, even with one provider cooling.
	if len(opens) != 2 || opens[1].provider != provB || opens[1].bid != bidOf(provB) {
		t.Fatalf("opens %+v", opens)
	}
}

// A mid-stream failure on the failover retry acts on the retried session
// (B), not the one that failed first (A): B cools, its session is retired
// and the next request opens on C.
func TestFailoverRetryMidStream(t *testing.T) {
	m := newFakeMarket(t, provA, provB, provC)
	m.behave[provA] = pme402
	m.behave[provB] = func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"Insufficient balance"}},"statusCode":402}`))
	}
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, true)
	if code != 200 || !strings.Contains(body, `"Hel"`) || !strings.Contains(body, `data: {"error":`) || strings.Contains(body, "providerModelError") {
		t.Fatalf("got %d %q", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		// The stream_retired audit is written by the background failover
		// goroutine after the close, so wait for both, not just the close.
		if _, closes := m.snapshot(); len(closes) == 2 && len(e.failoverAudits()) == 2 {
			break
		}
		if time.Now().After(deadline) {
			_, closes := m.snapshot()
			t.Fatalf("B's session was not retired: closes %v audits %v", closes, e.failoverAudits())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, closes := m.snapshot()
	if !strings.HasSuffix(closes[0], "aaaa") || !strings.HasSuffix(closes[1], "bbbb") {
		t.Fatalf("closes %v", closes)
	}
	if !e.srv.pool.IsCooling(testModelID, provB) || !e.srv.pool.IsCooling(testModelID, provA) {
		t.Fatal("A and B should both be cooling")
	}
	if a := e.failoverAudits(); len(a) != 2 || a[0] != "stream_retired" || a[1] != "retried_ok" { // newest first
		t.Fatalf("audits %v", a)
	}
	// Retired: a rehydrate does not adopt B's session back.
	_ = e.srv.pool.RehydrateForce()
	if id := e.pooled(); strings.HasSuffix(id, "bbbb") {
		t.Fatalf("B's session re-adopted: %s", id)
	}
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("next request %d", code)
	}
	opens, _ := m.snapshot()
	if last := opens[len(opens)-1]; last.provider != provC || last.bid != bidOf(provC) {
		t.Fatalf("next request opened on %s: %+v", last.provider, opens)
	}
}

// Pre-token streaming errors arrive as non-2xx, so they fail over like a
// buffered request.
func TestFailoverStreamBeforeFirstChunk(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = pme402
	m.behave[provB] = func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}
	e := newFailoverEnv(t, m, nil)
	code, body := e.chat(t, true)
	if code != 200 || !strings.Contains(body, `"ok"`) || strings.Contains(body, "providerModelError") {
		t.Fatalf("got %d %q", code, body)
	}
}

// 10. sessionFailover setting off: no failover, the status is relayed.
func TestFailoverDisabled(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = pme402
	e := newFailoverEnv(t, m, func(c *config.Config) { c.SessionFailover = false })
	code, _ := e.chat(t, false)
	opens, closes := m.snapshot()
	if code != 402 || len(opens) != 1 || len(closes) != 0 {
		t.Fatalf("code %d opens %d closes %d", code, len(opens), len(closes))
	}
}
