package router

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The open-session body always carries "directPayment": false.
func TestOpenSessionNeverDirectPayment(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"sessionID":"0xs"}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "u", "p")
	if _, err := c.OpenSession("0xmodel", OpenSessionRequest{SessionDuration: 600, Failover: true}); err != nil {
		t.Fatal(err)
	}
	if v, ok := body["directPayment"]; !ok || v != false {
		t.Fatalf("directPayment must be present and false: %v", body)
	}
}

// Request-body guard: no router call (sessions, bids, models or anything
// else) may ask for direct payment; the request never leaves the client.
func TestGuardRefusesDirectPaymentBodies(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "u", "p")
	paths := []string{"/blockchain/sessions", "/blockchain/bids/0xbid/session", "/blockchain/models/0xm/session", "/anything"}
	bodies := []any{
		map[string]any{"directPayment": true},
		map[string]any{"DirectPayment": "true"},
		map[string]any{"stake": "1", "nested": map[string]any{"directPayment": true}},
		[]any{map[string]any{"isDirectPaymentFromUser": true}},
		map[string]any{"direct_payment": 1},
	}
	for _, p := range paths {
		for _, b := range bodies {
			if err := c.doJSON(http.MethodPost, p, b, nil); !errors.Is(err, ErrDirectPayment) {
				t.Fatalf("%s %v: err=%v, want ErrDirectPayment", p, b, err)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("guarded requests reached the router: %d", hits.Load())
	}
	// Explicit false and unrelated bodies pass.
	for _, b := range []any{map[string]any{"directPayment": false}, map[string]any{"sessionDuration": 600}, nil} {
		if err := c.doJSON(http.MethodPost, "/blockchain/models/0xm/session", b, nil); err != nil {
			t.Fatalf("%v: %v", b, err)
		}
	}
}
