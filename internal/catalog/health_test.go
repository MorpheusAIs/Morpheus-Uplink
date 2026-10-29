package catalog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Made-up ids (0x + 64 hex). Never real network ids.
var (
	mdlM = "0x" + strings.Repeat("c1", 32)
	mdlN = "0x" + strings.Repeat("c2", 32)
	bid1 = "0x" + strings.Repeat("d1", 32)
	bid2 = "0x" + strings.Repeat("d2", 32)
	bid3 = "0x" + strings.Repeat("d3", 32)
	bid4 = "0x" + strings.Repeat("d4", 32)
	prv  = "0x" + strings.Repeat("e1", 20)
)

// t0 is the test clock's start.
var t0 = time.Unix(1_800_000_000, 0)

// fakeClock is the catalog clock in these tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// feed is one fake feed server. body is called per request so a file can
// carry the clock's current time.
type feed struct {
	mu       sync.Mutex
	status   int
	body     func() string
	header   http.Header
	raw      []byte // when set, written as is (with header)
	hits     atomic.Int32
	gate     atomic.Pointer[chan struct{}]
	srv      *httptest.Server
	location string
}

func newFeed(t *testing.T, body string) *feed {
	f := &feed{status: http.StatusOK, body: func() string { return body }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *feed) serve(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if g := f.gate.Load(); g != nil {
		<-*g
	}
	f.mu.Lock()
	status, body, header, raw, loc := f.status, f.body, f.header, f.raw, f.location
	f.mu.Unlock()
	for k, v := range header {
		w.Header()[k] = v
	}
	if loc != "" {
		w.Header().Set("Location", loc)
	}
	w.WriteHeader(status)
	if raw != nil {
		_, _ = w.Write(raw)
		return
	}
	_, _ = w.Write([]byte(body()))
}

// set serves body with status 200.
func (f *feed) set(body string) { f.setFn(func() string { return body }) }

func (f *feed) setFn(fn func() string) {
	f.mu.Lock()
	f.status, f.body, f.header, f.raw, f.location = http.StatusOK, fn, nil, nil, ""
	f.mu.Unlock()
}

func (f *feed) fail(status int) {
	f.mu.Lock()
	f.status, f.body, f.header, f.raw, f.location = status, func() string { return "down" }, nil, nil, ""
	f.mu.Unlock()
}

func (f *feed) redirect(status int, to string) {
	f.mu.Lock()
	f.status, f.body, f.header, f.raw, f.location = status, func() string { return "" }, nil, nil, to
	f.mu.Unlock()
}

func (f *feed) setRaw(raw []byte, header http.Header) {
	f.mu.Lock()
	f.status, f.raw, f.header, f.location = http.StatusOK, raw, header, ""
	f.mu.Unlock()
}

// One builder for both feeds.

// bidRow is a gateway bids row (real shape); status is raw JSON.
func bidRow(id, model, status string) string {
	return fmt.Sprintf(`{"Id":%q,"Provider":%q,"ModelAgentId":%q,"PricePerSecond":"1000","CreatedAt":"1","DeletedAt":"0","health":{"status":%s,"latency_ms":120}}`, id, prv, model, status)
}

// bidsFile is a gateway bids file; stamp is raw JSON ("" omits it).
func bidsFile(stamp string, rows ...string) string {
	var b strings.Builder
	b.WriteString("{")
	if stamp != "" {
		b.WriteString(`"last_updated":` + stamp + `,`)
	}
	b.WriteString(`"bids":[` + strings.Join(rows, ",") + `]}`)
	return b.String()
}

// detailRow is a models-file bidDetail row; status is raw JSON.
func detailRow(bidID, status string) string {
	return fmt.Sprintf(`{"bidId":%q,"providerId":%q,"pricePerSecond":"1000","priceMorPerHour":1,"status":%s}`, bidID, prv, status)
}

func modelRow(id, name string, details ...string) string {
	return fmt.Sprintf(`{"Name":%q,"Id":%q,"bidDetail":[%s]}`, name, id, strings.Join(details, ","))
}

// modelsFile is a gateway models file; stamp is raw JSON ("" omits it).
func modelsFile(stamp string, models ...string) string {
	var b strings.Builder
	b.WriteString("{")
	if stamp != "" {
		b.WriteString(`"last_updated":` + stamp + `,`)
	}
	b.WriteString(`"models":[` + strings.Join(models, ",") + `]}`)
	return b.String()
}

func unix(t time.Time) string { return fmt.Sprint(t.Unix()) }

const healthy, degraded = `"healthy"`, `"degraded"`

// stdModels: M (detail bid3 healthy) and N, stamped with the clock.
func stdModels(clk *fakeClock, mDetail ...string) func() string {
	if len(mDetail) == 0 {
		mDetail = []string{detailRow(bid3, healthy)}
	}
	return func() string {
		return modelsFile(unix(clk.now()),
			modelRow(mdlM, "example-model", mDetail...),
			modelRow(mdlN, "example-embed", detailRow(bid4, healthy)))
	}
}

// stdBids: M bid1 healthy, M bid2 degraded, N bid4 healthy.
func stdBids(clk *fakeClock) func() string {
	return func() string {
		return bidsFile(unix(clk.now()), bidRow(bid1, mdlM, healthy), bidRow(bid2, mdlM, degraded), bidRow(bid4, mdlN, healthy))
	}
}

type harness struct {
	c      *Catalog
	models *feed
	bids   *feed
	clk    *fakeClock
}

// newHarness: both feeds good (stdModels / stdBids) on a fake clock.
func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &fakeClock{t: t0}
	h := &harness{clk: clk, models: newFeed(t, ""), bids: newFeed(t, "")}
	h.models.setFn(stdModels(clk))
	h.bids.setFn(stdBids(clk))
	h.c = New(h.models.srv.URL).WithBidsURL(h.bids.srv.URL)
	h.c.SetClock(clk.now)
	return h
}

