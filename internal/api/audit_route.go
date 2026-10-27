package api

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/auditlog"
)

// AuditStream names where an audit event belongs.
type AuditStream string

const (
	// AuditStreamHighValue (audit-security.jsonl): admin and credential
	// events. Nothing an API key alone can trigger at request speed, except
	// a successful long-session open (bounded by the long-session limits).
	AuditStreamHighValue AuditStream = "security"
	// AuditStreamOps (audit.jsonl): failure noise and agent-triggerable
	// events.
	AuditStreamOps AuditStream = "ops"
)

// Audit file names inside DATA_DIR.
const (
	AuditFileSecurity = "audit-security.jsonl"
	AuditFileOps      = "audit.jsonl"
	// Failure/ops stream retention (fixed; it holds admin_bearer_reject).
	AuditOpsRetention = 30 * 24 * time.Hour
	// Lines of each stream loaded into the ring at boot.
	auditBootLines = 500
)

// opsKinds always go to the failure/ops stream, whatever the result.
var opsKinds = map[string]bool{
	"admin_bearer_reject": true,
	"login_lockout":       true,
	"session_release":     true,
	"provider_failover":   true,
	"session_reconcile":   true,
}

// loopableKinds are high-value but repeatable by a credential holder; they
// are aggregated per minute and live in the ring's evictable share, so they
// cannot rotate out a key_reveal or login line.
var loopableKinds = map[string]bool{
	"admin_key_use":           true,
	"admin_session_ip_change": true,
}

// AuditStreamFor returns the stream for an event kind and result: failure
// and blocked results, and the agent/system kinds above, go to ops;
// everything else (admin and credential actions that succeeded) is
// high-value.
func AuditStreamFor(kind, result string) AuditStream {
	if opsKinds[kind] {
		return AuditStreamOps
	}
	switch result {
	case "ok", "":
		return AuditStreamHighValue
	}
	if loopableKinds[kind] {
		return AuditStreamHighValue
	}
	return AuditStreamOps
}

// AuditSink receives every audit event with its stream.
type AuditSink interface {
	WriteAudit(stream AuditStream, ev AuditEvent)
}

// auditSink writes to the JSON log, the in-memory ring and, when DATA_DIR
// is set, the two persistent streams.
type auditSink struct {
	ring     *AuditLog
	security *auditlog.Stream // nil: ring only
	ops      *auditlog.Stream
}

func (a *auditSink) WriteAudit(stream AuditStream, ev AuditEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	ev.Stream = stream
	if !ev.quiet {
		a.logLine(stream, ev)
	}
	a.ring.Add(stream == AuditStreamHighValue && !loopableKinds[ev.Kind], ev)
	f := a.ops
	if stream == AuditStreamHighValue {
		f = a.security
	}
	if f != nil {
		f.Append(ev) // json.Marshal of the exported fields only
	}
}

func (a *auditSink) logLine(stream AuditStream, ev AuditEvent) {
	attrs := []any{"event", ev.Kind, "result", ev.Result, "ip", ev.IP}
	if ev.reason != "" {
		attrs = append(attrs, "reason", ev.reason)
	}
	if ev.Count > 0 {
		attrs = append(attrs, "count", ev.Count)
	}
	attrs = append(attrs, "stream", string(stream))
	slog.Info("audit", attrs...)
}

