package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

func TestCooldownTTLAndPruning(t *testing.T) {
	p := New(router.New("http://127.0.0.1:9", "u", "p"), 600)
	p.SetProviderCooldown(40 * time.Millisecond)
	p.MarkCooling("Model-X", "0xABCDEF")
	if !p.IsCooling("model-x", "0xabcdef") {
		t.Fatal("keys must be case-insensitive (model and provider)")
	}
	if p.IsCooling("model-y", "0xabcdef") {
		t.Fatal("cool-down is per model")
	}
	time.Sleep(60 * time.Millisecond)
	if p.IsCooling("model-x", "0xabcdef") {
		t.Fatal("cool-down did not expire")
	}
	p.MarkCooling("model-z", "0x01")
	p.fmu.Lock()
	n := len(p.cooling)
	p.fmu.Unlock()
	if n != 1 {
		t.Fatalf("expired entries not pruned on write: %d", n)
	}
	p.SetProviderCooldown(0)
	p.MarkCooling("model-q", "0x02")
	if p.IsCooling("model-q", "0x02") {
		t.Fatal("cool-down 0 must not remember providers")
	}
	p.MarkCooling("model-q", "")
	if p.IsCooling("model-q", "") {
		t.Fatal("unknown provider must never cool")
	}
}

// TestRehydrateAdoptsOrClosesUnheld (replaces the old cooling/retired skip
// test): a rescan adopts one unheld session per model by pickOne (latest
// EndsAt; cooling does not matter, health gates new opens only) and never
// a retired one, and closes nothing. The sweep then closes the unheld rest,
// one close per slot hold.
func TestRehydrateAdoptsOrClosesUnheld(t *testing.T) {
	const model = "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd"
	far := time.Now().Add(3 * time.Hour).Unix()
	near := time.Now().Add(2 * time.Hour).Unix()
	nearer := time.Now().Add(1 * time.Hour).Unix()
	var mu sync.Mutex
	closed := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			mu.Lock()
			for _, s := range []map[string]any{
				{"Id": "0xretired", "ModelAgentId": model, "Provider": "0xAAAA", "Stake": "1", "EndsAt": far, "ClosedAt": 0},
				{"Id": "0xcooling", "ModelAgentId": model, "Provider": "0xBBBB", "Stake": "1", "EndsAt": near, "ClosedAt": 0},
				{"Id": "0xgood", "ModelAgentId": model, "Provider": "0xCCCC", "Stake": "1", "EndsAt": nearer, "ClosedAt": 0},
			} {
				if !closed[s["Id"].(string)] {
					rows = append(rows, s)
				}
			}
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": rows})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		closed[r.PathValue("id")] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	p := New(router.New(ts.URL, "u", "p"), 600)
	p.retire("0xretired", time.Unix(far, 0))
	p.MarkCooling(model, "0xbbbb")
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()
	if len(snap) != 1 || snap[0].SessionID != "0xcooling" {
		t.Fatalf("adopted %+v, want only 0xcooling (latest EndsAt, not retired)", snap)
	}
	if got := len(p.ChainOpen()); got != 3 {
		t.Fatalf("chain open rows %d want 3", got)
	}
	mu.Lock()
	n := len(closed)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("rehydrate closed %d sessions; it only adopts", n)
	}
	for range 3 {
		p.ScanOrphans(context.Background())
	}
	mu.Lock()
	defer mu.Unlock()
	if !closed["0xretired"] || !closed["0xgood"] || closed["0xcooling"] {
		t.Fatalf("sweep closes %v, want 0xretired and 0xgood only", closed)
	}
}
