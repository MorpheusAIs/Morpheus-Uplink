package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPEmptyCIDRUsesRemoteAddr(t *testing.T) {
	lo, err := NewLockout(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 192.0.2.1")
	if got := lo.ClientIP(req); got != "203.0.113.9" {
		t.Fatalf("got %q want RemoteAddr host", got)
	}
}

func TestClientIPUntrustedPeerIgnoresXFF(t *testing.T) {
	lo, err := NewLockout([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:99" // not in trusted
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := lo.ClientIP(req); got != "203.0.113.9" {
		t.Fatalf("untrusted peer: got %q", got)
	}
}

func TestClientIPSpoofPrependDoesNotShift(t *testing.T) {
	lo, err := NewLockout([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:443" // trusted proxy
	// Attacker prepends; rightmost after stripping trusted hops is real client.
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.50, 10.0.0.2")
	if got := lo.ClientIP(req); got != "203.0.113.50" {
		t.Fatalf("spoof-prepend: got %q want 203.0.113.50", got)
	}
}

func TestClientIPSpoofRightmostTrustedStripped(t *testing.T) {
	lo, err := NewLockout([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:443"
	// Spoof-rightmost places a trusted hop at the end; must strip and take new rightmost.
	req.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.9")
	if got := lo.ClientIP(req); got != "203.0.113.77" {
		t.Fatalf("spoof-rightmost: got %q want 203.0.113.77", got)
	}
}
