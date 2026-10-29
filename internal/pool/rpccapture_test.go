package pool

import (
	"flag"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Set by the real-router e2e: a router reply body captured from a live
// open, and its HTTP status.
var (
	openBodyFile = flag.String("open-body", "", "file with a captured router open-error body (e2e only)")
	openBodyCode = flag.Int("open-code", http.StatusInternalServerError, "HTTP status of the captured body")
	openBodyWant = flag.String("open-want", providerFault.String(), "class the captured body must get")
)

var ipv4 = regexp.MustCompile(`[0-9]{1,3}(\.[0-9]{1,3}){3}`)

// TestClassifyCapturedOpenBody classifies a body captured by the
// real-router e2e (unknown-bid open) with the same classifier the pool
// uses, as an open error from a single-URL router with the window retry
// unused. Skipped without -open-body.
func TestClassifyCapturedOpenBody(t *testing.T) {
	if *openBodyFile == "" {
		t.Skip("no -open-body (set by scripts/e2e-real-router.sh)")
	}
	b, err := os.ReadFile(*openBodyFile)
	if err != nil {
		t.Fatal(err)
	}
	err = &router.StatusError{Method: http.MethodPost, Path: "/blockchain/bids/{id}/session", Code: *openBodyCode, Body: string(b)}
	if got := classifyOpenErr(err, time.Second, true); got.String() != *openBodyWant {
		t.Fatalf("captured body (status %d) classifies as %s, want %s: %s", *openBodyCode, got, *openBodyWant, ipv4.ReplaceAllString(router.Redact(string(b)), "<ip>"))
	}
	t.Logf("captured body (status %d) classifies as %s", *openBodyCode, *openBodyWant)
}
