package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const s4Chunk = "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"

// streamThenEOF sends one chunk and ends the body with no [DONE] and no
// finish_reason: an upstream cut.
func streamThenEOF(n int, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(s4Chunk))
	w.(http.Flusher).Flush()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// S4: an upstream EOF without [DONE] or finish_reason cools A and retires
// its session; the client gets the SSE error event; the next open walks
// the bids by bid and lands on B.
func TestS4EOFWithoutDoneCoolsAndRetires(t *testing.T) {
	for _, cool := range []int{600, 0} {
		m := newFakeMarket(t, provA, provB)
		m.behave[provA] = streamThenEOF
		e := newFailoverEnv(t, m, func(c *config.Config) { c.ProviderCooldownSec = cool })
		code, body := e.chat(t, true)
		if code != 200 || !strings.Contains(body, `"Hel"`) || !strings.Contains(body, `data: {"error":`) {
			t.Fatalf("cool %d: got %d %q", cool, code, body)
		}
		waitFor(t, "retire", func() bool { _, c := m.snapshot(); return len(c) == 1 && len(e.failoverAudits()) == 1 })
		if cool > 0 && !e.srv.pool.IsCooling(testModelID, provA) {
			t.Fatal("A not cooling")
		}
		if a := e.failoverAudits(); a[0] != "stream_retired" {
			t.Fatalf("audits %v", a)
		}
		if code, _ := e.chat(t, false); code != 200 {
			t.Fatalf("next %d", code)
		}
		opens, _ := m.snapshot()
		if len(opens) != 2 || opens[1].provider != provB || opens[1].bid != bidOf(provB) {
			t.Fatalf("cool %d: next open %+v, want a by-bid open on B", cool, opens)
		}
	}
}

