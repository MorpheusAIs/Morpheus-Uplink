package api

// Max stMOR/hr on the API: spec §5 tests 8 (API part), 11 (Go part), 12,
// 14 (Go part) and 15.

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

// Test 8, API part: both refusals are 503 with the fixed body, no
// Retry-After, nothing but type and message, audited; no open is sent.
func TestStakeLimitSpec8APIRefusals(t *testing.T) {
	for _, tc := range []struct {
		typ, msg string
		limit    *big.Int
		invalid  bool
	}{
		{"stake_limit_exceeded", stakeLimitExceededMsg, big.NewInt(3599999), false},
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
			if strings.Contains(body, "0x") || strings.Contains(body, "3599999") || strings.Contains(body, "3600000") {
				t.Fatalf("body carries an id, price or limit: %s", body)
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
		v, present := sl["maxStMorPerHourWei"]
		if !present {
			t.Fatal("maxStMorPerHourWei missing")
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
	if code, body := e.admin(t, http.MethodPost, "/admin/settings", `{"name":"maxStMorPerHour","value":"5"}`); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	if v, _, _ := limitOf(); v != "150000000000000000000" {
		t.Fatalf("saved, not restarted: %v", v)
	}
	e.srv.pool.SetStakeLimit(nil, true)
	if v, inv, _ := limitOf(); v != nil || inv != true {
		t.Fatalf("invalid: %v %v", v, inv)
	}
	e.srv.pool.SetStakeLimit(big.NewInt(3599999), false)
	if code, _, _ := chatFull(t, e, "test-model"); code != http.StatusServiceUnavailable {
		t.Fatalf("over limit: %d", code)
	}
	if _, _, st := limitOf(); st["stakeRefusal"] == nil {
		t.Fatal("no stakeRefusal after an exceeded refusal")
	}
}

// Test 12: one bid shows the same stMOR/hr on every surface; the old
// fields are gone.
func TestStakeLimitSpec12SamePriceEverywhere(t *testing.T) {
	a40 := "0x" + strings.Repeat("a", 40)
	m := newFakeMarket(t, a40)
	m.supply, m.budget = "2000000000000000000000000", "3000000000000000000000000"
	// The catalog lists the market's own bid, so the session row's bid is
	// in bidDetail.
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(modelsFeed(time.Now().Unix(), "test-model",
			feedRow{model: testModelID, bid: bidOf(a40), prov: a40, status: "healthy"}))
	}))
	t.Cleanup(cat.Close)
	e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = cat.URL })
	e.srv.pool.SetStakeLimit(stakeLimit150, false)
	// ceil(2e24 * 1000 * 3600 / 3e24) = 2400000.
	const want = "2400000"
	if code, _, body := chatFull(t, e, "test-model"); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
	if err := e.srv.pool.RehydrateForce(); err != nil { // the chain scan names the session's bid
		t.Fatal(err)
	}
	getJSON(t, e, "/admin/estimate-stakes", false) // a user action: fills
	for path, row := range priceRows(t, e, false) {
		if strings.HasSuffix(path, " body") {
			if _, ok := row["sessionCostWei"]; ok {
				t.Fatalf("%s: sessionCostWei still present", path)
			}
			continue
		}
		if row["stMorPerHourWei"] != want || row["priceSource"] != "firstTry" || row["pricePerSecondWei"] != "1000" {
			t.Fatalf("%s: %v", path, row)
		}
		if f, _ := row["stMorPerHour"].(float64); f != 2.4e-12 {
			t.Fatalf("%s: stMorPerHour %v", path, row["stMorPerHour"])
		}
		for _, old := range []string{"priceMorPerHour", "sessionCostWei"} {
			if _, ok := row[old]; ok {
				t.Fatalf("%s: %s still present", path, old)
			}
		}
	}
	_, st := getJSON(t, e, "/admin/status", false)
	ss, _ := st["sessions"].([]any)
	if len(ss) != 1 || ss[0].(map[string]any)["stMorPerHourWei"] != want {
		t.Fatalf("status sessions: %v", st["sessions"])
	}
}

// Test 14, Go part: flagged requests make no rated, supply or budget read.
// Cold: est. prices (null before a held pair). Warm: firstTry, no reads.
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
		if row["priceSource"] != "est." || row["stMorPerHourWei"] != nil || row["pricePerSecondWei"] != "1000" {
			t.Fatalf("cold %s: %v", path, row)
		}
	}
	if got := priceReads(m); got != before {
		t.Fatalf("cold polls read: %v -> %v", before, got)
	}

	getJSON(t, e, "/admin/estimate-stakes", false) // a user action: fills
	warm := priceReads(m)
	if warm == before {
		t.Fatal("the user action read nothing")
	}
	for i := 0; i < 5; i++ {
		for path, row := range priceRows(t, e, true) {
			if !strings.HasSuffix(path, " body") && (row["priceSource"] != "firstTry" || row["stMorPerHourWei"] != "3600000") {
				t.Fatalf("warm %s: %v", path, row)
			}
		}
	}
	if got := priceReads(m); got != warm {
		t.Fatalf("warm polls read: %v -> %v", warm, got)
	}
}

// Test 15: a cold /v1/models shows an est. price for every model with a
// bid and reads nothing.
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
		if mp["priceSource"] != "est." || mp["stMorPerHourWei"] != "3600000" {
			t.Fatalf("row %v", mp)
		}
	}
	if got := priceReads(m); got != before {
		t.Fatalf("reads %v -> %v", before, got)
	}
}
