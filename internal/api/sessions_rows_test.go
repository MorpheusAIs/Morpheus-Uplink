package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
)

// Sessions page: lease state (in flight, holders, long, opened, ends) is
// merged into the on-chain rows by session id; a pooled session the chain
// scan has not seen yet is appended; ends falls back to the lease.
func TestSessionRowsMerge(t *testing.T) {
	now := time.Now()
	opened, ends := now.Add(-time.Minute), now.Add(time.Hour)
	chain := []pool.ChainSession{
		{ModelID: "0xm1", SessionID: "0xAA", StakeWei: "1", EndsAt: ends, Usable: true, Pooled: true},
		{ModelID: "0xm2", SessionID: "0xbb", StakeWei: "2", Usable: true},
	}
	snaps := []pool.Snapshot{
		{ModelID: "0xm1", SessionID: "0xaa", ExpiresAt: ends, InFlight: 2, HolderCount: 1, Holders: []string{"k1"}, OpenedAt: opened, EndsAt: ends.Add(time.Second), Opener: "k1"},
		{ModelID: "0xm3", SessionID: "0xcc", ExpiresAt: now.Add(time.Minute), OpenedAt: opened, EndsAt: now.Add(2 * time.Minute)},
	}
	rows := sessionRows(chain, snaps, now)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	a, b, c := rows[0], rows[1], rows[2]
	if a.InFlight != 2 || a.HolderCount != 1 || len(a.Holders) != 1 || a.Opener != "k1" || a.OpenedAt == nil || !a.EndsAt.Equal(ends) || a.StakeWei != "1" {
		t.Errorf("merged row %+v", a)
	}
	if b.InFlight != 0 || b.OpenedAt != nil || b.Holders == nil || !b.EndsAt.IsZero() {
		t.Errorf("chain-only row %+v", b)
	}
	if c.SessionID != "0xcc" || !c.Pooled || !c.Usable || !c.EndsAt.Equal(snaps[1].EndsAt) || c.OpenedAt == nil {
		t.Errorf("pool-only row %+v", c)
	}
	raw, _ := json.Marshal(a)
	for _, k := range []string{`"modelId"`, `"sessionId"`, `"stakeWei"`, `"endsAt"`, `"usable"`, `"pooled"`, `"inFlight"`, `"holderCount"`, `"holders"`, `"openedAt"`, `"opener"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("row JSON lacks %s: %s", k, raw)
		}
	}
}

// /admin/status carries the lease state on "sessions" only; the separate
// poolSessions list is gone.
func TestStatusSessionsCarryLeaseState(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("chat %d %s", code, body)
	}
	code, body := e.admin(t, "GET", "/admin/status", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var st map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st["poolSessions"]; ok {
		t.Fatal("poolSessions still in /admin/status")
	}
	var rows []sessionRow
	if err := json.Unmarshal(st["sessions"], &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Pooled && r.OpenedAt != nil && !r.EndsAt.IsZero() && r.Holders != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("no pooled row with lease state: %s", st["sessions"])
	}
}

// A pooled slot the chain does not list is shown only while live: an
// expired one, or one with no lease state (a closed session's stale
// slot), adds no row.
func TestSessionRowsSkipStalePoolSlots(t *testing.T) {
	now := time.Now()
	snaps := []pool.Snapshot{
		{ModelID: "0xm1", SessionID: "0xghost", ExpiresAt: now.Add(time.Hour)},
		{ModelID: "0xm2", SessionID: "0xexpired", ExpiresAt: now.Add(-time.Second), OpenedAt: now.Add(-time.Hour), EndsAt: now.Add(time.Minute)},
		{ModelID: "0xm3", SessionID: "0xlive", ExpiresAt: now.Add(time.Hour), OpenedAt: now, EndsAt: now.Add(time.Hour)},
	}
	rows := sessionRows(nil, snaps, now)
	if len(rows) != 1 || rows[0].SessionID != "0xlive" {
		t.Fatalf("rows %+v", rows)
	}
}

// Close then list: after a pooled session is closed (here by the rescan,
// as past its end on chain), /admin/status lists no row for it and the
// pool keeps no slot.
func TestStatusNoGhostRowAfterClose(t *testing.T) {
	m := newFakeMarket(t, provA)
	e := newFailoverEnv(t, m, nil)
	if code, body := e.chat(t, false); code != 200 {
		t.Fatalf("chat %d %s", code, body)
	}
	m.mu.Lock()
	var id string
	for k, s := range m.sessions {
		id = k
		s.ends = time.Now().Add(-time.Minute).Unix()
	}
	m.mu.Unlock()
	if err := e.srv.pool.RehydrateForce(); err != nil {
		t.Fatal(err)
	}
	if _, closes := m.snapshot(); len(closes) != 1 || closes[0] != id {
		t.Fatalf("closes %v", closes)
	}
	code, body := e.admin(t, "GET", "/admin/status", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var st struct {
		Sessions []sessionRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Sessions) != 0 {
		t.Fatalf("ghost rows after close: %+v", st.Sessions)
	}
	if snap := e.srv.pool.Snapshot(); len(snap) != 0 {
		t.Fatalf("pool slot kept after close: %+v", snap)
	}
}
