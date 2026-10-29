package router

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	id := "0x" + strings.Repeat("ab", 32)
	in := `failed to get bid: Post "https://x.invalid/v2/KEY": dial; bid ` + id + ` via http://h.invalid:8545?k=KEY2`
	got := Redact(in)
	for _, bad := range []string{"KEY", "x.invalid", "h.invalid", id} {
		if strings.Contains(got, bad) {
			t.Fatalf("Redact kept %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "<url>") || !strings.Contains(got, id[:10]) || !strings.HasPrefix(got, "failed to get bid: Post ") {
		t.Fatalf("Redact = %q", got)
	}
	se := &StatusError{Method: "POST", Path: "/blockchain/bids/" + id + "/session", Code: 500, Body: in}
	if strings.Contains(se.Error(), "KEY") || strings.Contains(se.Error(), id) {
		t.Fatalf("StatusError.Error() = %q", se.Error())
	}
	if se.Body != in {
		t.Fatal("Body changed")
	}
	if SafeError(nil) != "" {
		t.Fatal("SafeError(nil)")
	}
}
