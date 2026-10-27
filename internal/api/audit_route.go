package api

import (
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// AuditStream names where an audit event belongs. Today every event goes
// to the in-memory ring and the JSON log; a persistent audit can implement
// AuditSink and route by stream (high-value vs failure/ops).
type AuditStream string

const (
	// AuditStreamHighValue: admin and credential events.
	AuditStreamHighValue AuditStream = "security"
	// AuditStreamOps: failure noise and agent-triggerable events.
	AuditStreamOps AuditStream = "ops"
)

// auditStreams assigns events that need a fixed stream. Unlisted events
// default to AuditStreamHighValue unless their result is a failure.
var auditStreams = map[string]AuditStream{
	"admin_key_use":           AuditStreamHighValue,
	"admin_session_ip_change": AuditStreamHighValue,
	"step_up":                 AuditStreamHighValue,
	"key_reveal":              AuditStreamHighValue,
}

// AuditStreamFor returns the stream for an event kind and result.
func AuditStreamFor(kind, result string) AuditStream {
	if st, ok := auditStreams[kind]; ok {
		if kind == "step_up" && result != "ok" {
			return AuditStreamOps
		}
		return st
	}
	switch result {
	case "fail", "blocked":
		return AuditStreamOps
	}
	return AuditStreamHighValue
}

// AuditSink receives every audit event with its stream.
type AuditSink interface {
	WriteAudit(stream AuditStream, ev AuditEvent)
}

// ringSink is the current sink: the JSON log plus the in-memory ring.
type ringSink struct{ ring *AuditLog }

func (r ringSink) WriteAudit(stream AuditStream, ev AuditEvent) {
	attrs := []any{"event", ev.Kind, "result", ev.Result, "ip", ev.IP}
	if ev.reason != "" {
		attrs = append(attrs, "reason", ev.reason)
	}
	attrs = append(attrs, "stream", string(stream))
	slog.Info("audit", attrs...)
	r.ring.RecordFull(ev.Kind, ev.Actor, ev.IP, ev.Result, ev.Message)
}

// auditAggregator writes a loopable event at most once per window per
// (event, key): the first occurrence is written at once; repeats within the
// window are counted and written as one summary line (count=N) when the
// window ends. Distinct keys per event are capped; beyond the cap, new keys
// share one "other" bucket, so rotating the key (for example the client
// address) cannot grow memory or the line rate.
type auditAggregator struct {
	mu      sync.Mutex
	window  time.Duration
	maxKeys int // per event
	now     func() time.Time
	emit    func(stream AuditStream, ev AuditEvent)
	buckets map[aggKey]*aggBucket
	perKind map[string]int
}

type aggKey struct{ kind, key string }

type aggBucket struct {
	start      time.Time
	suppressed int
	last       AuditEvent
	stream     AuditStream
}

const aggOtherKey = "\x00other"

func newAuditAggregator(window time.Duration, maxKeys int, emit func(AuditStream, AuditEvent)) *auditAggregator {
	return &auditAggregator{
		window:  window,
		maxKeys: maxKeys,
		now:     time.Now,
		emit:    emit,
		buckets: map[aggKey]*aggBucket{},
		perKind: map[string]int{},
	}
}

// Record writes ev now, or counts it if (ev.Kind, key) was already written
// in the current window. key is used for grouping only and is never logged.
func (a *auditAggregator) Record(stream AuditStream, key string, ev AuditEvent) {
	a.mu.Lock()
	now := a.now()
	out := a.flushLocked(now)
	k := aggKey{ev.Kind, key}
	b := a.buckets[k]
	if b == nil && a.perKind[ev.Kind] >= a.maxKeys {
		k = aggKey{ev.Kind, aggOtherKey}
		b = a.buckets[k]
	}
	if b != nil {
		b.suppressed++
		b.last = ev
		a.mu.Unlock()
		a.write(out)
		return
	}
	a.buckets[k] = &aggBucket{start: now, last: ev, stream: stream}
	if k.key != aggOtherKey {
		a.perKind[ev.Kind]++
	}
	a.mu.Unlock()
	a.write(append(out, pending{stream, ev}))
}

// Flush writes the summaries of every window that has ended.
func (a *auditAggregator) Flush() {
	a.mu.Lock()
	out := a.flushLocked(a.now())
	a.mu.Unlock()
	a.write(out)
}

type pending struct {
	stream AuditStream
	ev     AuditEvent
}

func (a *auditAggregator) flushLocked(now time.Time) []pending {
	var out []pending
	for k, b := range a.buckets {
		if now.Sub(b.start) < a.window {
			continue
		}
		if b.suppressed > 0 {
			ev := b.last
			ev.At = now.UTC()
			rep := "repeated count=" + strconv.Itoa(b.suppressed) + " in " + a.window.String()
			ev.Message = joinMsg(ev.Message, rep)
			ev.reason = joinMsg(ev.reason, rep)
			if k.key == aggOtherKey {
				ev.IP = ""
				ev.Actor = ""
				ev.Message = joinMsg(ev.Kind+" other", rep)
				ev.reason = joinMsg("other", rep)
			}
			out = append(out, pending{b.stream, ev})
		}
		delete(a.buckets, k)
		if k.key != aggOtherKey {
			a.perKind[k.kind]--
		}
	}
	return out
}

func (a *auditAggregator) write(ps []pending) {
	for _, p := range ps {
		a.emit(p.stream, p.ev)
	}
}

func joinMsg(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

// Aggregation bounds for loopable admin events (admin_key_use,
// admin_session_ip_change): one line per key per minute, at most
// aggMaxKeys distinct keys per event per minute plus one "other" line.
const (
	aggWindow  = time.Minute
	aggMaxKeys = 64
)
