package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// A close drops the model's pool slot too (ids compared without case), so
// no stale slot is listed or reused after it.
func TestNotifyClosedDropsPoolSlot(t *testing.T) {
	p := New(router.New("http://127.0.0.1:1", "u", "p"), 600)
	now := time.Now()
	p.mu.Lock()
	p.entries["m1"] = entry{sessionID: "0xAbCdEf", expiresAt: now.Add(time.Hour)}
	p.newStateLocked("m1", "0xAbCdEf", now, now.Add(time.Hour), "")
	p.entries["m2"] = entry{sessionID: "0x2222", expiresAt: now.Add(time.Hour)}
	p.newStateLocked("m2", "0x2222", now, now.Add(time.Hour), "")
	p.mu.Unlock()
	p.notifyClosed("0xabcdEF")
	snap := p.Snapshot()
	if len(snap) != 1 || snap[0].SessionID != "0x2222" {
		t.Fatalf("snapshot after close %+v", snap)
	}
}

// rescanRouter lists the given open sessions, or fails the list.
type rescanRouter struct {
	mu   sync.Mutex
	open []map[string]any
	fail bool
}

func (r *rescanRouter) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallet", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"address":"0x1111111111111111111111111111111111111111"}`))
	})
	mux.HandleFunc("GET /blockchain/sessions/user", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			http.Error(w, `{"error":"unavailable"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": r.open})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// A successful scan prunes pool slots whose session is not open on chain,
// except one with a call in flight or one opened after the scan began; a
// failed scan prunes nothing.
func TestRescanPrunesClosedSlots(t *testing.T) {
	model := "0xabc123abc123abc123abc123abc123abc123abc123abc123abc123abc123abcd"
	rr := &rescanRouter{open: []map[string]any{{"Id": "0xONCHAIN", "ModelAgentId": model, "Stake": "1000",
		"OpenedAt": time.Now().Add(-time.Minute).Unix(), "EndsAt": time.Now().Add(time.Hour).Unix(), "ClosedAt": 0}}}
	p := New(router.New(rr.server(t).URL, "u", "p"), 600)
	now := time.Now()
	past, future := now.Add(-5*time.Minute), now.Add(time.Hour)
	p.mu.Lock()
	p.entries["gone"] = entry{sessionID: "0xgone", expiresAt: future}
	p.newStateLocked("gone", "0xgone", past, future, "")
	p.entries["nostate"] = entry{sessionID: "0xnostate", expiresAt: future}
	p.entries["busy"] = entry{sessionID: "0xbusy", expiresAt: future}
	p.newStateLocked("busy", "0xbusy", past, future, "").inflight = 1
	p.entries["fresh"] = entry{sessionID: "0xfresh", expiresAt: future}
	p.newStateLocked("fresh", "0xfresh", now.Add(time.Hour), future, "")
	p.entries[normModel(model)] = entry{sessionID: "0xonchain", expiresAt: future}
	p.newStateLocked(normModel(model), "0xonchain", past, future, "")
	p.mu.Unlock()
	slots := func() map[string]bool {
		out := map[string]bool{}
		for _, s := range p.Snapshot() {
			out[s.SessionID] = true
		}
		return out
	}

	rr.mu.Lock()
	rr.fail = true
	rr.mu.Unlock()
	if err := p.RehydrateForce(); err == nil {
		t.Fatal("scan error not reported")
	}
	if got := slots(); len(got) != 5 {
		t.Fatalf("failed scan pruned: %v", got)
	}

	rr.mu.Lock()
	rr.fail = false
	rr.mu.Unlock()
	if err := p.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	got := slots()
	for _, id := range []string{"0xgone", "0xnostate"} {
		if got[id] {
			t.Errorf("%s not pruned: %v", id, got)
		}
	}
	for _, id := range []string{"0xbusy", "0xfresh", "0xonchain"} {
		if !got[id] {
			t.Errorf("%s pruned: %v", id, got)
		}
	}
}
