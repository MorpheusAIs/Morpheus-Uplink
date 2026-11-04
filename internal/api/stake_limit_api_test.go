package api

// Max stMOR per session and feed prices on the API.

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

var stakeLimit150 = new(big.Int).Mul(big.NewInt(150), big.NewInt(1e18))

// priceReads is the market's rated, supply and budget read counts.
func priceReads(m *fakeMarket) [3]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return [3]int{m.ratedReads, m.supplyReads, m.budgetReads}
}

// getJSON is a GET on the console session (admin paths) or with the API
// key (/v1/), optionally flagged as a poll, decoded into a map.
func getJSON(t *testing.T, e *failoverEnv, path string, poll bool) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if poll {
		req.Header.Set("X-Uplink-Poll", "1")
	}
	c := http.DefaultClient
	if strings.HasPrefix(path, "/v1/") {
		req.Header.Set("Authorization", "Bearer "+e.key)
	} else {
		if e.console == nil {
			e.admin(t, http.MethodGet, "/admin/settings", "") // logs in
		}
		withCSRF(req, e.srv.cfg)
		c = e.console
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

// priceRows are the price-carrying objects of each price surface.
func priceRows(t *testing.T, e *failoverEnv, poll bool) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, p := range []string{"/v1/models", "/admin/models"} {
		code, b := getJSON(t, e, p, poll)
		data, _ := b["data"].([]any)
		if code != 200 || len(data) != 1 {
			t.Fatalf("%s: %d %v", p, code, b)
		}
		out[p] = data[0].(map[string]any)["morpheus"].(map[string]any)
	}
	code, b := getJSON(t, e, "/admin/estimate-stake?model=test-model", poll)
	if code != 200 {
		t.Fatalf("estimate-stake: %d %v", code, b)
	}
	out["/admin/estimate-stake"] = b
	code, b = getJSON(t, e, "/admin/estimate-stakes", poll)
	rows, _ := b["models"].([]any)
	if code != 200 || len(rows) != 1 {
		t.Fatalf("estimate-stakes: %d %v", code, b)
	}
	out["/admin/estimate-stakes"] = rows[0].(map[string]any)
	out["/admin/estimate-stakes body"] = b
	return out
}

// Both refusals are 503 with no Retry-After and nothing but type, message
// and code, audited; no open is sent. The exceeded text names the stake
// needed, the cap, the session length and what to change.
func TestStakeLimitSpec8APIRefusals(t *testing.T) {
	exceeded := "Opening test-model for 10 min needs at least 0.000001 stMOR; the Uplink cap is 0.000001 stMOR per session. Raise Max stMOR per session or ask for a shorter session."
	for _, tc := range []struct {
		typ, msg string
		limit    *big.Int
		invalid  bool
	}{
		{"stake_limit_exceeded", exceeded, big.NewInt(600999), false},
		{"stake_limit_invalid", stakeLimitInvalidMsg, nil, true},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			m := newFakeMarket(t, "0x"+strings.Repeat("a", 40))
			e := newFailoverEnv(t, m, nil)
			e.srv.pool.SetStakeLimit(tc.limit, tc.invalid)
			code, hdr, body := chatFull(t, e, "test-model")
			if code != http.StatusServiceUnavailable || hdr.Get("Retry-After") != "" {
				t.Fatalf("code=%d Retry-After=%q body=%s", code, hdr.Get("Retry-After"), body)
			}
			var got struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("body: %s", body)
			}
			want := map[string]any{"type": tc.typ, "message": tc.msg, "code": float64(503)}
			if len(got.Error) != len(want) {
				t.Fatalf("error fields: %s", body)
			}
			for k, v := range got.Error {
				if want[k] != v {
					t.Fatalf("error.%s = %v; body %s", k, v, body)
				}
			}
			if got.Error["type"] != tc.typ || got.Error["message"] != tc.msg {
				t.Fatalf("body %s", body)
			}
			if strings.Contains(body, "0x") || strings.Contains(body, "600999") || strings.Contains(body, "601000") {
				t.Fatalf("body carries an id or a wei amount: %s", body)
			}
			if opens, _ := m.snapshot(); len(opens) != 0 {
				t.Fatalf("opens %d, want 0", len(opens))
			}
			var results []string
			for _, ev := range e.srv.audit.Recent(1000) {
				if ev.Kind == "provider_unavailable" {
					results = append(results, ev.Result)
				}
			}
			if len(results) != 1 || results[0] != tc.typ {
				t.Fatalf("audit %v, want [%s]", results, tc.typ)
			}
		})
	}
}

