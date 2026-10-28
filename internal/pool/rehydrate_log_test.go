package pool

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// The "rehydrated" line is logged when the counts change, not on every
// rescan (status polls rescan often).
func TestRehydrateLogsOnlyOnChange(t *testing.T) {
	var buf lockedBuf
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	session := func(id, model string) map[string]any {
		return map[string]any{"Id": id, "ModelAgentId": model, "Stake": "1000",
			"OpenedAt": time.Now().Add(-time.Minute).Unix(), "EndsAt": time.Now().Add(time.Hour).Unix(), "ClosedAt": 0}
	}
	m1 := "0x" + strings.Repeat("a1", 32)
	m2 := "0x" + strings.Repeat("b2", 32)
	rr := &rescanRouter{open: []map[string]any{session("0x01", m1)}}
	p := New(router.New(rr.server(t).URL, "u", "p"), 600)
	const line = "pool: rehydrated "
	scan := func() {
		t.Helper()
		if err := p.RehydrateForce(); err != nil {
			t.Fatal(err)
		}
	}

	scan()
	scan()
	scan()
	if n := buf.count(line); n != 1 {
		t.Fatalf("unchanged rescans logged %d lines, want 1", n)
	}
	rr.mu.Lock()
	rr.open = append(rr.open, session("0x02", m2))
	rr.mu.Unlock()
	scan()
	scan()
	if n := buf.count(line); n != 2 {
		t.Fatalf("after a count change: %d lines, want 2", n)
	}
	if buf.count("rehydrated 2 open on-chain session(s), adopted 2") != 1 {
		t.Fatalf("new counts not logged")
	}
}
