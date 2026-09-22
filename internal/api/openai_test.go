package api

import (
	"encoding/json"
	"io"
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

func TestValidateInferenceBody(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		requireMessages bool
		wantModel       string
		wantErrSubstr   string
	}{
		{name: "ok chat", body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, requireMessages: true, wantModel: "m"},
		{name: "ok with extras", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"venice_parameters":{"x":1},"thinking":true,"tools":[]}`, requireMessages: true, wantModel: "m"},
		{name: "non-object array", body: `[]`, requireMessages: true, wantErrSubstr: "JSON object"},
		{name: "non-object string", body: `"nope"`, requireMessages: true, wantErrSubstr: "JSON object"},
		{name: "invalid json", body: `{`, requireMessages: true, wantErrSubstr: "JSON object"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`, requireMessages: true, wantErrSubstr: "model"},
		{name: "empty messages", body: `{"model":"m","messages":[]}`, requireMessages: true, wantErrSubstr: "messages"},
		{name: "missing messages", body: `{"model":"m"}`, requireMessages: true, wantErrSubstr: "messages"},
		{name: "reject session_id", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"session_id":"0xabc"}`, requireMessages: true, wantErrSubstr: "session_id"},
		{name: "reject model_id", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"model_id":"0x"}`, requireMessages: true, wantErrSubstr: "model_id"},
		{name: "reject chat_id", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"chat_id":"c"}`, requireMessages: true, wantErrSubstr: "chat_id"},
		{name: "reject provider_url", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"provider_url":"http://x"}`, requireMessages: true, wantErrSubstr: "provider_url"},
		{name: "reject node_url", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"node_url":"http://x"}`, requireMessages: true, wantErrSubstr: "node_url"},
		{name: "embeddings no messages ok", body: `{"model":"m","input":"hi"}`, requireMessages: false, wantModel: "m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := validateInferenceBody([]byte(tc.body), tc.requireMessages)
			if tc.wantErrSubstr != "" {
				if errMsg == "" || !strings.Contains(errMsg, tc.wantErrSubstr) {
					t.Fatalf("errMsg=%q want substr %q", errMsg, tc.wantErrSubstr)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected err: %s", errMsg)
			}
			if got != tc.wantModel {
				t.Fatalf("model=%q want %q", got, tc.wantModel)
			}
		})
	}
}

func TestChatRejectsBeforeSessionOpen(t *testing.T) {
	ts, cfg, opened := newTestServer(t)
	master := keymaker.MasterKey(cfg.APIKeySeed)

	badBodies := []string{
		`[]`,
		`{"model":"test-model"}`,
		`{"model":"test-model","messages":[]}`,
		`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"session_id":"0xhijack"}`,
		`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"provider_url":"http://evil"}`,
	}
	for _, body := range badBodies {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+master)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: status %d want 400 (%s)", body, resp.StatusCode, raw)
		}
		if opened.Load() != 0 {
			t.Fatalf("body %s opened a session (count=%d)", body, opened.Load())
		}
	}
}

func newRouterWithChat(t *testing.T, opened *atomic.Int32, chat http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": "0xsession1"})
	})
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("GET /healthcheck", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	})
	mux.HandleFunc("GET /blockchain/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eth":1,"mor":1}`))
	})
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	return httptest.NewServer(mux)
}

func newServerAgainstRouter(t *testing.T, rt *httptest.Server, seed string) (*httptest.Server, *config.Config) {
	t.Helper()
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)
	cfg := &config.Config{
		RouterURL:          rt.URL,
		RouterUser:         "admin",
		RouterPass:         "routerpass",
		AdminPassword:      "adminpass",
		APIKeySeed:         seed,
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
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, cfg.SessionDurationSec, true, false), rc, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, cfg
}

func TestProviderErrorDoesNotOpenSecondSession(t *testing.T) {
	var opened atomic.Int32
	rt := newRouterWithChat(t, &opened, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider exploded", http.StatusBadGateway)
	})
	t.Cleanup(rt.Close)
	ts, cfg := newServerAgainstRouter(t, rt, "provider-err-seed")
	master := keymaker.MasterKey(cfg.APIKeySeed)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected non-OK when provider returns 502")
	}
	if opened.Load() != 1 {
		t.Fatalf("OpenSession count=%d want 1 (no blind replay on provider 5xx)", opened.Load())
	}
}

func TestPartialSSEDoesNotOpenSecondSession(t *testing.T) {
	var opened atomic.Int32
	rt := newRouterWithChat(t, &opened, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	})
	t.Cleanup(rt.Close)
	ts, cfg := newServerAgainstRouter(t, rt, "partial-sse-seed")
	master := keymaker.MasterKey(cfg.APIKeySeed)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if opened.Load() != 1 {
		t.Fatalf("OpenSession count=%d want 1 (partial SSE must not reopen)", opened.Load())
	}
}

func TestSessionDeadDoesOpenSecondSession(t *testing.T) {
	var opened atomic.Int32
	var chats atomic.Int32
	rt := newRouterWithChat(t, &opened, func(w http.ResponseWriter, r *http.Request) {
		n := chats.Add(1)
		if n == 1 {
			http.Error(w, "session expired", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	t.Cleanup(rt.Close)
	ts, cfg := newServerAgainstRouter(t, rt, "session-dead-seed")
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
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if opened.Load() != 2 {
		t.Fatalf("OpenSession count=%d want 2 (proven session-dead may reopen)", opened.Load())
	}
}
