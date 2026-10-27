package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
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
// the bids (by bid, not omitProvider) and lands on B.
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

// S4: UPLINK_PROVIDER_EXCLUDE keeps opens off a provider, by id or name;
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

// S4 ruling: the soft avoidances (cool-down, the one-shot after-cut
// exclusion) fall back to a plain open when nothing else is left; a
// configured exclusion is never overridden, and when only configured-
// excluded providers remain the answer is 503 with no ids.
func TestS4FallbackRules(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)
	type tc struct {
		name      string
		providers []string
		exclude   []string
		cooling   []string
		cut       string
		code      int
		notOn     string // the open must not land on this provider
		plain     bool   // the open is a plain open (no omit, no bid)
	}
	for _, c := range []tc{
		{name: "all cooling → plain", providers: []string{a40, b40}, cooling: []string{a40, b40}, code: 200, plain: true},
		{name: "after-cut only provider → plain", providers: []string{a40}, cut: a40, code: 200, plain: true},
		{name: "cooling fallback keeps the configured exclusion", providers: []string{a40, b40, c40}, exclude: []string{a40}, cooling: []string{b40, c40}, code: 200, notOn: a40},
		{name: "after-cut fallback keeps the configured exclusion", providers: []string{a40, b40}, exclude: []string{a40}, cut: b40, code: 200, notOn: a40},
		{name: "two exclusions, rest cooling", providers: []string{a40, b40, c40}, exclude: []string{a40, b40}, cooling: []string{c40}, code: 200, notOn: a40},
		{name: "only excluded remain → 503", providers: []string{a40}, exclude: []string{a40}, code: 503},
		{name: "only excluded remain, two → 503", providers: []string{a40, b40}, exclude: []string{a40, b40}, cooling: []string{a40}, code: 503},
	} {
		m := newFakeMarket(t, c.providers...)
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
		code, body := e.chat(t, false)
		if code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
			continue
		}
		opens, _ := m.snapshot()
		if c.code == 503 {
			if len(opens) != 0 || strings.Contains(body, "0x") || !strings.Contains(body, "no_eligible_provider") {
				t.Errorf("%s: opens %d body %s", c.name, len(opens), body)
			}
			continue
		}
		last := opens[len(opens)-1]
		if c.notOn != "" {
			for _, o := range opens {
				for _, ex := range c.exclude {
					if o.provider == ex {
						t.Errorf("%s: opened on excluded provider %s", c.name, ex[:6])
					}
				}
			}
		}
		if c.plain && (last.omit != "" || last.bid != "") {
			t.Errorf("%s: want a plain open, got %+v", c.name, last)
		}
	}
}

// Conformance SHOULD-A: an omitProvider open that lands on a configured
// exclusion anyway (router not honouring omitProvider) is closed, and the
// request gets 503 with no ids.
func TestS4OmitOpenOnExcludedClosed(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	for _, bidsFail := range []bool{false, true} {
		m := newFakeMarket(t, a40, b40)
		m.ignoreOmit, m.bidsFail = true, bidsFail
		e := newFailoverEnv(t, m, nil)
		e.srv.pool.SetProviderExclude(map[string][]string{testModelID: {a40}}, nil)
		code, body := e.chat(t, false)
		opens, closes := m.snapshot()
		if code != 503 || strings.Contains(body, "0x") || !strings.Contains(body, "no_eligible_provider") {
			t.Errorf("bidsFail=%v: %d %s", bidsFail, code, body)
		}
		if len(opens) != 1 || opens[0].omit != a40 || opens[0].provider != a40 || len(closes) != 1 {
			t.Errorf("bidsFail=%v: opens %d closes %d", bidsFail, len(opens), len(closes))
		}
		if len(e.srv.pool.Snapshot()) != 0 {
			t.Errorf("bidsFail=%v: excluded session pooled", bidsFail)
		}
	}
}

// Conformance SHOULD-B: bids unreadable. One configured exclusion: the
// open sends omitProvider and does not land on it. Two or more: nothing is
// opened and the request gets 502.
func TestS4BidsUnreadableFallback(t *testing.T) {
	a40, b40, c40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40), "0x"+strings.Repeat("c", 40)

	m := newFakeMarket(t, a40, b40)
	m.bidsFail = true
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.SetProviderExclude(map[string][]string{testModelID: {a40}}, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("one exclusion: %d %s", code, body)
	}
	opens, _ := m.snapshot()
	if len(opens) != 1 || opens[0].omit != a40 || opens[0].bid != "" || opens[0].provider != b40 {
		t.Fatalf("one exclusion: opens %+v", opens)
	}

	m2 := newFakeMarket(t, a40, b40, c40)
	m2.bidsFail = true
	e2 := newFailoverEnv(t, m2, nil)
	e2.srv.pool.SetProviderExclude(map[string][]string{testModelID: {a40, b40}}, nil)
	code, body := e2.chat(t, false)
	if code != 502 || strings.Contains(body, "0x") {
		t.Fatalf("two exclusions: %d %s", code, body)
	}
	if opens, _ := m2.snapshot(); len(opens) != 0 {
		t.Fatalf("two exclusions opened %d", len(opens))
	}
}

