package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

// No healthy bid for the model → 503 no_available_provider, no ids, no
// open attempts of any kind.
func TestNoHealthyBid503NoAvailable(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	m := newFakeMarket(t, a40, b40)
	m.unhealthy[a40], m.unhealthy[b40] = true, true
	e := newFailoverEnv(t, m, nil)
	code, hdr, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, `"type":"no_available_provider"`) || strings.Contains(body, "0x") {
		t.Fatalf("got %d %s", code, body)
	}
	if ra := hdr.Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After %q", ra)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("opens %+v", opens)
	}
}

// Every rated provider excluded → 503 no_eligible_provider, no opens.
func TestFullyExcluded503NoEligible(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	m := newFakeMarket(t, a40, b40)
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.SetProviderExclude([]string{a40, mixedCase(b40)})
	code, _, body := chatFull(t, e, "test-model")
	if code != 503 || !strings.Contains(body, `"type":"no_eligible_provider"`) || strings.Contains(body, "0x") {
		t.Fatalf("got %d %s", code, body)
	}
	if opens, _ := m.snapshot(); len(opens) != 0 {
		t.Fatalf("opens %+v", opens)
	}
}

// fakeClock is a settable clock for the catalog.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type statusFeeds struct {
	Feeds *struct {
		Models struct {
			LastGoodFetchAt *string `json:"last_good_fetch_at"`
			FeedLastUpdated *string `json:"feed_last_updated"`
			AgeSeconds      *int64  `json:"age_seconds"`
			Stale           bool    `json:"stale"`
			Loaded          bool    `json:"loaded"`
		} `json:"models"`
		Bids struct {
			LastGoodFetchAt *string `json:"last_good_fetch_at"`
			FeedLastUpdated *string `json:"feed_last_updated"`
			AgeSeconds      *int64  `json:"age_seconds"`
			Stale           bool    `json:"stale"`
			Loaded          bool    `json:"loaded"`
		} `json:"bids"`
		HealthSource string `json:"health_source"`
	} `json:"feeds"`
}

func getStatusFeeds(t *testing.T, c *http.Client, base string) statusFeeds {
	t.Helper()
	resp, err := c.Get(base + "/admin/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out statusFeeds
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Feeds == nil {
		t.Fatal("status has no feeds")
	}
	return out
}

// /admin/status shows each feed's age and turns stale after 30 minutes;
// a never-loaded feed shows loaded:false with null times; a models
// rollback keeps the age growing (ages come from the last good health
// snapshot) while requests keep being served from the last good copy.
func TestStatusShowsFeedAgeAndStale(t *testing.T) {
	clk := &fakeClock{t: time.Now().Truncate(time.Second)}
	var stamp atomic.Int64
	a40 := testProv
	row := feedRow{model: testModelID, bid: bidOf(a40), prov: a40, status: "healthy"}
	stamp.Store(clk.Now().Unix())
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(modelsFeed(stamp.Load(), "test-model", row))
	}))
	t.Cleanup(models.Close)
	m := newFakeMarket(t, a40)
	e := newFailoverEnv(t, m, func(c *config.Config) {
		c.ActiveModelsURL = models.URL
		c.GatewayBidsURL = models.URL + "/missing-bids" // never loads: same body, wrong shape
	})
	e.srv.catalog.SetClock(clk.Now)
	ac := adminClient(t, e.ts, &config.Config{AdminPassword: "adminpass"})

	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
	st := getStatusFeeds(t, ac, e.ts.URL)
	f := st.Feeds
	if !f.Models.Loaded || f.Models.Stale || f.Models.AgeSeconds == nil || *f.Models.AgeSeconds != 0 ||
		f.Models.LastGoodFetchAt == nil || f.Models.FeedLastUpdated == nil {
		t.Fatalf("models fresh: %+v", f.Models)
	}
	if f.Bids.Loaded || f.Bids.Stale || f.Bids.AgeSeconds != nil || f.Bids.LastGoodFetchAt != nil || f.Bids.FeedLastUpdated != nil {
		t.Fatalf("bids never loaded: %+v", f.Bids)
	}
	if f.HealthSource != "bid_detail" {
		t.Fatalf("health_source %q", f.HealthSource)
	}

	// Rollback: the list refreshes but health keeps the older snapshot.
	clk.Add(6 * time.Minute)
	stamp.Store(clk.Now().Unix() - 3600)
	e.srv.catalog.List()
	if got := getStatusFeeds(t, ac, e.ts.URL).Feeds.Models; got.Stale || got.AgeSeconds == nil || *got.AgeSeconds != 360 {
		t.Fatalf("after rollback: %+v", got)
	}
	clk.Add(25*time.Minute + time.Second) // 31m01s since the last good snapshot
	e.srv.catalog.List()
	got := getStatusFeeds(t, ac, e.ts.URL).Feeds.Models
	if !got.Stale || !got.Loaded || got.AgeSeconds == nil || *got.AgeSeconds != 31*60+1 {
		t.Fatalf("stale: %+v", got)
	}
	// Stale is visible, never a refusal: the request is still served.
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("stale chat: %d %s", code, body)
	}
}

// Feed status is part of /admin/status only, behind the admin session.
func TestStatusFeedsAdminOnly(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	for _, auth := range []string{"", "Bearer " + "sk-not-a-key"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/status", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || strings.Contains(string(raw), `"feeds"`) {
			t.Fatalf("unauthenticated status %d: %s", resp.StatusCode, raw)
		}
	}
	st := getStatusFeeds(t, adminClient(t, ts, cfg), ts.URL)
	if st.Feeds.HealthSource == "" {
		t.Fatal("admin status: empty health_source")
	}
}
