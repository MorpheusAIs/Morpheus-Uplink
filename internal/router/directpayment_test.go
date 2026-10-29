package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The by-bid open (the only open) posts to /blockchain/bids/{id}/session
// with exactly {sessionDuration, failover:false, directPayment:false,
// rejectExisting}.
func TestOpenSessionByBidBodyPinned(t *testing.T) {
	const bid = "0xb0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	var (
		body       map[string]any
		path, meth string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, meth = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"sessionID":"0xs"}`))
	}))
	defer ts.Close()
	c := New(ts.URL, "u", "p")
	if _, err := c.OpenSessionByBid(bid, 600); err != nil {
		t.Fatal(err)
	}
	if meth != http.MethodPost || path != "/blockchain/bids/"+bid+"/session" {
		t.Fatalf("open went to %s %s", meth, path)
	}
	if v, ok := body["directPayment"]; !ok || v != false {
		t.Fatalf("directPayment must be present and false: %v", body)
	}
	// Uplink owns failover: the router's in-prompt failover is always off.
	if v, ok := body["failover"]; !ok || v != false {
		t.Fatalf("failover must be present and false: %v", body)
	}
	if v, ok := body["sessionDuration"]; !ok || v != float64(600) {
		t.Fatalf("sessionDuration: %v", body)
	}
	if v, ok := body["rejectExisting"]; !ok || v != false {
		t.Fatalf("rejectExisting must be present and false: %v", body)
	}
	// Staking-only body: nothing beyond these fields is sent.
	if len(body) != 4 {
		t.Fatalf("unexpected open body fields: %v", body)
	}
}

// With rejectExisting the flag is sent, and the router's 409 naming the
// live session is an *ExistingSessionError; without it a 409 stays a
// plain status error.
func TestOpenSessionByBidRejectExisting(t *testing.T) {
	const (
		bid      = "0xb0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
		existing = "0x5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	)
	var body map[string]any
	answer := `{"existingSessionID":"` + existing + `"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(answer))
	}))
	defer ts.Close()
	c := New(ts.URL, "u", "p")
	_, err := c.OpenSessionByBidCtx(context.Background(), bid, 600, true)
	var ex *ExistingSessionError
	if !errors.As(err, &ex) || ex.SessionID != existing {
		t.Fatalf("err = %v, want ExistingSessionError naming %s", err, existing)
	}
	if v, ok := body["rejectExisting"]; !ok || v != true {
		t.Fatalf("rejectExisting must be true: %v", body)
	}
	if _, err := c.OpenSessionByBidCtx(context.Background(), bid, 600, false); errors.As(err, &ex) || err == nil {
		t.Fatalf("flag off: err = %v, want a plain status error", err)
	}
	for _, bad := range []string{`{"existingSessionID":"0x12"}`, `{"error":"conflict"}`, `not json`} {
		answer = bad
		if _, err := c.OpenSessionByBidCtx(context.Background(), bid, 600, true); errors.As(err, &ex) || err == nil {
			t.Fatalf("answer %s: err = %v, want a plain status error", bad, err)
		}
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