func ids(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (h *harness) wantSet(t *testing.T, model string, want ...string) {
	t.Helper()
	got, err := h.c.HealthyBidIDs(model)
	if err != nil {
		t.Fatalf("HealthyBidIDs(%s): %v", model[:8], err)
	}
	sort.Strings(want)
	if g, w := ids(got), strings.Join(want, ","); g != w {
		t.Fatalf("HealthyBidIDs(%s) = [%s], want [%s]", model[:8], g, w)
	}
}

func (h *harness) wantUnknown(t *testing.T, model string) {
	t.Helper()
	if got, err := h.c.HealthyBidIDs(model); !errors.Is(err, ErrHealthUnknown) {
		t.Fatalf("HealthyBidIDs = %v, %v; want ErrHealthUnknown", got, err)
	}
}

func (h *harness) status(t *testing.T) (models, bids FeedState, source string) {
	t.Helper()
	st := h.c.FeedStatus()
	return st["models"].(FeedState), st["bids"].(FeedState), st["health_source"].(string)
}

// past moves the clock past the TTL and the failure pause.
func (h *harness) past() { h.clk.add(h.c.ttl + h.c.retryAfter + time.Second) }

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// logBuf captures slog output for one test.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) count(sub ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range strings.Split(l.b.String(), "\n") {
		all := line != ""
		for _, s := range sub {
			all = all && strings.Contains(line, s)
		}
		if all {
			n++
		}
	}
	return n
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *logBuf {
	t.Helper()
	lb := &logBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lb
}

func TestHealthyBidsFromBidsFile(t *testing.T) {
	h := newHarness(t)
	upper := "0x" + strings.Repeat("D1", 32)
	h.bids.set(bidsFile(unix(t0), bidRow(upper, strings.ToUpper(mdlM[:2])+strings.ToUpper(mdlM[2:]), healthy),
		bidRow(bid2, mdlM, degraded), bidRow(bid4, mdlN, healthy)))
	h.wantSet(t, mdlM, bid1) // lowercased; degraded skipped
	h.wantSet(t, "0x"+strings.ToUpper(mdlM[2:]), bid1)
	h.wantSet(t, mdlN, bid4)
	if _, _, src := h.status(t); src != "bids" {
		t.Fatalf("health_source %q", src)
	}
}

func TestHealthyBidsEmptyBidsFileDoesNotFallBack(t *testing.T) {
	h := newHarness(t)
	// Usable bids file, no healthy bid for M; bidDetail lists M healthy.
	h.bids.set(bidsFile(unix(t0), bidRow(bid2, mdlM, degraded), bidRow(bid4, mdlN, healthy)))
	h.wantSet(t, mdlM)
}

func TestHealthyBidsFallbackToBidDetail(t *testing.T) {
	h := newHarness(t)
	h.bids.fail(http.StatusInternalServerError)
	h.wantSet(t, mdlM, bid3)
	if _, _, src := h.status(t); src != "bid_detail" {
		t.Fatalf("health_source %q", src)
	}
}

func TestBidsLoadedOnceWinsOverBidDetail(t *testing.T) {
	h := newHarness(t)
	h.wantSet(t, mdlM, bid1)
	h.bids.fail(http.StatusInternalServerError)
	h.models.setFn(stdModels(h.clk, detailRow(bid2, healthy)))
	before := h.models.hits.Load()
	for i := 0; i < 12; i++ { // hours of a failing bids feed
		h.clk.add(20 * time.Minute)
		h.wantSet(t, mdlM, bid1)
	}
	if h.models.hits.Load() == before {
		t.Fatal("models feed not refreshed")
	}
	if _, _, src := h.status(t); src != "bids" {
		t.Fatalf("health_source %q", src)
	}
}

func TestBidDetailFallbackValidation(t *testing.T) {
	h := newHarness(t)
	h.bids.fail(http.StatusInternalServerError)
	h.models.set(modelsFile(unix(t0),
		modelRow(mdlM, "example-model",
			detailRow("0xd1", healthy),                        // short
			detailRow("0x"+strings.Repeat("00", 32), healthy), // zero
			detailRow("0x"+strings.Repeat("zz", 32), healthy), // non-hex
			detailRow(bid2, `null`),                           // non-string
			detailRow(bid3, degraded),                         // not healthy
			detailRow(bid4, healthy)),
		modelRow("0xc1", "short-model", detailRow(bid1, healthy)),
		modelRow("0x"+strings.Repeat("00", 32), "zero-model", detailRow(bid1, healthy)),
		modelRow("0x"+strings.Repeat("g1", 32), "nonhex-model", detailRow(bid1, healthy))))
	h.wantSet(t, mdlM, bid4)
	for _, bad := range []string{"0xc1", "0x" + strings.Repeat("00", 32), "0x" + strings.Repeat("g1", 32)} {
		h.wantSet(t, bad)
	}

}