// openAuditSink opens the two streams in dataDir (ring only when dataDir is
// empty) and loads the last lines of both into the ring.
func openAuditSink(ring *AuditLog, dataDir string, securityRetention time.Duration) *auditSink {
	a := &auditSink{ring: ring}
	if dataDir == "" {
		return a
	}
	a.security = auditlog.Open(dataDir, AuditFileSecurity, auditlog.Options{Retention: securityRetention})
	a.ops = auditlog.Open(dataDir, AuditFileOps, auditlog.Options{Retention: AuditOpsRetention})
	a.Prune()
	var evs []AuditEvent
	for _, st := range []*auditlog.Stream{a.security, a.ops} {
		for _, line := range st.Tail(auditBootLines) {
			var ev AuditEvent
			if json.Unmarshal(line, &ev) == nil && ev.Kind != "" && !ev.At.IsZero() {
				evs = append(evs, ev)
			}
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	for _, ev := range evs {
		ring.Add(ev.Stream == AuditStreamHighValue && !loopableKinds[ev.Kind], ev)
	}
	return a
}

// Prune deletes rotated audit files past their stream's retention.
func (a *auditSink) Prune() {
	for _, st := range []*auditlog.Stream{a.security, a.ops} {
		if st != nil {
			if n := st.Prune(); n > 0 {
				slog.Info("audit retention: pruned rotated files", "count", n)
			}
		}
	}
}

// diskStatus reports the audit files' sizes for the Status disk panel.
func (a *auditSink) diskStatus() map[string]any {
	out := map[string]any{"securityBytes": int64(0), "opsBytes": int64(0), "persistent": false}
	if a.security != nil {
		out["securityBytes"] = a.security.Bytes()
		out["opsBytes"] = a.ops.Bytes()
		out["persistent"] = !a.security.Disabled() && !a.ops.Disabled()
	}
	return out
}

// auditAggregator writes a repeatable event at most once per window per
// (event, key): the first occurrence is written at once; repeats within the
// window are counted and written as one summary line with a count when the
// window ends. Distinct keys are bounded per event and in total; beyond the
// bound, new keys share one "other" bucket per event, so rotating the key
// (for example the client address) cannot grow memory or the line rate.
type auditAggregator struct {
	mu         sync.Mutex
	window     time.Duration
	maxPerKind int
	maxTotal   int
	now        func() time.Time
	emit       func(stream AuditStream, ev AuditEvent)
	buckets    map[aggKey]*aggBucket
	perKind    map[string]int
	keyed      int      // buckets other than "other"
	queue      []aggKey // buckets in start order (flush scans the front only)
}

type aggKey struct{ kind, key string }

type aggBucket struct {
	start      time.Time
	suppressed int
	last       AuditEvent
	stream     AuditStream
}

const aggOtherKey = "\x00other"

func newAuditAggregator(window time.Duration, maxPerKind, maxTotal int, emit func(AuditStream, AuditEvent)) *auditAggregator {
	return &auditAggregator{
		window:     window,
		maxPerKind: maxPerKind,
		maxTotal:   maxTotal,
		now:        time.Now,
		emit:       emit,
		buckets:    map[aggKey]*aggBucket{},
		perKind:    map[string]int{},
	}
}

// Record writes ev now, or counts it if (ev.Kind, key) was already written
// in the current window. key groups events and is never logged.
func (a *auditAggregator) Record(stream AuditStream, key string, ev AuditEvent) {
	a.mu.Lock()
	now := a.now()
	out := a.flushLocked(now)
	k := aggKey{ev.Kind, key}
	b := a.buckets[k]
	if b == nil && (a.perKind[ev.Kind] >= a.maxPerKind || a.keyed >= a.maxTotal) {
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
	a.queue = append(a.queue, k)
	if k.key != aggOtherKey {
		a.perKind[ev.Kind]++
		a.keyed++
	}
	a.mu.Unlock()
	if ev.At.IsZero() {
		ev.At = now.UTC()
	}
	a.write(append(out, pending{stream, ev}))
}

// Flush writes the summaries of every window that has ended.
func (a *auditAggregator) Flush() {
	a.mu.Lock()
	out := a.flushLocked(a.now())
	a.mu.Unlock()
	a.write(out)
}

// Size is the number of live buckets ("other" included).
func (a *auditAggregator) Size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.buckets)
}

type pending struct {
	stream AuditStream
	ev     AuditEvent
}

func (a *auditAggregator) flushLocked(now time.Time) []pending {
	var out []pending
	i := 0
	for ; i < len(a.queue); i++ {
		k := a.queue[i]
		b := a.buckets[k]
		if now.Sub(b.start) < a.window {
			break
		}
		if b.suppressed > 0 {
			ev := b.last
			ev.At = now.UTC()
			ev.Count = b.suppressed
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
			a.keyed--
		}
	}
	if i > 0 {
		a.queue = append(a.queue[:0:0], a.queue[i:]...)
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

// Aggregation bounds. Loopable high-value events (admin_key_use,
// admin_session_ip_change): one line per key per minute, at most
// aggMaxKeys keys per event plus one "other" line, so a credential holder
// cannot flood the high-value file. Failure/ops events: per (IP or key id,
// event) per minute, about 10,000 keys in total plus one "other" per event.
const (
	aggWindow     = time.Minute
	aggMaxKeys    = 64
	opsAggMaxKeys = 10000
)
