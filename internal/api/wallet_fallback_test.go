package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// rejectingRouter answers every authenticated call with 401, like a router
// whose COOKIE_CONTENT no longer matches Uplink's.
func rejectingRouter(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
			return
		}
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
	}))
}

func newServerWithRouter(t *testing.T, rt *httptest.Server) (*httptest.Server, *config.Config) {
	t.Helper()
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)
	cfg := &config.Config{
		RouterURL:          rt.URL,
		RouterUser:         "uplink",
		RouterPass:         "stale",
		AdminPassword:      "adminpass",
		APIKeySeed:         "test-seed",
		DataDir:            t.TempDir(),
		ActiveModelsURL:    cat.URL,
		SessionDurationSec: 600,
		DevHTTP:            true,
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, 600), rc, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	cfg.WebPublicURL = u.Scheme + "://" + u.Host
	return ts, cfg
}

func getStatus(t *testing.T, c *http.Client, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := c.Get(ts.URL + "/admin/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Router rejects the credential: routerAuth reports the 401 distinctly and
// the wallet lookup error is surfaced.
func TestStatusRouterAuthRejected(t *testing.T) {
	rt := rejectingRouter(t)
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt)
	st := getStatus(t, adminClient(t, ts, cfg), ts)
	if st["walletAddressError"] == nil {
		t.Fatal("expected walletAddressError")
	}
	ra, _ := st["routerAuth"].(map[string]any)
	if ra == nil || ra["ok"] != false || ra["kind"] != router.KindAuth {
		t.Fatalf("routerAuth: %v", st["routerAuth"])
	}
}

// Once the router has answered, a later failure keeps the last-known value.
func TestStatusWalletKeepsLastKnown(t *testing.T) {
	ok := true
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wallet" && ok {
			_, _ = w.Write([]byte(`{"address":"0x3333333333333333333333333333333333333333"}`))
			return
		}
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt)
	c := adminClient(t, ts, cfg)
	if st := getStatus(t, c, ts); st["walletAddressSource"] != "router" {
		t.Fatalf("first: %v", st["walletAddressSource"])
	}
	ok = false
	st := getStatus(t, c, ts)
	if st["walletAddress"] != "0x3333333333333333333333333333333333333333" || st["walletAddressSource"] != "last-known" {
		t.Fatalf("last-known: %v / %v", st["walletAddress"], st["walletAddressSource"])
	}
}

// The GUI catalog endpoint works with the admin session alone (the GUI only
// holds masked API keys) and does not depend on router auth.
func TestAdminModelsUsesSessionNotRouter(t *testing.T) {
	rt := rejectingRouter(t)
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt)
	c := adminClient(t, ts, cfg)
	resp, err := c.Get(ts.URL + "/admin/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Data) != 1 {
		t.Fatalf("models: %s", b)
	}
	// Unauthenticated: refused.
	r2, _ := http.Get(ts.URL + "/admin/models")
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon status=%d", r2.StatusCode)
	}
}

// Stake estimates name a rejected router credential distinctly.
func TestEstimateStakesReportsRouterAuthKind(t *testing.T) {
	rt := rejectingRouter(t)
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt)
	c := adminClient(t, ts, cfg)
	resp, err := c.Get(ts.URL + "/admin/estimate-stakes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusBadGateway || out["kind"] != router.KindAuth {
		t.Fatalf("status=%d body=%v", resp.StatusCode, out)
	}
}