// TestBidDetailNonStringStatusSkipped: a bidDetail row whose status is not
// a JSON string ("status": 1, true, null, {}) is skipped and counted; for
// each value the models file still loads (no failure, no rollback), the
// other rows stay usable and their healthy set is served, and List /
// Resolve return the same models as without the bad row.
func TestBidDetailNonStringStatusSkipped(t *testing.T) {
	type view struct {
		names, ids []string
		rows       []int
	}
	snapshot := func(t *testing.T, c *Catalog) view {
		t.Helper()
		l, err := c.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var v view
		for _, m := range l {
			id, err := c.Resolve(m.Name)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", m.Name, err)
			}
			v.names, v.ids = append(v.names, m.Name), append(v.ids, id)
			usable := 0
			for _, d := range m.BidDetail {
				if d.Status != "" {
					usable++
				}
			}
			v.rows = append(v.rows, usable)
		}
		return v
	}
	file := func(bad string) string {
		details := []string{detailRow(bid4, healthy), detailRow(bid2, degraded)}
		if bad != "" {
			details = append(details[:1], append([]string{detailRow(bid3, bad)}, details[1:]...)...)
		}
		return modelsFile(unix(t0),
			modelRow(mdlM, "example-model", details...),
			modelRow(mdlN, "example-embed", detailRow(bid1, healthy)))
	}
	base := newHarness(t)
	base.bids.fail(http.StatusInternalServerError)
	base.models.set(file(""))
	base.wantSet(t, mdlM, bid4)
	want := snapshot(t, base.c)
	for _, bad := range []string{`1`, `true`, `null`, `{}`} {
		t.Run(bad, func(t *testing.T) {
			lb := captureLogs(t)
			h := newHarness(t)
			h.bids.fail(http.StatusInternalServerError)
			h.models.set(file(bad))
			// Other rows usable, their healthy set served.
			h.wantSet(t, mdlM, bid4)
			h.wantSet(t, mdlN, bid1)
			// The file loaded: copy in use, no models failure or rollback logged,
			// the bad row skipped and counted.
			if m, _, src := h.status(t); !m.Loaded || m.LastGoodFetchAt == nil || *m.FeedLastUpdated != rfc(t0) || src != "bid_detail" {
				t.Fatalf("models feed not loaded: %+v %s", m, src)
			}
			if lb.count(`msg="catalog feed refreshed"`, "feed=models", "rows=4", "usable=3", "healthy=2", "skipped=1") != 1 {
				t.Fatalf("models counts not logged:\n%s", lb)
			}
			if lb.count("rollback") != 0 || lb.count("malformed") != 0 || lb.count("feed failed", "feed=models") != 0 {
				t.Fatalf("failure logged:\n%s", lb)
			}
			// List / Resolve: the same models as without the bad row.
			got := snapshot(t, h.c)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("List/Resolve = %+v, want %+v", got, want)
			}
		})
	}
}

func TestBidDetailAllNonHealthyReplacesSnapshot(t *testing.T) {
	h := newHarness(t)
	h.bids.fail(http.StatusInternalServerError)
	h.wantSet(t, mdlM, bid3)
	h.models.setFn(stdModels(h.clk, detailRow(bid3, degraded)))
	h.past()
	h.wantSet(t, mdlM)
}

func TestStaleFeedKeepsServing(t *testing.T) {
	t.Run("bids", func(t *testing.T) {
		h := newHarness(t)
		h.wantSet(t, mdlM, bid1)
		h.bids.fail(http.StatusInternalServerError)
		for _, d := range []time.Duration{31 * time.Minute, 6 * time.Hour, 3 * 24 * time.Hour} {
			h.clk.t = t0.Add(d)
			h.wantSet(t, mdlM, bid1)
			if _, b, _ := h.status(t); !b.Stale || !b.Loaded {
				t.Fatalf("+%v bids status %+v", d, b)
			}
		}
		// Frozen last_updated, 200s: same.
		h.bids.set(bidsFile(unix(t0), bidRow(bid1, mdlM, healthy)))
		h.past()
		h.wantSet(t, mdlM, bid1)
		if _, b, _ := h.status(t); !b.Stale {
			t.Fatalf("frozen bids status %+v", b)
		}
	})
	t.Run("bid_detail", func(t *testing.T) {
		h := newHarness(t)
		h.bids.fail(http.StatusInternalServerError)
		h.wantSet(t, mdlM, bid3)
		h.models.fail(http.StatusInternalServerError)
		for _, d := range []time.Duration{31 * time.Minute, 6 * time.Hour, 3 * 24 * time.Hour} {
			h.clk.t = t0.Add(d)
			h.wantSet(t, mdlM, bid3)
			if m, _, src := h.status(t); !m.Stale || !m.Loaded || src != "bid_detail" {
				t.Fatalf("+%v models status %+v %s", d, m, src)
			}
		}
	})
}

func TestNeverLoadedRefuses(t *testing.T) {
	h := newHarness(t)
	h.bids.fail(http.StatusInternalServerError)
	h.models.fail(http.StatusInternalServerError)
	h.wantUnknown(t, mdlM)
	m, b, src := h.status(t)
	if src != "none" || m.Loaded || b.Loaded || m.AgeSeconds != nil || b.AgeSeconds != nil || m.Stale || b.Stale {
		t.Fatalf("status %+v %+v %s", m, b, src)
	}
	// A models copy that loads the list but carries no usable health
	// (no last_updated) still refuses.
	h.models.set(modelsFile("", modelRow(mdlM, "example-model", detailRow(bid3, healthy))))
	h.past()
	if _, err := h.c.List(); err != nil {
		t.Fatal(err)
	}
	h.wantUnknown(t, mdlM)
}

