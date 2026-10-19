package housekeep

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

func TestReclaimViaRouter(t *testing.T) {
	var withdraws atomic.Int32
	claimable := big.NewInt(1_000_000_000_000_000_000) // 1 MOR
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":"1000000000000000000","mor":"0"}`))
	})
	mux.HandleFunc("GET /blockchain/stakes/onhold", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"available": claimable.String(),
			"hold":      "0",
		})
	})
	mux.HandleFunc("POST /blockchain/stakes/withdraw", func(w http.ResponseWriter, r *http.Request) {
		withdraws.Add(1)
		claimable = big.NewInt(0)
		_ = json.NewEncoder(w).Encode(map[string]string{"tx": "0xabc"})
	})
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rc := router.New(srv.URL, "uplink", "pass")
	pl := pool.New(rc, 600, true, false)
	runner, err := New(Config{
		Enabled:            true,
		Router:             rc,
		WithdrawThreshold:  big.NewInt(0),
		WithdrawIterations: 255,
		MaxWithdrawRounds:  3,
	}, pl)
	if err != nil {
		t.Fatal(err)
	}
	if !runner.ReclaimReady() {
		t.Fatal("expected reclaim ready")
	}
	res := runner.RunOnce(context.Background())
	if res.Status != "actioned" {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	if withdraws.Load() < 1 {
		t.Fatalf("withdraws=%d", withdraws.Load())
	}
	if len(res.TxHashes) != 1 || res.TxHashes[0] != "0xabc" {
		t.Fatalf("hashes=%v", res.TxHashes)
	}
}

func TestReclaimCapsRounds(t *testing.T) {
	var withdraws atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":"1","mor":"0"}`))
	})
	mux.HandleFunc("GET /blockchain/stakes/onhold", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"available": "1000", "hold": "0"})
	})
	mux.HandleFunc("POST /blockchain/stakes/withdraw", func(w http.ResponseWriter, r *http.Request) {
		withdraws.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"tx": "0x" + strings.Repeat("a", 64)})
	})
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc := router.New(srv.URL, "u", "p")
	runner, _ := New(Config{
		Enabled: true, Router: rc, MaxWithdrawRounds: 2,
		WithdrawThreshold: big.NewInt(0), WithdrawIterations: 255,
	}, pool.New(rc, 600, true, false))
	res := runner.Run(context.Background())
	if withdraws.Load() != 2 {
		t.Fatalf("want 2 rounds, got %d status=%s", withdraws.Load(), res.Status)
	}
}

func TestGasLowFlag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":"1","mor":"0"}`)) // tiny
	})
	mux.HandleFunc("GET /blockchain/stakes/onhold", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"available": "0", "hold": "0"})
	})
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc := router.New(srv.URL, "u", "p")
	runner, _ := New(Config{
		Enabled: true, Router: rc, GasLowWei: big.NewInt(1000),
		WithdrawThreshold: big.NewInt(0),
	}, pool.New(rc, 600, true, false))
	res := runner.Run(context.Background())
	if !res.GasLow {
		t.Fatalf("expected gasLow, eth=%s", res.EthBalanceWei)
	}
}
