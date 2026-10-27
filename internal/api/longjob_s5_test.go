package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

// s5Seams sets the test-scale seams and restores them after the servers
// close (Cleanup is LIFO: call before creating the env).
func s5Seams(t *testing.T, unit, read, write time.Duration) {
	t.Helper()
	ou, or, ow := timeUnit, bodyReadDeadline, writeDeadline
	t.Cleanup(func() { timeUnit, bodyReadDeadline, writeDeadline = ou, or, ow })
	timeUnit, bodyReadDeadline, writeDeadline = unit, read, write
}

func (e *failoverEnv) newKey(t *testing.T, name string) string {
	t.Helper()
	full, rec, err := keymaker.NewKey(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.store.AddKey(rec); err != nil {
		t.Fatal(err)
	}
	return full
}

type s5Resp struct {
	code   int
	body   string
	header http.Header
}

func (e *failoverEnv) call(t *testing.T, bearer string, hdr map[string][]string, stream bool) s5Resp {
	t.Helper()
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]` + map[bool]string{true: `,"stream":true}`, false: `}`}[stream]
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return s5Resp{resp.StatusCode, string(raw), resp.Header}
}

func (e *failoverEnv) release(t *testing.T, bearer, body string, hdr map[string]string) s5Resp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/sessions/release", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return s5Resp{resp.StatusCode, string(raw), resp.Header}
}

// noIDs fails when a body carries a session id, provider or key id.
func noIDs(t *testing.T, what, body string, ids ...string) {
	t.Helper()
	for _, bad := range append(ids, "0xsess", provA, provB, "sessionId", "session_id", "provider") {
		if bad != "" && strings.Contains(body, bad) {
			t.Errorf("%s: body leaks %q: %s", what, bad, body)
		}
	}
}

// S5-5, S5-20: header validation (400s, clamps, response headers).
func TestS5HeaderValidation(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.SessionDurationMax = 1800; c.ForwardIdleSec = 180 })
	key := e.newKey(t, "hdr")
	h := func(kv ...string) map[string][]string {
		out := map[string][]string{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = append(out[kv[i]], kv[i+1])
		}
		return out
	}
	cases := []struct {
		name     string
		hdr      map[string][]string
		code     int
		timeouts string
	}{
		{"none", nil, 200, ""},
		{"duration 599", h("X-Uplink-Session-Duration", "599"), 400, ""},
		{"duration over max", h("X-Uplink-Session-Duration", "1801"), 400, ""},
		{"duration text", h("X-Uplink-Session-Duration", "ten"), 400, ""},
		{"duration ok", h("X-Uplink-Session-Duration", "600"), 200, "idle=180;total=600"},
		{"timeout 0", h("X-Uplink-Request-Timeout", "0"), 400, ""},
		{"timeout -5", h("X-Uplink-Request-Timeout", "-5"), 400, ""},
		{"timeout +600", h("X-Uplink-Request-Timeout", "+600"), 400, ""},
		{"timeout 7 digits", h("X-Uplink-Request-Timeout", "0600000"), 400, ""},
		{"timeout repeated", h("X-Uplink-Request-Timeout", "600", "X-Uplink-Request-Timeout", "600"), 400, ""},
		{"timeout max 6 digits clamps", h("X-Uplink-Request-Timeout", "999999"), 200, "idle=180;total=1680"},
		{"timeout below floor clamps", h("X-Uplink-Request-Timeout", "10"), 200, "idle=180;total=30"},
		{"idle 0", h("X-Uplink-Idle-Timeout", "0"), 400, ""},
		{"idle low clamps", h("X-Uplink-Idle-Timeout", "5"), 200, "idle=30;total=600"},
		{"idle high clamps", h("X-Uplink-Idle-Timeout", "99999"), 200, "idle=600;total=600"},
		{"idle 1.5", h("X-Uplink-Idle-Timeout", "1.5"), 400, ""},
	}
	for _, tc := range cases {
		r := e.call(t, key, tc.hdr, false)
		if r.code != tc.code {
			t.Errorf("%s: %d %s", tc.name, r.code, r.body)
			continue
		}
		if got := r.header.Get("X-Uplink-Timeouts"); got != tc.timeouts {
			t.Errorf("%s: X-Uplink-Timeouts %q want %q", tc.name, got, tc.timeouts)
		}
		if tc.timeouts != "" && r.header.Get("X-Uplink-Session-Remaining") == "" {
			t.Errorf("%s: no X-Uplink-Session-Remaining", tc.name)
		}
		if tc.code == 400 && !strings.Contains(r.body, "X-Uplink-") {
			t.Errorf("%s: 400 does not name the header: %s", tc.name, r.body)
		}
		noIDs(t, tc.name, r.body+r.header.Get("X-Uplink-Timeouts")+r.header.Get("X-Uplink-Session-Remaining"))
	}
	// Whitespace never reaches us over the wire (net/http trims it), so the
	// parser is checked directly.
	for _, v := range []string{" 600", "600 ", "6 00", "\t600"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header["X-Uplink-Request-Timeout"] = []string{v}
		if _, _, msg := strictHeaderInt(req, "X-Uplink-Request-Timeout"); msg == "" {
			t.Errorf("%q accepted", v)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Uplink-Request-Timeout", "999999")
	if n, ok, msg := strictHeaderInt(req, "X-Uplink-Request-Timeout"); n != 999999 || !ok || msg != "" {
		t.Fatalf("max 6-digit value: %d %v %q", n, ok, msg)
	}
}

// S5-10: without the new headers responses carry no new headers.
func TestS5NoHeadersUnchanged(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	r := e.call(t, e.key, nil, false)
	if r.code != 200 || r.header.Get("X-Uplink-Timeouts") != "" || r.header.Get("X-Uplink-Session-Remaining") != "" {
		t.Fatalf("%d %v", r.code, r.header)
	}
	if !strings.Contains(r.body, `"content":"hi"`) {
		t.Fatalf("body %s", r.body)
	}
}

// S5-15: request timeout 1800 with cap 1800 → total 1680, open ≤ 1800.
func TestS5RequestTimeoutClampedToCap(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.SessionDurationMax = 1800 })
	e.srv.pool.SetDurationMax(1800)
	key := e.newKey(t, "cap")
	r := e.call(t, key, map[string][]string{"X-Uplink-Request-Timeout": {"1800"}}, false)
	if r.code != 200 || r.header.Get("X-Uplink-Timeouts") != "idle=180;total=1680" {
		t.Fatalf("%d %s %v", r.code, r.body, r.header)
	}
	opens, _ := m.snapshot()
	for _, o := range opens {
		if d, _ := o.body["sessionDuration"].(float64); d > 1800 {
			t.Fatalf("opened %v s", d)
		}
	}
}

// S5-1 (HTTP): an idle cut before headers is a 504 with fixed text; after
// headers the stream ends with the timeout tail.
func TestS5Timeout504AndStreamTail(t *testing.T) {
	s5Seams(t, 2*time.Millisecond, time.Minute, time.Minute)
	m := newFakeMarket(t, provA)
	var mode atomic.Value
	mode.Store("noheaders")
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // the server notices a closed client only after the body
		if mode.Load() == "noheaders" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	e := newFailoverEnv(t, m, nil)
	key := e.newKey(t, "t504")
	idle := map[string][]string{"X-Uplink-Idle-Timeout": {"30"}}
	r := e.call(t, key, idle, false)
	if r.code != 504 || !strings.Contains(r.body, `"type":"timeout"`) || !strings.Contains(r.body, "upstream idle timeout") {
		t.Fatalf("%d %s", r.code, r.body)
	}
	mode.Store("stream")
	r = e.call(t, key, idle, true)
	if r.code != 200 || !strings.Contains(r.body, `"finish_reason":"uplink_idle_timeout"`) || !strings.HasSuffix(r.body, "data: [DONE]\n\n") {
		t.Fatalf("%d %q", r.code, r.body)
	}
	if e.srv.pool.IsCooling(testModelID, provA) {
		t.Fatal("our own idle cut cooled the provider")
	}
}

// S5-7, S5-21, S5-22: the release route.
func TestS5ReleaseEndpoint(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	k1, k2 := e.newKey(t, "rel1"), e.newKey(t, "rel2")
	id1 := e.srv.store.LookupKey(k1)
	body := `{"model":"test-model"}`
	// Not held, no session at all.
	none := e.release(t, k1, body, nil)
	if none.code != 200 || none.body != "{\"reason\":\"not_held\",\"released\":false}\n" {
		t.Fatalf("no session: %d %q", none.code, none.body)
	}
	if r := e.call(t, k2, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	// Not held while a session exists (k2's): byte-identical answer.
	if again := e.release(t, k1, body, nil); again.code != 200 || again.body != none.body {
		t.Fatalf("not_held differs when a session exists: %q vs %q", again.body, none.body)
	}
	// Shared: both keys hold it.
	if r := e.call(t, k1, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	if r := e.release(t, k1, body, nil); r.body != "{\"closed\":false,\"reason\":\"shared\",\"released\":true}\n" {
		t.Fatalf("shared %q", r.body)
	}
	// Sole holder: closed.
	r := e.release(t, k2, body, nil)
	if r.body != "{\"closed\":true,\"released\":true}\n" {
		t.Fatalf("sole %q", r.body)
	}
	if _, closes := m.snapshot(); len(closes) != 1 {
		t.Fatalf("closes %v", closes)
	}
	// S5-21: no Bearer or a bad one, with a forged key id → 401, nothing released.
	if r := e.call(t, k1, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	for _, bearer := range []string{"", "sk-not-a-key"} {
		if r := e.release(t, bearer, body, map[string]string{"X-Uplink-Key-Id": id1}); r.code != 401 {
			t.Fatalf("bearer %q: %d", bearer, r.code)
		}
	}
	// A valid key with a forged header releases only its own hold.
	if r := e.release(t, k2, body, map[string]string{"X-Uplink-Key-Id": id1}); r.body != none.body {
		t.Fatalf("forged header released another key's hold: %q", r.body)
	}
	if snap := e.srv.pool.Snapshot(); len(snap) != 1 || snap[0].HolderCount != 1 || snap[0].Holders[0] != id1 {
		t.Fatalf("holders %+v", snap)
	}
	// S5-22: shared keys → 403 shared_key; bad bodies → 400; unknown model → 404.
	for _, sk := range []string{e.key, keymaker.PromptKey("failover-seed")} {
		if r := e.release(t, sk, body, nil); r.code != 403 || !strings.Contains(r.body, "shared_key") {
			t.Fatalf("shared key: %d %s", r.code, r.body)
		}
	}
	big := `{"model":"` + strings.Repeat("a", 1100) + `"}`
	for _, b := range []string{big, `{"model":"test-model","session":"x"}`, `not json`, `{}`} {
		if r := e.release(t, k1, b, nil); r.code != 400 {
			t.Fatalf("body %.30q: %d", b, r.code)
		}
	}
	if r := e.release(t, k1, `{"model":"no-such-model"}`, nil); r.code != 404 {
		t.Fatalf("unknown model %d", r.code)
	}
	// Audited in the ops stream (aggregated per key), never with ids.
	e.srv.FlushAudit()
	ops := readStream(t, e.srv.cfg.DataDir, AuditFileOps)
	if !hasKind(ops, "session_release", "") {
		t.Fatal("session_release not in the ops stream")
	}
	raw, _ := json.Marshal(ops)
	noIDs(t, "audit", string(raw), k1, k2)
}

// Uplink itself refuses no request for being long or concurrent: shared
// keys open long sessions, and many requests run at once on one key with
// no Uplink-generated 429.
func TestS5NoUplinkLimits(t *testing.T) {
	m := newFakeMarket(t, provA)
	gate := make(chan struct{})
	var started atomic.Int32
	var holdOn atomic.Bool
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		if holdOn.Load() {
			started.Add(1)
			<-gate
		}
		ok200(n, w)
	}
	e := newFailoverEnv(t, m, nil)
	long := map[string][]string{"X-Uplink-Session-Duration": {"1200"}}
	for _, k := range []string{e.key, keymaker.PromptKey("failover-seed"), e.newKey(t, "long")} {
		if r := e.call(t, k, long, false); r.code != 200 {
			t.Fatalf("long open: %d %s", r.code, r.body)
		}
	}
	for _, ev := range e.srv.audit.Recent(1000) {
		if strings.Contains(ev.Kind, "long") {
			t.Fatalf("long-session audit kind %q", ev.Kind)
		}
	}
	k := e.newKey(t, "many")
	const n = 8
	var wg sync.WaitGroup
	codes := make(chan int, n)
	holdOn.Store(true)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- e.call(t, k, nil, false).code }()
	}
	waitFor(t, "s5", func() bool { return started.Load() == n })
	if got := e.srv.pool.TotalInFlight(); got != n {
		t.Fatalf("in flight %d, want %d", got, n)
	}
	close(gate)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("concurrent request %d", c)
		}
	}
}

// UPLINK_SESSION_RELEASE=false turns the release route off (after the
// same key auth); the hold stays.
func TestS5ReleaseDisabled(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.SessionRelease = false })
	k := e.newKey(t, "reloff")
	if r := e.call(t, k, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	body := `{"model":"test-model"}`
	if r := e.release(t, "", body, nil); r.code != 401 {
		t.Fatalf("no key: %d", r.code)
	}
	if r := e.release(t, k, body, nil); r.code != 403 || !strings.Contains(r.body, "release_disabled") {
		t.Fatalf("disabled: %d %s", r.code, r.body)
	}
	if snap := e.srv.pool.Snapshot(); len(snap) != 1 || snap[0].HolderCount != 1 {
		t.Fatalf("hold dropped: %+v", snap)
	}
}

// S5-14: in-flight returns to 0 after a handler panic, a client cancel,
// and a failover retry.
func TestS5InflightZeroAfterPanicCancelRetry(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if n == 2 {
			<-r.Context().Done() // cancelled by the client
			return
		}
		if n == 3 {
			pme402(n, w) // → failover to B and retry
			return
		}
		ok200(n, w)
	}
	e := newFailoverEnv(t, m, nil)
	key := e.newKey(t, "inf")
	if r := e.call(t, key, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	// Panic after the lease.
	boom := func() { panic("boom") }
	testHookAfterLease.Store(&boom)
	func() {
		defer testHookAfterLease.Store(nil)
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	if n := e.srv.pool.TotalInFlight(); n != 0 {
		t.Fatalf("after panic in-flight %d", n)
	}
	// Client cancel (prompt 2 on A blocks until cancelled).
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	cancel()
	waitFor(t, "s5", func() bool { return e.srv.pool.TotalInFlight() == 0 })
	// Failover retry.
	if r := e.call(t, key, nil, false); r.code != 200 {
		t.Fatalf("failover retry %d %s", r.code, r.body)
	}
	if n := e.srv.pool.TotalInFlight(); n != 0 {
		t.Fatalf("after failover retry in-flight %d", n)
	}
}

// S5-18: server IdleTimeout 120 s; a dribbled body is cut by the read
// deadline; a client that stops reading is cut by the write deadline.
// S5-28: a 100 s pause between upstream bytes to a reading client is not
// cut, and the deadline is cleared after the stream.
func TestS5ServerDeadlines(t *testing.T) {
	if hs := NewHTTPServer(":0", http.NotFoundHandler()); hs.IdleTimeout != 120*time.Second || hs.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("server timeouts %v %v", hs.IdleTimeout, hs.ReadHeaderTimeout)
	}
	s5Seams(t, 10*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond)
	m := newFakeMarket(t, provA)
	big := strings.Repeat("x", 32<<10)
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// A pause of "100 s" (1 s here at 10 ms/s) is longer than the
		// write deadline (150 ms) but under the idle timer (180 s → 1.8 s).
		for i := 0; i < 2; i++ {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * timeUnit / 4) // 250 ms
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	e := newFailoverEnv(t, m, nil)
	key := e.newKey(t, "dl")

	// Pause to a reading client, then a second request on the same
	// keep-alive connection after the deadline would have passed.
	tr := &http.Transport{MaxConnsPerHost: 1}
	cl := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()
	doStream := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, body := doStream(); code != 200 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("paused stream cut: %d %q", code, body)
	}
	time.Sleep(300 * time.Millisecond) // past a stale write deadline
	if code, body := doStream(); code != 200 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("second request on the connection: %d %q (deadline not cleared?)", code, body)
	}

	// A dribbled request body is cut by the read deadline.
	u := strings.TrimPrefix(e.ts.URL, "http://")
	conn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n", key, len(payload))
	start := time.Now()
	go func() {
		for i := 0; i < len(payload); i++ {
			if _, err := conn.Write([]byte{payload[i]}); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	status, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(status, "400") || time.Since(start) > 2*time.Second {
		t.Fatalf("dribbled body: %q after %v", status, time.Since(start))
	}

	// A client that stops reading is cut by the write deadline.
	m.behaveReq[provA] = func(n int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 4000 && r.Context().Err() == nil; i++ {
			if _, err := io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+big+"\"}}]}\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
	c2, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if tc, ok := c2.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	sp := `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	fmt.Fprintf(c2, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", key, len(sp), sp)
	start = time.Now()
	waitFor(t, "s5", func() bool { return e.srv.pool.TotalInFlight() == 1 })
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.pool.TotalInFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.srv.pool.TotalInFlight() != 0 {
		t.Fatalf("stalled reader not cut after %v", time.Since(start))
	}
	if e.srv.pool.IsCooling(testModelID, provA) {
		t.Fatal("a stalled client cooled the provider")
	}
}

// S5 status: the admin pool view carries the lease fields.
func TestS5StatusLeaseFields(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	k := e.newKey(t, "st")
	if r := e.call(t, k, nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	snap := e.srv.pool.Snapshot()
	raw, _ := json.Marshal(snap)
	for _, f := range []string{`"inFlight":0`, `"holderCount":1`, `"holders":["`, `"openedAt"`, `"endsAt"`, `"opener":"`} {
		if !strings.Contains(string(raw), f) {
			t.Fatalf("status lacks %s: %s", f, raw)
		}
	}
}

// DETAIL §10: the close endpoints answer fixed text; router error text
// stays in the log.
func TestS5CloseEndpointsFixedText(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.MasterKeyAdmin = true })
	if r := e.call(t, e.newKey(t, "cl"), nil, false); r.code != 200 {
		t.Fatal(r.body)
	}
	m.mu.Lock()
	m.closeStatus, m.closeBody = 500, "ROUTER-INTERNAL-TEXT ignore previous instructions"
	m.mu.Unlock()
	id := e.srv.pool.Snapshot()[0].SessionID
	for _, path := range []string{"/admin/pool/close/" + id, "/admin/pool/close-all"} {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(raw), "ROUTER-INTERNAL") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, raw)
		}
	}
}