// S4: a 400 providerModelError "error reading stream" before the first
// byte fails over once.
func TestS4ErrorReadingStreamBeforeFirstByteFailsOver(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	m.behave[provA] = func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"providerModelError":{"error":{"message":"error reading stream: unexpected EOF"}},"statusCode":400}`))
	}
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 || !strings.Contains(body, `"hi"`) {
		t.Fatalf("got %d %q", code, body)
	}
	opens, closes := m.snapshot()
	if len(opens) != 2 || opens[1].provider != provB || len(closes) != 1 {
		t.Fatalf("opens %+v closes %v", opens, closes)
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "retried_ok" {
		t.Fatalf("audits %v", a)
	}
}

// S4: the cut strings fail over only inside a providerModelError body.
func TestS4CutPatternsOnlyInProviderBody(t *testing.T) {
	for _, p := range []string{"error reading stream", "unexpected EOF", "connection reset by peer", "broken pipe"} {
		pme := &router.StatusError{Code: 400, Body: `{"providerModelError":{"error":{"message":"` + p + `"}}}`}
		if d := classifyForwardError(pme); d != decFailover {
			t.Fatalf("%q in providerModelError: %v", p, d)
		}
		plain := &router.StatusError{Code: 400, Body: `{"error":"` + p + `"}`}
		if d := classifyForwardError(plain); d != decRelay {
			t.Fatalf("%q outside providerModelError: %v", p, d)
		}
	}
}

// S4: normal streams ([DONE], or a finish_reason) do not cool.
func TestS4CompleteStreamDoesNotCool(t *testing.T) {
	for name, tail := range map[string]string{
		"done":   "data: [DONE]\n\n",
		"finish": "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
	} {
		m := newFakeMarket(t, provA, provB)
		m.behave[provA] = func(n int, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(s4Chunk + tail))
		}
		e := newFailoverEnv(t, m, nil)
		code, body := e.chat(t, true)
		if code != 200 || strings.Contains(body, `"error"`) {
			t.Fatalf("%s: %d %q", name, code, body)
		}
		assertNotCooled(t, e, m)
	}
}

func assertNotCooled(t *testing.T, e *failoverEnv, m *fakeMarket) {
	t.Helper()
	time.Sleep(150 * time.Millisecond) // a background failover would have started
	_, closes := m.snapshot()
	if e.srv.pool.IsCooling(testModelID, provA) || len(closes) != 0 || len(e.failoverAudits()) != 0 {
		t.Fatalf("cooled or retired: cooling=%v closes=%v audits=%v",
			e.srv.pool.IsCooling(testModelID, provA), closes, e.failoverAudits())
	}
	if e.pooled() == "" {
		t.Fatal("session left the pool")
	}
	// The next open is not steered away either: the pooled session is reused.
	if code, _ := e.chat(t, false); code != 200 {
		t.Fatalf("next %d", code)
	}
	if opens, _ := m.snapshot(); len(opens) != 1 {
		t.Fatalf("reopened: %+v", opens)
	}
}

// blockingUpstream sends one chunk, then waits for the request to be
// cancelled and ends without [DONE] (which would be a cut if it counted).
func blockingUpstream(seen chan<- struct{}) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(s4Chunk))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(seen)
		case <-time.After(5 * time.Second):
		}
	}
}

// S4 never-cools 1: the client closes its socket mid-stream.
func TestS4ClientClosesSocketDoesNotCool(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	seen := make(chan struct{})
	m.behaveReq[provA] = blockingUpstream(seen)
	e := newFailoverEnv(t, m, nil)
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.Contains(line, "Hel") {
		t.Fatalf("first line %q", line)
	}
	resp.Body.Close()
	tr.CloseIdleConnections()
	select {
	case <-seen: // the upstream request was cancelled with ours
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request not cancelled after the client hung up")
	}
	m.mu.Lock()
	m.behaveReq = map[string]func(int, http.ResponseWriter, *http.Request){}
	m.mu.Unlock()
	assertNotCooled(t, e, m)
}

// failWriter fails every write after the first with err, optionally
// cancelling the request context on the first write instead.
type failWriter struct {
	mu      sync.Mutex
	h       http.Header
	writes  int
	err     error
	onWrite func()
}

func (f *failWriter) Header() http.Header { return f.h }
func (f *failWriter) WriteHeader(int)     {}
func (f *failWriter) Flush()              {}
func (f *failWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.writes == 1 {
		if f.onWrite != nil {
			f.onWrite()
		}
		return len(b), nil
	}
	if f.err != nil {
		return 0, f.err
	}
	return len(b), nil
}

// twoChunksThenEOF sends two chunks and an EOF without [DONE].
func twoChunksThenEOF(n int, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(s4Chunk))
	w.(http.Flusher).Flush()
	time.Sleep(30 * time.Millisecond)
	_, _ = w.Write([]byte(s4Chunk))
	w.(http.Flusher).Flush()
}

func serveStream(t *testing.T, e *failoverEnv, ctx context.Context, w http.ResponseWriter) {
	t.Helper()
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:5000"
	e.srv.Handler().ServeHTTP(w, req)
}

// S4 never-cools 2 and 3: a write to our client fails with broken pipe or
// connection reset (and the upstream then also ends without [DONE]).
func TestS4ClientWriteErrorDoesNotCool(t *testing.T) {
	for _, werr := range []error{syscall.EPIPE, syscall.ECONNRESET} {
		m := newFakeMarket(t, provA, provB)
		m.behave[provA] = twoChunksThenEOF
		e := newFailoverEnv(t, m, nil)
		fw := &failWriter{h: http.Header{}, err: werr}
		serveStream(t, e, context.Background(), fw)
		if fw.writes < 2 {
			t.Fatalf("%v: writes %d", werr, fw.writes)
		}
		m.behave[provA] = nil
		assertNotCooled(t, e, m)
	}
}

// S4 never-cools 4, 5 and 6: the request context is cancelled, or one of
// Uplink's own timers (total, idle) ends the call mid-stream.
func TestS4OwnCancelAndTimeoutsDoNotCool(t *testing.T) {
	for name, cause := range map[string]error{
		"canceled":      context.Canceled,
		"total_timeout": router.ErrTotalTimeout,
		"idle_timeout":  router.ErrIdleTimeout,
	} {
		m := newFakeMarket(t, provA, provB)
		seen := make(chan struct{})
		m.behaveReq[provA] = blockingUpstream(seen)
		e := newFailoverEnv(t, m, nil)
		ctx, cancel := context.WithCancelCause(context.Background())
		fw := &failWriter{h: http.Header{}, onWrite: func() { cancel(cause) }}
		serveStream(t, e, ctx, fw)
		select {
		case <-seen:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: upstream not cancelled", name)
		}
		m.mu.Lock()
		m.behaveReq = map[string]func(int, http.ResponseWriter, *http.Request){}
		m.mu.Unlock()
		assertNotCooled(t, e, m)
	}
}

// S4: the providerExclude setting keeps opens off a provider, by id or name;
// bad entries are ignored.
func TestS4ProviderExcludeConfig(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	got := config.ParseProviderExclude("test-model=" + a40 + ",nope ; " + testModelID + "=" + b40 + ";bad;0x12=" + a40 + ";=" + a40)
	if len(got) != 2 || len(got["test-model"]) != 1 || got["test-model"][0] != a40 || got[testModelID][0] != b40 {
		t.Fatalf("parsed %v", got)
	}
	for _, key := range []string{"test-model", testModelID} {
		m := newFakeMarket(t, a40, b40)
		e := newFailoverEnv(t, m, nil)
		e.srv.pool.SetProviderExclude(map[string][]string{key: {a40}}, e.srv.catalog.NameByID)
		_, _ = e.srv.catalog.Resolve("test-model") // load the catalog for the name lookup
		if code, _ := e.chat(t, false); code != 200 {
			t.Fatalf("%s: chat %d", key, code)
		}
		opens, _ := m.snapshot()
		if len(opens) != 1 || opens[0].provider != b40 {
			t.Fatalf("%s: opens %+v", key, opens)
		}
	}
}

// A2 fail fast (DETAIL §14.4 #1-3, #6, #9): an avoidance is never dropped
// to get a session. Every eligible provider avoided → 503
// no_available_provider, nothing opened; only configured-excluded
// providers → 503 no_eligible_provider; anything avoided with a provider
// left → open by bid on it (never a plain open).
func TestS4FallbackRules(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	type tc struct {
		name      string
		providers []string
		exclude   []string
		cooling   []string
		cut       string
		code      int
		typ       string // 503 error type
		retry     bool   // a Retry-After header is expected
		on        string // 200: the provider the open must land on
	}
	for _, c := range []tc{
		{name: "all cooling → no_available", providers: []string{a40, b40}, cooling: []string{a40, b40}, code: 503, typ: "no_available_provider", retry: true},
		{name: "after-cut only provider → no_available", providers: []string{a40}, cut: a40, code: 503, typ: "no_available_provider"},
		{name: "configured + rest cooling → no_available", providers: []string{a40, b40, c40}, exclude: []string{a40}, cooling: []string{b40, c40}, code: 503, typ: "no_available_provider", retry: true},
		{name: "configured + rest cut → no_available", providers: []string{a40, b40}, exclude: []string{a40}, cut: b40, code: 503, typ: "no_available_provider"},
		{name: "two exclusions, rest cooling → no_available", providers: []string{a40, b40, c40}, exclude: []string{a40, b40}, cooling: []string{c40}, code: 503, typ: "no_available_provider", retry: true},
		{name: "only excluded remain → no_eligible", providers: []string{a40}, exclude: []string{a40}, code: 503, typ: "no_eligible_provider"},
		{name: "only excluded remain, two → no_eligible", providers: []string{a40, b40}, exclude: []string{a40, b40}, cooling: []string{a40}, code: 503, typ: "no_eligible_provider"},
		{name: "one configured exclusion → by bid", providers: []string{a40, b40}, exclude: []string{a40}, code: 200, on: b40},
		{name: "one cooling → by bid", providers: []string{a40, b40}, cooling: []string{a40}, code: 200, on: b40},
		{name: "after cut → by bid", providers: []string{a40, b40}, cut: a40, code: 200, on: b40},
	} {
		m := newFakeMarket(t, c.providers...)
		// A by-model open on the stub lands on the first provider.
		e := newFailoverEnv(t, m, nil)
		if len(c.exclude) > 0 {
			e.srv.pool.SetProviderExclude(map[string][]string{testModelID: c.exclude}, nil)
		}
		for _, p := range c.cooling {
			e.srv.pool.MarkCooling(testModelID, p)
		}
		if c.cut != "" {
			e.srv.pool.MarkStreamCut(testModelID, c.cut)
		}
		code, hdr, body := chatFull(t, e, "test-model")
		if code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
			continue
		}
		opens, _ := m.snapshot()
		if c.code == 503 {
			if len(opens) != 0 || strings.Contains(body, "0x") || !strings.Contains(body, `"type":"`+c.typ+`"`) {
				t.Errorf("%s: opens %d body %s", c.name, len(opens), body)
			}
			ra := hdr.Get("Retry-After")
			if c.retry != (ra != "") || ra != "" && !retryAfterOK(ra, 600) {
				t.Errorf("%s: Retry-After %q", c.name, ra)
			}
			continue
		}
		if len(opens) != 1 || opens[0].bid != bidOf(c.on) || opens[0].provider != c.on {
			t.Errorf("%s: want one by-bid open on %s, got %+v", c.name, c.on[:6], opens)
		}
	}
}

// retryAfterOK: digits only, 1..max.
func retryAfterOK(v string, max int) bool {
	n, err := strconv.Atoi(v)
	return err == nil && strconv.Itoa(n) == v && n >= 1 && n <= max
}

// chatFull is chatModel with the response headers.
func chatFull(t *testing.T, e *failoverEnv, model string) (int, http.Header, string) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

// MUST-1 (DETAIL §14.4 #8): rated bids unreadable while any provider is
// avoided (cooling, cut, one or two configured exclusions) → 503
// no_available_provider, no ids, and ZERO open attempts of any kind
// (counted by model and by bid on a router stub whose open by model
// ignores any avoidance),
// again on a second request; Retry-After only when a cool-down is known.
// With nothing avoided, one normal open.
func TestS4BidsUnreadableFallback(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	for _, c := range []struct {
		name    string
		exclude []string
		cooling []string
		cut     string
		retry   bool
	}{
		{name: "cooling", cooling: []string{a40}, retry: true},
		{name: "cut", cut: a40},
		{name: "one configured exclusion", exclude: []string{a40}},
		{name: "two configured exclusions", exclude: []string{a40, b40}},
		{name: "configured + cooling", exclude: []string{a40}, cooling: []string{b40}, retry: true},
	} {
		m := newFakeMarket(t, a40, b40, c40)
		m.bidsFail = true
		e := newFailoverEnv(t, m, nil)
		if len(c.exclude) > 0 {
			e.srv.pool.SetProviderExclude(map[string][]string{testModelID: c.exclude}, nil)
		}
		for _, p := range c.cooling {
			e.srv.pool.MarkCooling(testModelID, p)
		}
		if c.cut != "" {
			e.srv.pool.MarkStreamCut(testModelID, c.cut)
		}
		code, hdr, body := chatFull(t, e, "test-model")
		if code != 503 || !strings.Contains(body, `"type":"no_available_provider"`) || strings.Contains(body, "0x") {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if ra := hdr.Get("Retry-After"); c.retry != (ra != "") || ra != "" && !retryAfterOK(ra, 600) {
			t.Errorf("%s: Retry-After %q", c.name, ra)
		}
		if opens, _ := m.snapshot(); len(opens) != 0 {
			t.Errorf("%s: %d open attempts, want 0: %+v", c.name, len(opens), opens)
		}
		code, _, _ = chatFull(t, e, "test-model")
		opens, _ := m.snapshot()
		if c.cut != "" {
			// The refusal consumed the cut: nothing is avoided any more.
			if code != 200 || len(opens) != 1 || opens[0].bid != "" {
				t.Errorf("%s: after the cut was consumed: %d opens %+v", c.name, code, opens)
			}
			continue
		}
		if code != 503 || len(opens) != 0 {
			t.Errorf("%s: second request %d, %d open attempts", c.name, code, len(opens))
		}
	}
	// Nothing avoided: one normal open by model.
	m := newFakeMarket(t, a40, b40)
	m.bidsFail = true
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("nothing avoided: %d %s", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 1 || opens[0].bid != "" {
		t.Fatalf("nothing avoided: opens %+v", opens)
	}
}

// Security gate SHOULD-1 and the console-warning NIT: a name-keyed
// exclusion must not fail open when the catalog has no name for the id
// (cold boot, model feed down, client sends the raw 0x id). The open is
// refused with 503 and no ids until the name resolves, the status reports
// the refusal while it lasts, and then the exclusion applies.
func TestS4NameKeyedExclusionFailsClosed(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	var feedUp atomic.Bool
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !feedUp.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"Name":"test-model","Id":"` + testModelID + `","Tags":["chat"]}]}`))
	}))
	t.Cleanup(feed.Close)
	m := newFakeMarket(t, a40, b40)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = feed.URL })
	e.srv.catalog.SetRetryAfter(0) // the feed comes back within the test
	e.srv.pool.SetProviderExclude(map[string][]string{"test-model": {a40}}, e.srv.catalog.NameByID)
	chatID := func() (int, string) { t.Helper(); return chatModel(t, e, testModelID) }
	code, body := chatID()
	if code != 503 || !strings.Contains(body, "no_eligible_provider") || strings.Contains(body, a40) || strings.Contains(body, b40) {
		t.Fatalf("feed down: %d %s", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("feed down: opened %+v", opens)
	}
	// The console status reports the refusal (a count, no ids) while it lasts.
	unresolved := func() string { t.Helper(); n, _ := statusRefusals(t, e); return n }
	if n := unresolved(); n != "1" {
		t.Fatalf("status exclusionsUnresolved=%s while refusing, want 1", n)
	}
	feedUp.Store(true)
	if code, body := chatID(); code != 200 {
		t.Fatalf("feed up: %d %s", code, body)
	}
	if n := unresolved(); n != "0" {
		t.Fatalf("status exclusionsUnresolved=%s after the name resolved, want 0", n)
	}
	for _, o := range func() []openRec { o, _ := m.snapshot(); return o }() {
		if o.provider == a40 {
			t.Fatal("opened on the name-keyed excluded provider")
		}
	}
}

