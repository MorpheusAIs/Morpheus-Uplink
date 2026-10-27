package api

import (
	"sort"
	"sync"
	"time"
)

// AuditEvent is a redacted security-relevant event (no secrets).
type AuditEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor,omitempty"`
	IP      string    `json:"ip,omitempty"`
	Result  string    `json:"result,omitempty"`
	Message string    `json:"message"`
	// Count is set on an aggregated summary line: how many more events of
	// this kind and key the window held.
	Count int `json:"count,omitempty"`
	// Stream is where the event was written (security or ops).
	Stream AuditStream `json:"stream,omitempty"`
	// reason is the short failure/detail code for the JSON log (not part
	// of the ring's JSON shape).
	reason string
	// quiet: the caller already wrote its own structured log line.
	quiet bool
}

// AuditLog is the in-memory ring behind the Security panel and
// GET /admin/audit. Protected events (high-value, except the aggregated
// loopable ones) live in their own share, so failure/ops lines can never
// evict them; Recent gives them at least half of any answer.
type AuditLog struct {
	mu        sync.Mutex
	cap       int
	protected []AuditEvent // oldest first, at most cap
	other     []AuditEvent // oldest first, at most cap
	now       func() time.Time
}

func NewAuditLog(capacity int) *AuditLog {
	if capacity < 16 {
		capacity = 16
	}
	return &AuditLog{cap: capacity, now: time.Now}
}

// Record and RecordFull add a high-value (protected) event.
func (a *AuditLog) Record(kind, actor, message string) {
	a.RecordFull(kind, actor, "", "", message)
}

func (a *AuditLog) RecordFull(kind, actor, ip, result, message string) {
	if a == nil {
		return
	}
	a.Add(true, AuditEvent{At: a.now().UTC(), Kind: kind, Actor: actor, IP: ip, Result: result, Message: message, Stream: AuditStreamHighValue})
}

// Add appends ev to the protected or the other share.
func (a *AuditLog) Add(protected bool, ev AuditEvent) {
	if a == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = a.now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if protected {
		a.protected = pushCapped(a.protected, ev, a.cap)
	} else {
		a.other = pushCapped(a.other, ev, a.cap)
	}
}

func pushCapped(s []AuditEvent, ev AuditEvent, c int) []AuditEvent {
	if len(s) >= c {
		copy(s, s[1:])
		s[len(s)-1] = ev
		return s
	}
	return append(s, ev)
}

// Recent returns up to limit events, newest first. Protected events get at
// least half of the answer when there are that many.
func (a *AuditLog) Recent(limit int) []AuditEvent {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	total := len(a.protected) + len(a.other)
	if limit <= 0 || limit > a.cap {
		limit = a.cap
	}
	if limit > total {
		limit = total
	}
	reserve := min(len(a.protected), (limit+1)/2)
	nOther := min(len(a.other), limit-reserve)
	nProt := min(len(a.protected), limit-nOther)
	out := make([]AuditEvent, 0, nProt+nOther)
	out = append(out, a.protected[len(a.protected)-nProt:]...)
	out = append(out, a.other[len(a.other)-nOther:]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}
