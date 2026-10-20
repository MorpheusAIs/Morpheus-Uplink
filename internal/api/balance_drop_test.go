package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// A MOR drop (session stake lock) is one neutral audit event with the
// amount; no second copy without the amount.
func TestBalanceDropSingleNeutralEvent(t *testing.T) {
	var call atomic.Int32
	bodies := []string{
		`{"eth":"1000000000000000000","mor":"5000000000000000000"}`,
		`{"eth":"999000000000000000","mor":"3500000000000000000"}`,
	}
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(call.Add(1)) - 1
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		_, _ = w.Write([]byte(bodies[i]))
	}))
	t.Cleanup(rt.Close)
	cfg := &config.Config{RouterURL: rt.URL, AdminPassword: "x", APIKeySeed: "s", DataDir: t.TempDir(), SessionDurationSec: 600}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, "u", "p")
	srv := NewServer(cfg, st, catalog.New("http://127.0.0.1:9"), pool.New(rc, 600, true), rc, nil)
	srv.CheckOutflow()
	srv.CheckOutflow()
	var mor, eth []AuditEvent
	for _, e := range srv.audit.Recent(50) {
		if strings.Contains(e.Kind, "outflow") || strings.Contains(e.Message, "outflow") {
			t.Fatalf("event still uses outflow wording: %+v", e)
		}
		if e.Kind == balanceDropKind && strings.HasPrefix(e.Message, "MOR") {
			mor = append(mor, e)
		}
		if e.Kind == balanceDropKind && strings.HasPrefix(e.Message, "ETH") {
			eth = append(eth, e)
		}
	}
	if len(mor) != 1 || mor[0].Message != "MOR balance decreased by 1.5 MOR (session stakes lock and are returned)" {
		t.Fatalf("MOR drop events: %+v", mor)
	}
	if len(eth) != 1 || eth[0].Message != "ETH balance decreased by 0.001 ETH (usually gas)" {
		t.Fatalf("ETH drop events: %+v", eth)
	}
}
