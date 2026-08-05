package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

const testModelID = "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd"

// mockRouter fakes the proxy-router endpoints the gateway uses.
func mockRouter(t *testing.T, sessionsOpened *atomic.Int32) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "admin" || pass != "routerpass" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.PathValue("id") != testModelID {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		sessionsOpened.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": "0xsession1"})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") != "0xsession1" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	})
	mux.HandleFunc("GET /healthcheck", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	})
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":1000000000000000000,"mor":5000000000000000000}`))
	})
	return httptest.NewServer(mux)
}

func mockCatalog(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"Name":"test-model","Id":"` + testModelID + `","Tags":["chat"]}]}`))
	}))
}

func newTestServer(t *testing.T) (*httptest.Server, *config.Config, *atomic.Int32) {
	t.Helper()
	var opened atomic.Int32
	rt := mockRouter(t, &opened)
	t.Cleanup(rt.Close)
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)

	cfg := &config.Config{
		RouterURL:          rt.URL,
		RouterUser:         "admin",
		RouterPass:         "routerpass",
		AdminPassword:      "adminpass",
		APIKeySeed:         "test-seed",
		DataDir:            t.TempDir(),
		ActiveModelsURL:    cat.URL,
		SessionDurationSec: 600,
		SessionFailover:    true,
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, cfg.SessionDurationSec, true, false), rc)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, cfg, &opened
}

func TestChatCompletionsEndToEnd(t *testing.T) {
	ts, cfg, opened := newTestServer(t)
	master := keymaker.MasterKey(cfg.APIKeySeed)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if opened.Load() != 1 {
		t.Fatalf("sessions opened = %d, want 1", opened.Load())
	}

	// Second request must reuse the pooled session.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+master)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if opened.Load() != 1 {
		t.Fatalf("sessions opened after reuse = %d, want 1", opened.Load())
	}
}

func TestAuthRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	for _, token := range []string{"", "sk-bogus.deadbeef"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, resp.StatusCode)
		}
	}
}

func TestGeneratedKeyLifecycle(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// Create a key via the admin API.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/keys", strings.NewReader(`{"name":"cursor"}`))
	req.SetBasicAuth("admin", "adminpass")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Key    string
		Record keymaker.Record
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Key == "" {
		t.Fatalf("create key: status %d, key %q", resp.StatusCode, created.Key)
	}

	// The generated key must authenticate.
	chatReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/models", nil)
	chatReq.Header.Set("Authorization", "Bearer "+created.Key)
	chatReq.Method = http.MethodGet
	resp2, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("models with generated key: status %d, want 200", resp2.StatusCode)
	}

	// Revoke, then it must fail.
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/admin/keys/"+created.Record.ID, nil)
	del.SetBasicAuth("admin", "adminpass")
	resp3, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status %d", resp3.StatusCode)
	}
	resp4, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key: status %d, want 401", resp4.StatusCode)
	}
}

func TestKeyScopes(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	master := keymaker.MasterKey(cfg.APIKeySeed)
	prompt := keymaker.PromptKey(cfg.APIKeySeed)

	get := func(path, bearer string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Both derived keys may prompt (via /v1/models as the cheap proxy).
	if code := get("/v1/models", prompt); code != http.StatusOK {
		t.Fatalf("prompt key on /v1/models: %d, want 200", code)
	}
	if code := get("/v1/models", master); code != http.StatusOK {
		t.Fatalf("master key on /v1/models: %d, want 200", code)
	}

	// Only the master key reaches the admin surface via Bearer.
	if code := get("/admin/keys", master); code != http.StatusOK {
		t.Fatalf("master key on /admin/keys: %d, want 200", code)
	}
	if code := get("/admin/keys", prompt); code != http.StatusUnauthorized {
		t.Fatalf("prompt key on /admin/keys: %d, want 401", code)
	}
	if code := get("/node/healthcheck", prompt); code != http.StatusUnauthorized {
		t.Fatalf("prompt key on /node: %d, want 401", code)
	}
	if code := get("/node/healthcheck", master); code != http.StatusOK {
		t.Fatalf("master key on /node: %d, want 200", code)
	}
}

// TestMasterKeyCreatesSubkeys pins the contract that the master key alone
// (no admin password) can mint and revoke prompt-scoped subkeys.
func TestMasterKeyCreatesSubkeys(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	master := keymaker.MasterKey(cfg.APIKeySeed)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/keys", strings.NewReader(`{"name":"subkey-via-master"}`))
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Key    string
		Record keymaker.Record
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Key == "" {
		t.Fatalf("create subkey via master: status %d, key %q", resp.StatusCode, created.Key)
	}

	// The subkey prompts but cannot create further keys (prompt scope only).
	mReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	mReq.Header.Set("Authorization", "Bearer "+created.Key)
	mResp, err := http.DefaultClient.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("subkey on /v1/models: %d, want 200", mResp.StatusCode)
	}
	cReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/keys", strings.NewReader(`{"name":"nope"}`))
	cReq.Header.Set("Authorization", "Bearer "+created.Key)
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatal(err)
	}
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("subkey creating keys: %d, want 401", cResp.StatusCode)
	}

	// Master revokes it again.
	dReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/admin/keys/"+created.Record.ID, nil)
	dReq.Header.Set("Authorization", "Bearer "+master)
	dResp, err := http.DefaultClient.Do(dReq)
	if err != nil {
		t.Fatal(err)
	}
	dResp.Body.Close()
	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke subkey via master: %d, want 200", dResp.StatusCode)
	}
}

func TestNodePassthroughRequiresAdmin(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/node/healthcheck")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /node: status %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/node/healthcheck", nil)
	req.SetBasicAuth("admin", "adminpass")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("admin /node: status %d, want 200", resp2.StatusCode)
	}
}
