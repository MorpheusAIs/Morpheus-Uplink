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
