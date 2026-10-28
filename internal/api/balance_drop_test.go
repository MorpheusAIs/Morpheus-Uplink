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
	srv := NewServer(cfg, st, catalog.New("http://127.0.0.1:9"), pool.New(rc, 600), rc, nil)
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

// The stdout audit line of an event with no reason code carries the actor
// and the message, so a balance_drop line shows the amount; a line with a
// reason keeps the reason and leaves the message out.
func TestAuditLogLineActorAndMessage(t *testing.T) {
	logs := captureSlog(t)
	a := &auditSink{ring: NewAuditLog(10)}
	a.WriteAudit(AuditStreamHighValue, AuditEvent{Kind: balanceDropKind, Actor: "router-wallet",
		Message: "MOR balance decreased by 1.5 MOR " + morDropNote})
	a.WriteAudit(AuditStreamOps, AuditEvent{Kind: "admin_login", Actor: "10.0.0.1", IP: "10.0.0.1",
		Result: "fail", Message: "admin_login fail bad_password", reason: "bad_password"})
	var drop, login string
	for _, l := range strings.Split(logs.String(), "\n") {
		switch {
		case strings.Contains(l, "event="+balanceDropKind):
			drop = l
		case strings.Contains(l, "event=admin_login"):
			login = l
		}
	}
	if !strings.Contains(drop, "actor=router-wallet") || !strings.Contains(drop, `message="MOR balance decreased by 1.5 MOR`) {
		t.Fatalf("balance_drop line lacks actor or amount: %q", drop)
	}
	if !strings.Contains(login, "reason=bad_password") || strings.Contains(login, "message=") || strings.Contains(login, "actor=") {
		t.Fatalf("reason line: %q", login)
	}
}
