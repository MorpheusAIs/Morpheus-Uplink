package pool

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// The router can quote the RPC URL (and so its key) in an open error. The
// URL is masked in Error() and in the logs; the classifier still reads the
// raw body, so the class does not change; full bid and session ids stay
// out of the logs.
func TestOpenErrorURLMaskedClassUnchanged(t *testing.T) {
	const text = `failed to get bid: Post "https://x.invalid/v2/KEY": 429 Too Many Requests`
	raw := status500(text)
	var se *router.StatusError
	if !errors.As(raw, &se) || !strings.Contains(se.Body, "x.invalid/v2/KEY") {
		t.Fatalf("raw body changed: %+v", se)
	}
	msg := raw.Error()
	if strings.Contains(msg, "KEY") || strings.Contains(msg, "x.invalid") || !strings.Contains(msg, "<url>") {
		t.Fatalf("Error() = %q: want the URL masked", msg)
	}
	if got := classifyOpenErr(raw, time.Second, true); got != transientPreSend {
		t.Fatalf("class %s, want %s", got, transientPreSend)
	}
	if got := classifyOpenErr(errors.New("status 500: "+msg), time.Second, true); got != maybeSent {
		t.Fatalf("plain error class %s, want maybe_sent (only the raw body is read)", got)
	}

	logs := captureLogs(t)
	_, lr, p, _ := rpcEnv(t)
	threeBidders(lr, modelB)
	lr.setOpenFault(faultBurst(1, routerErr(text), false))
	_, res := mustAcquire(t, p, modelB, AcquireOpts{})
	if res.SessionID == "" {
		t.Fatalf("no session after one masked 429: %+v", res)
	}
	out := logs.String()
	if logs.count("pool: open failed") == 0 || !strings.Contains(out, "class="+transientPreSend.String()) {
		t.Fatalf("open failure not logged as %s:\n%s", transientPreSend, out)
	}
	if strings.Contains(out, "KEY") || strings.Contains(out, "x.invalid") {
		t.Fatalf("logs carry the RPC URL:\n%s", out)
	}
	for _, id := range append([]string{res.SessionID}, lr.bidsTried()...) {
		if len(id) > 10 && strings.Contains(out, id) {
			t.Fatalf("logs carry a full id %s:\n%s", short(id), out)
		}
	}
}

func TestCutKindNamesNoPath(t *testing.T) {
	id := bidID(modelA, leaseProv1)
	se := &router.StatusError{Method: "POST", Path: "/blockchain/sessions/" + id + "/close", Code: 504, Body: "gateway timeout"}
	if got := cutKind(se); got != "status_504" {
		t.Fatalf("cutKind = %q", got)
	}
	if strings.Contains(se.Error(), id) || !strings.Contains(se.Error(), short(id)) {
		t.Fatalf("Error() = %q: want the short id only", se.Error())
	}
}
