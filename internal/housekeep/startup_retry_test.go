package housekeep

import (
	"context"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// The boot scan races the router: the first attempts get connection refused,
// then the router comes up. The startup loop must retry with backoff and the
// stale error must be gone from Last() once a later scan succeeds.
func TestStartupScanRetriesUntilRouterReady(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":"1000000000000000000","mor":"0"}`))
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

	// Reserve a port, close it so dials are refused, start the real server
	// on the same port a little later.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	rc := router.New("http://"+addr, "uplink", "pass")
	pl := pool.New(rc, 600)
	runner, err := New(Config{
		Enabled:           true,
		Router:            rc,
		WithdrawThreshold: big.NewInt(0),
		CheckEvery:        time.Hour,
		StartupBackoff:    20 * time.Millisecond,
		StartupBackoffMax: 40 * time.Millisecond,
		StartupRetryFor:   5 * time.Second,
	}, pl)
	if err != nil {
		t.Fatal(err)
	}

	var started atomic.Bool
	go func() {
		time.Sleep(150 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
		srv.Start()
		started.Store(true)
		t.Cleanup(srv.Close)
	}()

	// Observe the stale error while the router is down.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Start(ctx)

	sawErr := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		last := runner.Last()
		if last.Status == "error" {
			sawErr = true
		}
		if started.Load() && last.Status != "" && last.Status != "error" && last.ClosedViaScan {
			if !sawErr {
				t.Fatal("expected the first boot scan to fail while the router was down")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("startup scan never succeeded; last=%+v sawErr=%v", runner.Last(), sawErr)
}

// Retry is bounded: a router that never comes up stops the loop within the
// budget instead of spinning forever.
func TestStartupScanRetryIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	rc := router.New("http://"+addr, "uplink", "pass")
	pl := pool.New(rc, 600)
	runner, _ := New(Config{
		Enabled:           true,
		Router:            rc,
		StartupBackoff:    10 * time.Millisecond,
		StartupBackoffMax: 20 * time.Millisecond,
		StartupRetryFor:   200 * time.Millisecond,
	}, pl)
	start := time.Now()
	runner.startupRun(context.Background())
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("startup retry not bounded: %s", el)
	}
	if got := runner.Last(); got.Status != "error" {
		t.Fatalf("want error after budget, got %+v", got)
	}
	if router.ErrorKind(errFromMsg(runner.Last().Message)) == "" {
		t.Fatal("expected a classified error")
	}
}

type msgErr string

func (m msgErr) Error() string { return string(m) }

func errFromMsg(s string) error { return msgErr(s) }
