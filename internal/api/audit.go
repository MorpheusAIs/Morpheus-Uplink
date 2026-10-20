package api

import (
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
}

// AuditLog is an in-memory ring of recent events for the Security panel.
type AuditLog struct {
	mu  sync.Mutex
	cap int
	ev  []AuditEvent
}

func NewAuditLog(capacity int) *AuditLog {
	if capacity < 16 {
		capacity = 16
	}
	return &AuditLog{cap: capacity, ev: make([]AuditEvent, 0, capacity)}
}

func (a *AuditLog) Record(kind, actor, message string) {
	a.RecordFull(kind, actor, "", "", message)
}

func (a *AuditLog) RecordFull(kind, actor, ip, result, message string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := AuditEvent{
		At:      time.Now().UTC(),
		Kind:    kind,
		Actor:   actor,
		IP:      ip,
		Result:  result,
		Message: message,
	}
	if len(a.ev) >= a.cap {
		copy(a.ev, a.ev[1:])
		a.ev[len(a.ev)-1] = e
		return
	}
	a.ev = append(a.ev, e)
}

func (a *AuditLog) Recent(limit int) []AuditEvent {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > len(a.ev) {
		limit = len(a.ev)
	}
	out := make([]AuditEvent, limit)
	for i := 0; i < limit; i++ {
		out[i] = a.ev[len(a.ev)-1-i]
	}
	return out
}
