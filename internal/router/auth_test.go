package router

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 401 sets the auth-failure state; the next 2xx clears it.
func TestAuthFailureState(t *testing.T) {
	var want atomic.Value
	want.Store("new-pass")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != want.Load().(string) {
			http.Error(w, "invalid username or password", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "old-pass")
	_, err := c.WalletAddress()
	if ErrorKind(err) != KindAuth {
		t.Fatalf("want auth error, got %v (%s)", err, ErrorKind(err))
	}
	if _, _, failing := c.AuthFailure(); !failing {
		t.Fatal("expected auth failure state")
	}

	want.Store("old-pass")
	if _, err := c.WalletAddress(); err != nil {
		t.Fatalf("after router accepts: %v", err)
	}
	if _, _, failing := c.AuthFailure(); failing {
		t.Fatal("auth failure should clear after success")
	}
}

func TestErrorKind(t *testing.T) {
	cases := map[string]error{
		KindAuth:     &StatusError{Code: 401},
		KindNotReady: errors.New("GET /wallet: dial tcp 1.2.3.4:8082: connect: connection refused"),
		KindOther:    &StatusError{Code: 500},
		KindNone:     nil,
	}
	for want, err := range cases {
		if got := ErrorKind(err); got != want {
			t.Errorf("ErrorKind(%v) = %q want %q", err, got, want)
		}
	}
}