func TestFeedLastUpdatedFrozenMarksStale(t *testing.T) {
	h := newHarness(t)
	h.bids.set(bidsFile(unix(t0), bidRow(bid1, mdlM, healthy)))
	h.wantSet(t, mdlM, bid1)
	h.clk.add(31 * time.Minute)
	before := h.bids.hits.Load()
	h.wantSet(t, mdlM, bid1)
	if h.bids.hits.Load() == before {
		t.Fatal("bids not refetched")
	}
	_, b, _ := h.status(t)
	if !b.Stale || b.AgeSeconds == nil || *b.AgeSeconds != 31*60 || *b.LastGoodFetchAt != rfc(h.clk.now()) || *b.FeedLastUpdated != rfc(t0) {
		t.Fatalf("bids status %+v age=%v", b, *b.AgeSeconds)
	}
}

func TestFeedLastUpdatedFutureSkew(t *testing.T) {
	for _, tc := range []struct {
		skew time.Duration
		ok   bool
	}{{4 * time.Minute, true}, {6 * time.Minute, false}} {
		h := newHarness(t)
		h.bids.set(bidsFile(unix(t0.Add(tc.skew)), bidRow(bid1, mdlM, healthy)))
		if _, b, _ := h.status(t); b.Loaded {
			t.Fatal("loaded before use")
		}
		h.wantSet(t, mdlM, map[bool]string{true: bid1, false: bid3}[tc.ok])
		if _, b, _ := h.status(t); b.Loaded != tc.ok {
			t.Fatalf("bids +%v loaded=%v", tc.skew, b.Loaded)
		}

		h2 := newHarness(t)
		h2.bids.fail(http.StatusInternalServerError)
		h2.models.set(modelsFile(unix(t0.Add(tc.skew)), modelRow(mdlM, "example-model", detailRow(bid3, healthy))))
		if tc.ok {
			h2.wantSet(t, mdlM, bid3)
		} else {
			h2.wantUnknown(t, mdlM)
		}
	}
}

func TestFeedLastUpdatedMissingIsFailure(t *testing.T) {
	for _, stamp := range []string{"", `"1800000000"`, `1800000000.5`, `null`} {
		h := newHarness(t)
		h.bids.set(bidsFile(stamp, bidRow(bid1, mdlM, healthy)))
		h.wantSet(t, mdlM, bid3) // bids never loaded: bidDetail
		if _, b, _ := h.status(t); b.Loaded {
			t.Fatalf("bids stamp %q loaded", stamp)
		}
		h2 := newHarness(t)
		h2.bids.fail(http.StatusInternalServerError)
		h2.models.set(modelsFile(stamp, modelRow(mdlM, "example-model", detailRow(bid3, healthy))))
		h2.wantUnknown(t, mdlM)
	}
}

func TestFeedOlderLastUpdatedDoesNotReplace(t *testing.T) {
	T := t0
	t.Run("bids", func(t *testing.T) {
		lb := captureLogs(t)
		h := newHarness(t)
		h.bids.set(bidsFile(unix(T), bidRow(bid1, mdlM, healthy)))
		h.wantSet(t, mdlM, bid1)
		// B: older, different set.
		h.bids.set(bidsFile(unix(T.Add(-time.Hour)), bidRow(bid2, mdlM, healthy)))
		h.past()
		h.wantSet(t, mdlM, bid1)
		if lb.count("feed=bids", "error=rollback") != 1 {
			t.Fatalf("no rollback line:\n%s", lb)
		}
		_, b, _ := h.status(t)
		if *b.LastGoodFetchAt != rfc(T) || *b.AgeSeconds != int64(h.clk.now().Sub(T)/time.Second) {
			t.Fatalf("fetchedAt refreshed by a rollback: %+v", b)
		}
		// C: same stamp, different set: A's set is kept and only the fetch
		// time (last_good_fetch_at) is refreshed; age_seconds does not drop,
		// because it is measured from the older of the fetch time and
		// last_updated.
		_, bPrev, _ := h.status(t)
		h.bids.set(bidsFile(unix(T), bidRow(bid3, mdlM, healthy)))
		h.past()
		h.wantSet(t, mdlM, bid1)
		if _, b, _ := h.status(t); *b.LastGoodFetchAt != rfc(h.clk.now()) || *b.FeedLastUpdated != rfc(T) ||
			*b.AgeSeconds < *bPrev.AgeSeconds || *b.AgeSeconds != int64(h.clk.now().Sub(T)/time.Second) {
			t.Fatalf("equal stamp: %+v", b)
		}
		// D: newer replaces.
		h.bids.set(bidsFile(unix(T.Add(5*time.Minute)), bidRow(bid4, mdlM, healthy)))
		h.past()
		h.wantSet(t, mdlM, bid4)
	})
	t.Run("models", func(t *testing.T) {
		lb := captureLogs(t)
		h := newHarness(t)
		h.bids.fail(http.StatusInternalServerError)
		h.models.set(modelsFile(unix(T), modelRow(mdlM, "example-model", detailRow(bid1, healthy))))
		h.wantSet(t, mdlM, bid1)
		h.models.set(modelsFile(unix(T.Add(-time.Hour)), modelRow(mdlM, "example-model", detailRow(bid2, healthy))))
		h.past()
		h.wantSet(t, mdlM, bid1)
		if lb.count("feed=models", "error=rollback") != 1 {
			t.Fatalf("no rollback line:\n%s", lb)
		}
		m, _, src := h.status(t)
		if src != "bid_detail" || *m.LastGoodFetchAt != rfc(T) || *m.FeedLastUpdated != rfc(T) {
			t.Fatalf("models status from detailSnap: %+v %s", m, src)
		}
		// C: same stamp: only last_good_fetch_at moves; age_seconds does
		// not drop.
		mPrev, _, _ := h.status(t)
		h.models.set(modelsFile(unix(T), modelRow(mdlM, "example-model", detailRow(bid3, healthy))))
		h.past()
		h.wantSet(t, mdlM, bid1)
		if m, _, _ := h.status(t); *m.LastGoodFetchAt != rfc(h.clk.now()) || *m.AgeSeconds < *mPrev.AgeSeconds {
			t.Fatalf("equal stamp: %+v", m)
		}
		h.models.set(modelsFile(unix(T.Add(5*time.Minute)), modelRow(mdlM, "example-model", detailRow(bid4, healthy))))
		h.past()
		h.wantSet(t, mdlM, bid4)
	})
}

