package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// Every request Uplink sends to the router while serving inference (session
// open, failover re-open after a bad session, rehydrate, close) is recorded;
// none may ask for direct payment, and every session open says false.
func TestUplinkNeverSendsDirectPayment(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var opens []string
	firstChat := true
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/session") || r.URL.Path == "/blockchain/sessions":
			mu.Lock()
			opens = append(opens, string(b))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"sessionID":"0xsession1"}`))
		case r.URL.Path == "/v1/chat/completions":
			mu.Lock()
			fail := firstChat
			firstChat = false
			mu.Unlock()
			if fail { // force the failover/re-open path once
				http.Error(w, `{"error":"session expired"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		case r.URL.Path == "/blockchain/sessions/user":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case r.URL.Path == "/wallet":
			_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer rt.Close()
	cat := mockCatalog(t)
	defer cat.Close()
	cfg := &config.Config{RouterURL: rt.URL, RouterUser: "uplink", RouterPass: "p", AdminPassword: "adminpass",
		APIKeySeed: "seed", DataDir: t.TempDir(), ActiveModelsURL: cat.URL, SessionDurationSec: 600, SessionFailover: true, DevHTTP: true}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	pl := pool.New(rc, 600, true)
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pl, rc, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"directPayment":true}`))
		req.Header.Set("Authorization", "Bearer "+keymaker.MasterKey(cfg.APIKeySeed))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	pl.CloseAll()
	mu.Lock()
	defer mu.Unlock()
	if len(opens) == 0 {
		t.Fatal("no session was opened; test did not exercise the open path")
	}
	for _, o := range opens {
		if !strings.Contains(o, `"directPayment":false`) {
			t.Fatalf("session open without explicit directPayment:false: %s", o)
		}
	}
	for _, b := range bodies {
		if strings.HasPrefix(b, "POST /blockchain") && strings.Contains(strings.ToLower(strings.ReplaceAll(b, " ", "")), `"directpayment":true`) {
			t.Fatalf("direct payment requested: %s", b)
		}
	}
}