// chatModel sends a chat completion for the model string as given.
func chatModel(t *testing.T, e *failoverEnv, model string) (int, string) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
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

// statusRefusals reads exclusionsUnresolved and its capped flag from the
// console status.
func statusRefusals(t *testing.T, e *failoverEnv) (string, string) {
	t.Helper()
	code, body := e.admin(t, "GET", "/admin/status", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(st["exclusionsUnresolved"]), fmt.Sprint(st["exclusionsUnresolvedCapped"])
}

// Security gate SHOULD-2: one client key sending many distinct model ids
// while the feed is down at boot. Every request is still refused with 503
// and no ids; the status count stops at the cap with the capped flag set;
// ill-formed ids are not counted; and neither the requests nor the status
// polls turn into a feed fetch each (the stub feed counts fetches).
func TestS4RefusalRecordCappedNoFetchStorm(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	var fetches atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(feed.Close)
	m := newFakeMarket(t, a40, b40)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = feed.URL })
	e.srv.pool.SetProviderExclude(map[string][]string{"test-model": {a40}}, e.srv.catalog.NameByID)
	start := fetches.Load()

	for i := 0; i < pool.MaxUnresolvedRefusals+20; i++ {
		id := fmt.Sprintf("0x%064x", i+1)
		if code, body := chatModel(t, e, id); code != 503 || !strings.Contains(body, "no_eligible_provider") || strings.Contains(body, "0x") {
			t.Fatalf("id %d: %d %s", i, code, body)
		}
	}
	for _, bad := range []string{"0x12", "0x" + strings.Repeat("z", 64), "0x" + strings.Repeat("a", 70)} {
		if code, _ := chatModel(t, e, bad); code == 200 {
			t.Fatalf("ill-formed id %q served", bad)
		}
	}
	for i := 0; i < 5; i++ { // the Status tab polls
		n, capped := statusRefusals(t, e)
		if n != fmt.Sprint(pool.MaxUnresolvedRefusals) || capped != "true" {
			t.Fatalf("status poll %d: exclusionsUnresolved=%s capped=%s", i, n, capped)
		}
	}
	if got := fetches.Load() - start; got > 1 {
		t.Fatalf("feed fetched %d times for %d requests and 5 status polls, want at most 1", got, pool.MaxUnresolvedRefusals+23)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("opened %d sessions", len(opens))
	}
}