func TestBidDetailSnapshotNotRefreshedWithoutTimestamp(t *testing.T) {
	h := newHarness(t)
	h.bids.fail(http.StatusInternalServerError)
	h.models.set(modelsFile(unix(t0), modelRow(mdlM, "example-model", detailRow(bid1, healthy))))
	h.wantSet(t, mdlM, bid1)
	h.models.set(modelsFile("", modelRow(mdlM, "example-model", detailRow(bid2, healthy))))
	for h.clk.now().Sub(t0) <= 31*time.Minute {
		h.past()
		h.wantSet(t, mdlM, bid1)
		m, _, _ := h.status(t)
		if *m.LastGoodFetchAt != rfc(t0) || *m.FeedLastUpdated != rfc(t0) || *m.AgeSeconds != int64(h.clk.now().Sub(t0)/time.Second) {
			t.Fatalf("snapshot re-dated: %+v", m)
		}
	}
	if m, _, _ := h.status(t); !m.Stale {
		t.Fatalf("not stale at +31m: %+v", m)
	}
}

func TestStaleWarningRateLimited(t *testing.T) {
	lb := captureLogs(t)
	h := newHarness(t)
	h.wantSet(t, mdlM, bid1)
	h.bids.fail(http.StatusInternalServerError)
	stale := func() int { return lb.count(`msg="catalog feed stale"`, "feed=bids") }
	h.clk.add(31 * time.Minute)
	for i := 0; i < 20; i++ { // many opens within 10 min
		h.wantSet(t, mdlM, bid1)
		h.clk.add(20 * time.Second)
	}
	if n := stale(); n != 1 {
		t.Fatalf("stale warns within 10 min = %d, want 1", n)
	}
	h.clk.add(4 * time.Minute)
	h.wantSet(t, mdlM, bid1)
	h.wantSet(t, mdlM, bid1)
	if n := stale(); n != 2 {
		t.Fatalf("stale warns after 10 min = %d, want 2", n)
	}
	h.bids.setFn(stdBids(h.clk))
	h.past()
	h.wantSet(t, mdlM, bid1)
	h.wantSet(t, mdlM, bid1)
	if n := lb.count(`msg="catalog feed fresh again"`, "feed=bids"); n != 1 {
		t.Fatalf("recovery lines = %d, want 1", n)
	}
	if n := lb.count(`msg="catalog feed stale"`, "feed=models"); n != 0 {
		t.Fatalf("models (fresh) warned stale %d times", n)
	}

	// Rollback lines: the first, then one per 10 min with the count.
	h.bids.set(bidsFile(unix(t0), bidRow(bid1, mdlM, healthy)))
	rb := func() int { return lb.count("error=rollback", "feed=bids") }
	h.clk.add(h.c.ttl)
	for i := 0; i < 5; i++ {
		h.clk.add(h.c.retryAfter + time.Second)
		h.wantSet(t, mdlM, bid1)
	}
	if n := rb(); n != 1 {
		t.Fatalf("rollback lines = %d, want 1", n)
	}
	h.clk.add(10 * time.Minute)
	h.wantSet(t, mdlM, bid1)
	if n := rb(); n != 2 || lb.count("error=rollback", "feed=bids", "suppressed=4") != 1 {
		t.Fatalf("rollback lines = %d:\n%s", n, lb)
	}
}

func TestFeedStatusNeverFetches(t *testing.T) {
	h := newHarness(t)
	h.wantSet(t, mdlM, bid1)
	h.clk.add(time.Hour)
	m0, b0 := h.models.hits.Load(), h.bids.hits.Load()
	for i := 0; i < 10; i++ {
		_ = h.c.FeedStatus()
	}
	if h.models.hits.Load() != m0 || h.bids.hits.Load() != b0 {
		t.Fatal("FeedStatus fetched")
	}
}

func TestHealthyBidsMalformedKeepsLastGood(t *testing.T) {
	h := newHarness(t)
	h.wantSet(t, mdlM, bid1)
	now := func() string { return unix(h.clk.now()) }
	for _, body := range []func() string{
		func() string { return `{}` },
		func() string { return `{"last_updated":` + now() + `,"bids":{}}` },
		func() string { return `{"last_updated":` + now() + `,"bids":[` + bidRow(bid2, mdlM, healthy) },
		func() string { return bidsFile(now(), `{"Id":"0x12"}`, `{"Id":`+fmt.Sprintf("%q", bid2)+`}`) },
		func() string { return bidsFile(now()) },
	} {
		h.bids.setFn(body)
		h.past()
		h.wantSet(t, mdlM, bid1)
	}

	h2 := newHarness(t)
	h2.bids.fail(http.StatusInternalServerError)
	h2.wantSet(t, mdlM, bid3)
	h2.models.setFn(func() string {
		return modelsFile(unix(h2.clk.now()), modelRow(mdlM, "example-model", detailRow("0x12", healthy)))
	})
	h2.past()
	h2.wantSet(t, mdlM, bid3)
}

