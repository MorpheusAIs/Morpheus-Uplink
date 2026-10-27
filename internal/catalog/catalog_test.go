package catalog

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	cheapID  = "0xc2c4b037ff12e0aa81178deac52aeed902b36189b9e6feae22b72324c9221130"
	priceyID = "0xe79d9b0e5022c58535cf4373bd4b74ed4ca04dea76772b845bbe87a18c529e9c"
)

func bid(status, pps string) BidDetail {
	return BidDetail{Status: status, PricePerSecond: pps}
}

func twin(id, name string, bids ...BidDetail) Model {
	return Model{ID: id, Name: name, BidDetail: bids}
}

func loaded(models ...Model) *Catalog {
	c := New("http://catalog.invalid/models.json")
	c.models = append([]Model(nil), models...)
	c.byName, c.byID = indexModels(c.models)
	c.fetchedAt = time.Now()
	return c
}

func TestResolveUniqueNameAndRawID(t *testing.T) {
	c := loaded(twin(cheapID, "llama-3.3-70b", bid("healthy", "100")))
	id, err := c.Resolve("Llama-3.3-70b")
	if err != nil || id != cheapID {
		t.Fatalf("Resolve name: %q %v", id, err)
	}
	raw, err := c.Resolve("0xexplicit")
	if err != nil || raw != "0xexplicit" {
		t.Fatalf("Resolve raw id: %q %v", raw, err)
	}
	if _, err := c.Resolve("missing"); err == nil {
		t.Fatal("missing name should error")
	}
}

func TestResolveDuplicateNamePicksCheapestHealthy(t *testing.T) {
	cheap := twin(cheapID, "deepseek-v4-flash", bid("healthy", "104"))
	pricey := twin(priceyID, "deepseek-v4-flash", bid("Healthy", "950"))
	for _, order := range [][]Model{{pricey, cheap}, {cheap, pricey}} {
		c := loaded(order...)
		id, err := c.Resolve("deepseek-v4-flash")
		if err != nil {
			t.Fatal(err)
		}
		if id != cheapID {
			t.Fatalf("order %s then %s: got %s, want cheap twin", order[0].ID[:6], order[1].ID[:6], id)
		}
	}
}

func TestResolveHealthyBeatsCheaperUnhealthy(t *testing.T) {
	unhealthyCheap := twin(cheapID, "glm-5.2", bid("failed", "10"))
	healthyPricey := twin(priceyID, "glm-5.2", bid("healthy", "900"))
	c := loaded(unhealthyCheap, healthyPricey)
	id, err := c.Resolve("glm-5.2")
	if err != nil || id != priceyID {
		t.Fatalf("got %s %v, want healthy twin", id, err)
	}
}

func TestResolveUnhealthyFallsBackToCheapest(t *testing.T) {
	c := loaded(
		twin(priceyID, "gpt-5.4", bid("failed", "200")),
		twin(cheapID, "gpt-5.4", bid("failed", "50")),
	)
	id, err := c.Resolve("gpt-5.4")
	if err != nil || id != cheapID {
		t.Fatalf("got %s %v, want cheaper unhealthy twin", id, err)
	}
}

func TestResolveEqualPriceTieBreaksByID(t *testing.T) {
	// cheapID sorts before priceyID.
	c := loaded(
		twin(priceyID, "same", bid("healthy", "10")),
		twin(cheapID, "same", bid("healthy", "10")),
	)
	id, err := c.Resolve("same")
	if err != nil || id != cheapID {
		t.Fatalf("got %s %v, want lexicographically lower id", id, err)
	}
	if !strings.EqualFold(cheapID, c.byName["same"]) {
		t.Fatalf("byName = %s", c.byName["same"])
	}
}

func TestSendableID(t *testing.T) {
	m := twin(cheapID, "deepseek-v4-flash")
	if got := m.SendableID(false); got != "deepseek-v4-flash" {
		t.Fatalf("unique name: %s", got)
	}
	if got := m.SendableID(true); got != cheapID {
		t.Fatalf("shared name: %s", got)
	}
}

// Security gate SHOULD-2 (c)(d): a failed fetch starts a pause in which
// lookups make no new fetch (no fetch storm while the feed is down and
// nothing has ever loaded); after the pause one fetch is tried again.
func TestFailedFetchPausesRefetch(t *testing.T) {
	var fetches atomic.Int32
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"Name":"m","Id":"0xabc"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.SetRetryAfter(200 * time.Millisecond)
	for i := 0; i < 30; i++ {
		if n := c.NameByID("0xabc"); n != "" {
			t.Fatalf("name while down: %q", n)
		}
		if _, err := c.Resolve("m"); err == nil {
			t.Fatal("resolve while down succeeded")
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches while down within the pause = %d, want 1", got)
	}
	if c.Loaded() {
		t.Fatal("Loaded with no data")
	}
	up.Store(true)
	time.Sleep(250 * time.Millisecond)
	if n := c.NameByID("0xabc"); n != "m" || !c.Loaded() {
		t.Fatalf("after the pause: %q loaded=%v", n, c.Loaded())
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

// Security gate SHOULD-2 (c): the catalog lock is not held during a
// fetch, a caller with no data waits for the running fetch instead of
// starting another, and stale data is served at once while a refresh runs.
func TestFetchRunsOutsideLock(t *testing.T) {
	var fetches atomic.Int32
	var gate atomic.Pointer[chan struct{}]
	release := make(chan struct{})
	gate.Store(&release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if g := gate.Load(); g != nil {
			<-*g
		}
		_, _ = w.Write([]byte(`{"models":[{"Name":"m","Id":"0xabc"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	done := make(chan string, 2)
	go func() { done <- c.NameByID("0xabc") }()
	for fetches.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { done <- c.NameByID("0xabc") }()
	lockFree := make(chan bool, 1)
	go func() { lockFree <- c.Loaded() }()
	select {
	case l := <-lockFree:
		if l {
			t.Fatal("Loaded during the first fetch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("catalog lock held during a fetch")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		if n := <-done; n != "m" {
			t.Fatalf("waiter %d got %q", i, n)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 (the second caller waits)", got)
	}
	// Stale data while a refresh hangs: served at once.
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	rel2 := make(chan struct{})
	gate.Store(&rel2)
	go c.NameByID("0xabc") // starts the refresh, which hangs
	for fetches.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	got := make(chan string, 1)
	go func() { got <- c.NameByID("0xabc") }()
	select {
	case n := <-got:
		if n != "m" {
			t.Fatalf("stale lookup %q", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale lookup blocked behind a running refresh")
	}
	close(rel2)
}
