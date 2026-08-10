package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

func TestRehydrateAdoptsOpenSession(t *testing.T) {
	var closed atomic.Int32
	ends := time.Now().Add(2 * time.Hour).Unix()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{
					"Id":           "0xsessionalive",
					"ModelAgentId": "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd",
					"Stake":        "1000000000000000000",
					"EndsAt":       ends,
					"ClosedAt":     0,
				},
				{
					"Id":           "0xsessiondead",
					"ModelAgentId": "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead",
					"Stake":        "2000000000000000000",
					"EndsAt":       time.Now().Add(-time.Hour).Unix(),
					"ClosedAt":     0,
				},
			},
		})
	})
	mux.HandleFunc("POST /blockchain/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		closed.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	rc := router.New(ts.URL, "admin", "pass")
	p := New(rc, 600, true, false)
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 {
		t.Fatalf("expected 1 expired close, got %d", closed.Load())
	}
	snap := p.Snapshot()
	if len(snap) != 1 || snap[0].SessionID != "0xsessionalive" {
		t.Fatalf("adopted = %+v", snap)
	}
	chain := p.ChainOpen()
	if len(chain) != 1 || !chain[0].Pooled {
		t.Fatalf("chain display = %+v", chain)
	}
	if p.ActiveStakeWei().String() != "1000000000000000000" {
		t.Fatalf("active stake = %s", p.ActiveStakeWei())
	}
}

func TestEnsureDurationAdoptsChainBeforeOpen(t *testing.T) {
	var opened atomic.Int32
	modelID := "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd"
	ends := time.Now().Add(2 * time.Hour).Unix()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{
					"Id":           "0xsessionalive",
					"ModelAgentId": modelID,
					"Stake":        "1000000000000000000",
					"EndsAt":       ends,
					"ClosedAt":     0,
				},
			},
		})
	})
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": "0xshouldnotopen"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	rc := router.New(ts.URL, "admin", "pass")
	p := New(rc, 600, true, false)
	// Simulate invalidate after a bad prompt while chain session remains open.
	p.Invalidate(modelID)
	res, err := p.EnsureDuration(modelID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if res.Opened {
		t.Fatalf("expected adopt, got Opened=true")
	}
	if res.SessionID != "0xsessionalive" {
		t.Fatalf("session = %s", res.SessionID)
	}
	if opened.Load() != 0 {
		t.Fatalf("opened new sessions = %d, want 0", opened.Load())
	}
}