func TestFeedSizeCap(t *testing.T) {
	pad := strings.Repeat(" ", maxFeedBytes+1024)
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	bigBids := `{"last_updated":` + unix(t0) + `,` + pad + `"bids":[` + bidRow(bid1, mdlM, healthy) + `]}`
	bigModels := `{"last_updated":` + unix(t0) + `,` + pad + `"models":[` + modelRow(mdlM, "example-model", detailRow(bid3, healthy)) + `]}`
	if gzBomb := gz(bigBids); len(gzBomb) > 1<<20 {
		t.Fatalf("gzip bomb not small: %d", len(gzBomb))
	}
	gzHdr := http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"application/json"}}
	for _, enc := range []string{"plain", "gzip"} {
		h := newHarness(t)
		if enc == "plain" {
			h.bids.setRaw([]byte(bigBids), nil)
		} else {
			h.bids.setRaw(gz(bigBids), gzHdr)
		}
		h.wantSet(t, mdlM, bid3)
		if _, b, _ := h.status(t); b.Loaded {
			t.Fatalf("%s oversize bids loaded", enc)
		}

		h2 := newHarness(t)
		h2.bids.fail(http.StatusInternalServerError)
		if enc == "plain" {
			h2.models.setRaw([]byte(bigModels), nil)
		} else {
			h2.models.setRaw(gz(bigModels), gzHdr)
		}
		if _, err := h2.c.List(); err == nil {
			t.Fatalf("%s oversize models loaded", enc)
		}
		h2.wantUnknown(t, mdlM)
	}
	// Under the cap, gzip is fine.
	h := newHarness(t)
	h.bids.setRaw(gz(bidsFile(unix(t0), bidRow(bid1, mdlM, healthy))), gzHdr)
	h.wantSet(t, mdlM, bid1)
}

func TestFeedRedirectIsFailure(t *testing.T) {
	target := newFeed(t, "")
	clkTarget := &fakeClock{t: t0}
	target.setFn(stdBids(clkTarget))
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect} {
		h := newHarness(t)
		h.wantSet(t, mdlM, bid1)
		h.bids.redirect(code, target.srv.URL)
		h.models.redirect(code, target.srv.URL)
		h.past()
		h.wantSet(t, mdlM, bid1)
		if _, err := h.c.List(); err != nil {
			t.Fatalf("%d: list lost: %v", code, err)
		}
	}
	// https -> http.
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusFound)
	}))
	defer tls.Close()
	h := newHarness(t)
	h.c.client.Transport = tls.Client().Transport
	h.c.bidsURL = tls.URL
	h.wantSet(t, mdlM, bid3)
	if target.hits.Load() != 0 {
		t.Fatalf("redirect followed %d times", target.hits.Load())
	}
}

func TestHealthyBidsRowValidation(t *testing.T) {
	h := newHarness(t)
	zero := "0x" + strings.Repeat("00", 32)
	h.bids.set(bidsFile(unix(t0),
		`5`, `"row"`, `null`,
		fmt.Sprintf(`{"Id":%q,"ModelAgentId":%q}`, bid2, mdlM), // no health
		fmt.Sprintf(`{"Id":%q,"ModelAgentId":%q,"health":null}`, bid2, mdlM),
		bidRow("0xd2", mdlM, healthy),
		bidRow(zero, mdlM, healthy),
		bidRow("0x"+strings.Repeat("x2", 32), mdlM, healthy),
		bidRow(bid2+"00", mdlM, healthy),
		bidRow(bid2, "0xc1", healthy),
		bidRow(bid2, zero, healthy),
		fmt.Sprintf(`{"Id":%q,"ModelAgentId":%q,"health":{"status":"healthy"}}`, bid1, mdlM), // no Provider: fine
	))
	h.wantSet(t, mdlM, bid1)
	h.wantSet(t, zero)
}

