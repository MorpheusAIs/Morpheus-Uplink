package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestRehydrateSkipsCoolingAndRetired(t *testing.T) {
	const model = "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd"
	far := time.Now().Add(3 * time.Hour).Unix()
	near := time.Now().Add(2 * time.Hour).Unix()
	nearer := time.Now().Add(1 * time.Hour).Unix()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{}
		if r.URL.Query().Get("offset") == "0" {
			rows = []map[string]any{
				{"Id": "0xretired", "ModelAgentId": model, "Provider": "0xAAAA", "Stake": "1", "EndsAt": far, "ClosedAt": 0},
				{"Id": "0xcooling", "ModelAgentId": model, "Provider": "0xBBBB", "Stake": "1", "EndsAt": near, "ClosedAt": 0},
				{"Id": "0xgood", "ModelAgentId": model, "Provider": "0xCCCC", "Stake": "1", "EndsAt": nearer, "ClosedAt": 0},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": rows})
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
	if len(snap) != 1 || snap[0].SessionID != "0xgood" {
		t.Fatalf("adopted %+v, want only 0xgood", snap)
	}
	// Still listed for the GUI, just not pooled.
	if got := len(p.ChainOpen()); got != 3 {
		t.Fatalf("chain open rows %d want 3", got)
	}
	p.mu.Lock()
	prov := p.entries[normModel(model)].provider
	p.mu.Unlock()
	if prov != "0xcccc" {
		t.Fatalf("adopted entry provider %q", prov)
	}
}