// Test 11, Go part: stakeLimit is the boot-effective value (off is null,
// never ""); saving a new value leaves it unchanged until a restart. The
// refusal slot shows on /admin/status after an exceeded refusal.
func TestStakeLimitSpec11StatusLimit(t *testing.T) {
	m := newFakeMarket(t, "0x"+strings.Repeat("a", 40))
	e := newFailoverEnv(t, m, nil)
	limitOf := func() (any, any, map[string]any) {
		_, st := getJSON(t, e, "/admin/status", false)
		sl, ok := st["stakeLimit"].(map[string]any)
		if !ok {
			t.Fatalf("no stakeLimit: %v", st["stakeLimit"])
		}
		v, present := sl["maxStMorPerSessionWei"]
		if !present {
			t.Fatal("maxStMorPerSessionWei missing")
		}
		return v, sl["invalid"], st
	}
	if v, inv, st := limitOf(); v != nil || inv != false {
		t.Fatalf("off: %v %v", v, inv)
	} else if _, ok := st["stakeRefusal"]; ok {
		t.Fatal("stakeRefusal shown with no refusal")
	}
	e.srv.pool.SetStakeLimit(stakeLimit150, false)
	if v, inv, _ := limitOf(); v != "150000000000000000000" || inv != false {
		t.Fatalf("on: %v %v", v, inv)
	}
	if code, body := e.admin(t, http.MethodPost, "/admin/settings", `{"name":"maxStMorPerSession","value":"5"}`); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	if v, _, _ := limitOf(); v != "150000000000000000000" {
		t.Fatalf("saved, not restarted: %v", v)
	}
	e.srv.pool.SetStakeLimit(nil, true)
	if v, inv, _ := limitOf(); v != nil || inv != true {
		t.Fatalf("invalid: %v %v", v, inv)
	}
	e.srv.pool.SetStakeLimit(big.NewInt(600999), false)
	if code, _, _ := chatFull(t, e, "test-model"); code != http.StatusServiceUnavailable {
		t.Fatalf("over limit: %d", code)
	}
	_, _, st := limitOf()
	r, _ := st["stakeRefusal"].(map[string]any)
	if r == nil || r["needWei"] != "601000" || r["capWei"] != "600999" || r["durationSec"] != float64(600) {
		t.Fatalf("stakeRefusal after an exceeded refusal: %v", st["stakeRefusal"])
	}
}

// Test 12: one bid shows the same stake on every surface, priced from the
// feed; sessions carry no per-hour price.
func TestStakeLimitSpec12SamePriceEverywhere(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.supply, m.budget = "2000000000000000000000000", "3000000000000000000000000"
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(modelsFeed(time.Now().Unix(), "test-model",
			feedRow{model: testModelID, bid: bidOf(a40), prov: a40, status: "healthy"}))
	}))
	t.Cleanup(cat.Close)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = cat.URL })
	e.srv.pool.SetStakeLimit(stakeLimit150, false)
	// 600 s: ceil(2e24 * 1000 * 601 / 3e24) = 400667; per hour 2400000.
	const want, wantHour = "400667", "2400000"
	if code, _, body := chatFull(t, e, "test-model"); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
	if err := e.srv.pool.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	getJSON(t, e, "/admin/estimate-stakes", false) // a user action: reads the pair
	for path, row := range priceRows(t, e, false) {
		if strings.HasSuffix(path, " body") {
			if row["maxStMorPerSessionWei"] != "150000000000000000000" {
				t.Fatalf("%s: cap %v", path, row["maxStMorPerSessionWei"])
			}
			continue
		}
		if row["stakeWei"] != want || row["stMorPerHourWei"] != wantHour || row["priceSource"] != "feed" || row["pricePerSecondWei"] != "1000" {
			t.Fatalf("%s: %v", path, row)
		}
		if f, _ := row["stMor"].(float64); f != 4.00667e-13 {
			t.Fatalf("%s: stMor %v", path, row["stMor"])
		}
		for _, old := range []string{"priceMorPerHour", "sessionCostWei"} {
			if _, ok := row[old]; ok {
				t.Fatalf("%s: %s still present", path, old)
			}
		}
	}
	_, st := getJSON(t, e, "/admin/status", false)
	ss, _ := st["sessions"].([]any)
	if len(ss) != 1 {
		t.Fatalf("status sessions: %v", st["sessions"])
	}
	if _, ok := ss[0].(map[string]any)["stMorPerHourWei"]; ok {
		t.Fatalf("session row still has stMorPerHourWei: %v", ss[0])
	}
}