// TestHealthyBidsNonStringStatusSkipped: a bids row whose health.status is
// not a JSON string ("status": 1, true, null, {}) is skipped and counted;
// for each value the bids file still loads (no failure, no rollback), the
// other rows stay usable and their healthy set is served, and List /
// Resolve return the same models as without the bad row. A file with only
// non-string statuses has no usable row and is a failed file.
func TestHealthyBidsNonStringStatusSkipped(t *testing.T) {
	type view struct{ names, ids []string }
	snapshot := func(t *testing.T, c *Catalog) view {
		t.Helper()
		l, err := c.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var v view
		for _, m := range l {
			id, err := c.Resolve(m.Name)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", m.Name, err)
			}
			v.names, v.ids = append(v.names, m.Name), append(v.ids, id)
		}
		return v
	}
	file := func(bad string) string {
		rows := []string{bidRow(bid1, mdlM, healthy), bidRow(bid4, mdlN, healthy), bidRow(bid3, mdlN, degraded)}
		if bad != "" {
			rows = append(rows, bidRow(bid2, mdlM, bad))
		}
		return bidsFile(unix(t0), rows...)
	}
	base := newHarness(t)
	base.bids.set(file(""))
	base.wantSet(t, mdlM, bid1)
	want := snapshot(t, base.c)
	for _, bad := range []string{`1`, `true`, `null`, `{}`} {
		t.Run(bad, func(t *testing.T) {
			lb := captureLogs(t)
			h := newHarness(t)
			h.bids.set(file(bad))
			// Other rows usable, their healthy set served.
			h.wantSet(t, mdlM, bid1)
			h.wantSet(t, mdlN, bid4)
			// The file loaded: bids copy in use, no bids failure or
			// rollback logged, the bad row skipped and counted.
			if _, b, src := h.status(t); !b.Loaded || b.LastGoodFetchAt == nil || *b.FeedLastUpdated != rfc(t0) || src != "bids" {
				t.Fatalf("bids feed not loaded: %+v %s", b, src)
			}
			if lb.count(`msg="catalog feed refreshed"`, "feed=bids", "rows=4", "usable=3", "healthy=2", "skipped=1") != 1 {
				t.Fatalf("bids counts not logged:\n%s", lb)
			}
			if lb.count("rollback") != 0 || lb.count("malformed") != 0 || lb.count("feed failed", "feed=bids") != 0 {
				t.Fatalf("failure logged:\n%s", lb)
			}
			// List / Resolve: the same models as without the bad row.
			if got := snapshot(t, h.c); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("List/Resolve = %+v, want %+v", got, want)
			}
		})
	}

	// Only non-string statuses: no usable row, a failed file.
	h2 := newHarness(t)
	h2.bids.set(bidsFile(unix(t0), bidRow(bid2, mdlM, `1`), bidRow(bid3, mdlM, `true`), bidRow(bid4, mdlM, `null`), bidRow(bid1, mdlN, `{}`)))
	h2.wantSet(t, mdlM, bid3)
	if _, b, _ := h2.status(t); b.Loaded {
		t.Fatal("non-string-only bids file loaded")
	}
}

// TestJSONStringOnlyStrings: the shared status helper accepts only JSON
// strings; null (which json.Unmarshal into a string accepts without an
// error), numbers, booleans, objects, arrays and empty input are not
// usable.
func TestJSONStringOnlyStrings(t *testing.T) {
	for raw, want := range map[string]bool{
		`"healthy"`: true, ` "x" `: true, `""`: true,
		`null`: false, ` null`: false, `1`: false, `true`: false, `false`: false, `{}`: false, `["healthy"]`: false, ``: false, `"unterminated`: false,
	} {
		if _, ok := jsonString(json.RawMessage(raw)); ok != want {
			t.Errorf("jsonString(%q) ok = %v, want %v", raw, ok, want)
		}
	}
}

func TestFeedFetchPausesAfterFailure(t *testing.T) {
	type failure struct {
		name  string
		apply func(h *harness, f *feed, good func(time.Time) string)
	}
	failures := []failure{
		{"http_status", func(h *harness, f *feed, _ func(time.Time) string) { f.fail(http.StatusBadGateway) }},
		{"redirect", func(h *harness, f *feed, _ func(time.Time) string) {
			f.redirect(http.StatusFound, "http://127.0.0.1:1/")
		}},
		{"oversize", func(h *harness, f *feed, _ func(time.Time) string) {
			f.setRaw([]byte(strings.Repeat(" ", maxFeedBytes+1)), nil)
		}},
		{"malformed", func(h *harness, f *feed, _ func(time.Time) string) { f.set(`{"bids":`) }},
		{"bad_timestamp", func(h *harness, f *feed, good func(time.Time) string) {
			f.set(strings.Replace(good(h.clk.now()), `"last_updated":`, `"last_updated":"x","was":`, 1))
		}},
		{"rollback", func(h *harness, f *feed, good func(time.Time) string) {
			f.set(good(h.clk.now().Add(-time.Hour)))
		}},
	}
	t.Run("bids", func(t *testing.T) {
		good := func(at time.Time) string { return bidsFile(unix(at), bidRow(bid1, mdlM, healthy)) }
		fails := append(failures, failure{"no_usable_rows", func(h *harness, f *feed, _ func(time.Time) string) {
			f.set(bidsFile(unix(h.clk.now()), bidRow("0x1", mdlM, healthy)))
		}})
		for _, fl := range fails {
			lb := captureLogs(t)
			h := newHarness(t)
			h.bids.set(good(t0))
			h.wantSet(t, mdlM, bid1)
			h.past()
			fl.apply(h, h.bids, good)
			n0 := h.bids.hits.Load()
			for i := 0; i < 20; i++ {
				h.wantSet(t, mdlM, bid1)
				h.clk.add(400 * time.Millisecond) // 8 s in all
			}
			if got := h.bids.hits.Load() - n0; got != 1 {
				t.Fatalf("%s: bids fetches within the pause = %d, want 1", fl.name, got)
			}
			if lb.count("feed=bids", "error="+fl.name) != 1 {
				t.Fatalf("%s: failure class not logged:\n%s", fl.name, lb)
			}
			h.clk.add(3 * time.Second)
			h.wantSet(t, mdlM, bid1)
			if got := h.bids.hits.Load() - n0; got != 2 {
				t.Fatalf("%s: fetches after the pause = %d, want 2", fl.name, got)
			}
		}
	})
	t.Run("models", func(t *testing.T) {
		good := func(at time.Time) string {
			return modelsFile(unix(at), modelRow(mdlM, "example-model", detailRow(bid3, healthy)))
		}
		fails := append(failures, failure{"no_usable_rows", func(h *harness, f *feed, _ func(time.Time) string) {
			f.set(modelsFile(unix(h.clk.now()), modelRow(mdlM, "example-model", detailRow("0x1", healthy))))
		}})
		for _, fl := range fails {
			lb := captureLogs(t)
			h := newHarness(t)
			h.bids.fail(http.StatusInternalServerError)
			h.models.set(good(t0))
			h.wantSet(t, mdlM, bid3)
			h.past()
			fl.apply(h, h.models, good)
			n0 := h.models.hits.Load()
			for i := 0; i < 20; i++ {
				h.wantSet(t, mdlM, bid3)
				if id, err := h.c.Resolve("example-model"); err != nil || id != mdlM {
					t.Fatalf("%s: Resolve = %q, %v", fl.name, id, err)
				}
				if l, err := h.c.List(); err != nil || len(l) != 1 {
					t.Fatalf("%s: List = %v, %v", fl.name, l, err)
				}
				h.clk.add(400 * time.Millisecond)
			}
			if got := h.models.hits.Load() - n0; got != 1 {
				t.Fatalf("%s: models fetches within the pause = %d, want 1", fl.name, got)
			}
			if lb.count("feed=models", "error="+fl.name) != 1 {
				t.Fatalf("%s: failure class not logged:\n%s", fl.name, lb)
			}
			h.clk.add(3 * time.Second)
			h.wantSet(t, mdlM, bid3)
			// A failed list fetch retries after the pause; a list that
			// loaded (bidDetail-only failure) waits for its TTL.
			want := int32(2)
			if fl.name == "bad_timestamp" || fl.name == "rollback" || fl.name == "no_usable_rows" {
				want = 1
				h.clk.add(h.c.ttl)
				h.wantSet(t, mdlM, bid3)
				want = 2
			}
			if got := h.models.hits.Load() - n0; got != want {
				t.Fatalf("%s: fetches after the pause = %d, want %d", fl.name, got, want)
			}
		}
	})
}