// Security gate SHOULD-1: a name-keyed exclusion must not fail open when
// the catalog has no name for the id (cold boot, model feed down, client
// sends the raw 0x id): the open is refused with 503 and no ids until the
// name resolves; then the exclusion applies.
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
	e.srv.pool.SetProviderExclude(map[string][]string{"test-model": {a40}}, e.srv.catalog.NameByID)
	chatID := func() (int, string) {
		t.Helper()
		body := `{"model":"` + testModelID + `","messages":[{"role":"user","content":"hi"}]}`
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
	code, body := chatID()
	if code != 503 || !strings.Contains(body, "no_eligible_provider") || strings.Contains(body, a40) || strings.Contains(body, b40) {
		t.Fatalf("feed down: %d %s", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("feed down: opened %+v", opens)
	}
	feedUp.Store(true)
	if code, body := chatID(); code != 200 {
		t.Fatalf("feed up: %d %s", code, body)
	}
	for _, o := range func() []openRec { o, _ := m.snapshot(); return o }() {
		if o.provider == a40 {
			t.Fatal("opened on the name-keyed excluded provider")
		}
	}
}

// Review NIT (omit-path lookup): after an omitProvider open, when the
// session's provider cannot be read (detail lookup fails or has no
// provider) and a configured exclusion applies, the session is closed and
// a fresh open gets 503 with no ids. Without configured exclusions the
// session is used as before.
func TestS4OmitOpenProviderUnknownFailsClosed(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	for _, look := range []string{"fail", "empty"} {
		m := newFakeMarket(t, a40, b40)
		m.bidsFail, m.sessionLookup = true, look
		e := newFailoverEnv(t, m, nil)
		e.srv.pool.SetProviderExclude(map[string][]string{testModelID: {a40}}, nil)
		code, body := e.chat(t, false)
		opens, closes := m.snapshot()
		if code != 503 || strings.Contains(body, "0x") || !strings.Contains(body, "no_eligible_provider") {
			t.Errorf("lookup=%s: %d %s", look, code, body)
		}
		if len(opens) != 1 || opens[0].omit != a40 || len(closes) != 1 {
			t.Errorf("lookup=%s: opens %+v closes %v", look, opens, closes)
		}
		if len(e.srv.pool.Snapshot()) != 0 {
			t.Errorf("lookup=%s: unverified session pooled", look)
		}
	}
	// No configured exclusion: an unknown provider is not a reason to refuse.
	m := newFakeMarket(t, a40, b40)
	m.sessionLookup = "fail"
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("no exclusions: %d %s", code, body)
	}
}

// Review NIT (omit-path lookup), failover side: a pooled session is on a
// provider that is then configured as excluded; it fails with a 402, the
// replacement is opened with omitProvider, and its provider cannot be
// read. The replacement is closed and the caller gets the original
// upstream error, as when a replacement lands on an excluded provider.
func TestS4FailoverOmitProviderUnknownRelaysOriginal(t *testing.T) {
	m := newFakeMarket(t, provA, provB)
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("warm: %d %s", code, body)
	}
	m.mu.Lock()
	m.behave[provA] = pme402
	m.sessionLookup = "fail"
	m.mu.Unlock()
	e.srv.pool.SetProviderExclude(map[string][]string{testModelID: {provA}}, nil)
	code, body := e.chat(t, false)
	opens, closes := m.snapshot()
	if code != 402 || strings.Contains(body, "0x") {
		t.Fatalf("got %d %s, want the original 402 with no ids", code, body)
	}
	if len(opens) != 2 || opens[1].omit != provA {
		t.Fatalf("opens %+v", opens)
	}
	replacement := ""
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.sessions {
		if strings.HasPrefix(id, "0xsess02") {
			replacement = id
		}
	}
	closed := false
	for _, c := range closes {
		closed = closed || c == replacement
	}
	if replacement == "" || !closed {
		t.Fatalf("replacement %q not closed: %v", replacement, closes)
	}
	if a := e.failoverAudits(); len(a) != 1 || a[0] != "reopen_failed" {
		t.Errorf("audits %v", a)
	}
}