// The shown price is the cheapest bid an open could use: an unhealthy
// cheaper bid is passed over.
func TestFeedPriceSkipsUnhealthyBid(t *testing.T) {
	a40, b40 := "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40)
	m := newFakeMarket(t, a40)
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		raw, _ := json.Marshal(map[string]any{"last_updated": now, "last_updated_utc": utc(now), "models": []any{map[string]any{
			"Name": "test-model", "Id": testModelID, "Tags": []string{"chat"},
			"bidDetail": []any{
				map[string]any{"bidId": bidOf(b40), "providerId": b40, "pricePerSecond": "500", "status": "unhealthy"},
				map[string]any{"bidId": bidOf(a40), "providerId": a40, "pricePerSecond": "1000", "status": "healthy"},
			},
		}}})
		_, _ = w.Write(raw)
	}))
	t.Cleanup(cat.Close)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = cat.URL })
	e.srv.pool.SetHeldStakeInputs(big.NewInt(1e18), big.NewInt(1e18))
	_, b := getJSON(t, e, "/v1/models", false)
	data, _ := b["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("models: %v", b)
	}
	if mp := data[0].(map[string]any)["morpheus"].(map[string]any); mp["pricePerSecondWei"] != "1000" || mp["stakeWei"] != "601000" {
		t.Fatalf("row %v", mp)
	}
}

func TestStakeGroup(t *testing.T) {
	w := func(n int64) *big.Int { return big.NewInt(n) }
	for _, c := range []struct {
		stake, cap, liquid *big.Int
		want               string
	}{
		{w(5), w(5), w(5), "affordable"},
		{w(6), w(5), w(10), "overCap"},
		{w(11), w(5), w(10), "overBudget"},
		{w(11), nil, w(10), "overBudget"},
		{w(6), w(5), nil, "overCap"},
		{w(6), nil, nil, "affordable"},
		{nil, w(5), w(10), ""},
	} {
		if got := stakeGroup(c.stake, c.cap, c.liquid); got != c.want {
			t.Errorf("stakeGroup(%v, %v, %v) = %q, want %q", c.stake, c.cap, c.liquid, got, c.want)
		}
	}
}

// Test 14, Go part: flagged requests make no rated, supply or budget read.
// Cold: the feed price with no stake (null before a held pair). Warm: the
// stake at the held pair, still no reads.
func TestStakeLimitSpec14PollsReadNothing(t *testing.T) {
	m := newFakeMarket(t, "0x"+strings.Repeat("a", 40))
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.SetStakeLimit(stakeLimit150, false)
	getJSON(t, e, "/admin/status", false) // log in

	before := priceReads(m)
	for path, row := range priceRows(t, e, true) {
		if strings.HasSuffix(path, " body") {
			if row["supplyWei"] != nil || row["budgetWei"] != nil {
				t.Fatalf("cold %s: %v", path, row)
			}
			continue
		}
		if row["priceSource"] != "feed" || row["stakeWei"] != nil || row["pricePerSecondWei"] != "1000" {
			t.Fatalf("cold %s: %v", path, row)
		}
	}
	if got := priceReads(m); got != before {
		t.Fatalf("cold polls read: %v -> %v", before, got)
	}

	getJSON(t, e, "/admin/estimate-stakes", false) // a user action: reads the pair
	warm := priceReads(m)
	if warm == before {
		t.Fatal("the user action read nothing")
	}
	for i := 0; i < 5; i++ {
		for path, row := range priceRows(t, e, true) {
			if !strings.HasSuffix(path, " body") && (row["priceSource"] != "feed" || row["stakeWei"] != "601000") {
				t.Fatalf("warm %s: %v", path, row)
			}
		}
	}
	if got := priceReads(m); got != warm {
		t.Fatalf("warm polls read: %v -> %v", warm, got)
	}
}

// Test 15: /v1/models shows a feed price for every model with a bid and
// reads nothing.
func TestStakeLimitSpec15ColdModelsEst(t *testing.T) {
	m := newFakeMarket(t, "0x"+strings.Repeat("a", 40), "0x"+strings.Repeat("b", 40))
	e := newFailoverEnv(t, m, nil)
	e.srv.pool.SetStakeLimit(stakeLimit150, false)
	e.srv.pool.SetHeldStakeInputs(big.NewInt(1e18), big.NewInt(1e18))
	before := priceReads(m)
	code, b := getJSON(t, e, "/v1/models", false)
	data, _ := b["data"].([]any)
	if code != 200 || len(data) == 0 {
		t.Fatalf("models: %d %v", code, b)
	}
	for _, r := range data {
		mp := r.(map[string]any)["morpheus"].(map[string]any)
		if mp["priceSource"] != "feed" || mp["stakeWei"] != "601000" || mp["stMorPerHourWei"] != "3600000" {
			t.Fatalf("row %v", mp)
		}
	}
	if got := priceReads(m); got != before {
		t.Fatalf("reads %v -> %v", before, got)
	}
}