// The bids fetch runs outside the catalog lock: the lock stays free, a
// caller with a health copy is served at once, a caller with none waits
// for the running fetch instead of starting another.
func TestFetchOutsideLockCoversBids(t *testing.T) {
	h := newHarness(t)
	h.models.set(modelsFile("", modelRow(mdlM, "example-model", detailRow(bid3, healthy)))) // no health from models
	release := make(chan struct{})
	h.bids.gate.Store(&release)
	done := make(chan string, 2)
	call := func() {
		got, err := h.c.HealthyBidIDs(mdlM)
		done <- fmt.Sprint(ids(got), err)
	}
	go call()
	for h.bids.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	go call()
	free := make(chan struct{})
	go func() { _ = h.c.FeedStatus(); _ = h.c.Loaded(); close(free) }()
	select {
	case <-free:
	case <-time.After(2 * time.Second):
		t.Fatal("catalog lock held during the bids fetch")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		if got := <-done; got != bid1+"<nil>" {
			t.Fatalf("waiter %d: %s", i, got)
		}
	}
	if n := h.bids.hits.Load(); n != 1 {
		t.Fatalf("bids fetches = %d, want 1", n)
	}
	// A loaded copy is served at once while a refresh hangs.
	rel2 := make(chan struct{})
	h.bids.gate.Store(&rel2)
	h.clk.add(h.c.ttl + time.Second)
	go func() { _, _ = h.c.HealthyBidIDs(mdlM) }()
	for h.bids.hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	got := make(chan string, 1)
	go func() { s, _ := h.c.HealthyBidIDs(mdlM); got <- ids(s) }()
	select {
	case s := <-got:
		if s != bid1 {
			t.Fatalf("stale lookup %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lookup blocked behind a running bids refresh")
	}
	close(rel2)
}

func TestHealthyBidsConcurrentRefresh(t *testing.T) {
	h := newHarness(t)
	h.c.SetRetryAfter(0)
	var flip atomic.Int32
	h.bids.setFn(func() string {
		if flip.Add(1)%3 == 0 {
			return "{"
		}
		return bidsFile(unix(h.clk.now()), bidRow(bid1, mdlM, healthy), bidRow(bid2, mdlM, healthy))
	})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				switch (g + i) % 4 {
				case 0:
					h.clk.add(2 * time.Minute)
				case 1:
					_ = h.c.FeedStatus()
				case 2:
					_, _ = h.c.List()
				}
				set, err := h.c.HealthyBidIDs(mdlM)
				if err != nil {
					t.Error(err)
					return
				}
				for id := range set { // our copy: safe to mutate
					delete(set, id)
				}
			}
		}(g)
	}
	wg.Wait()
	h.wantSet(t, mdlM, bid1, bid2)
}

func TestListNotFilteredByHealth(t *testing.T) {
	h := newHarness(t)
	h.bids.set(bidsFile(unix(t0), bidRow(bid4, mdlN, healthy), bidRow(bid1, mdlM, degraded)))
	h.wantSet(t, mdlM)
	l, err := h.c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[0].ID != mdlM || len(l[0].BidDetail) != 1 || l[0].BidDetail[0].BidID != bid3 {
		t.Fatalf("List = %+v", l)
	}
	if id, err := h.c.Resolve("example-model"); err != nil || id != mdlM {
		t.Fatalf("Resolve = %q, %v", id, err)
	}
}
