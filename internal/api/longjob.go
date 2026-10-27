package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Long-job core (S5): per-call timeout headers, server read/write
// deadlines and the owner-scoped release.

// Test seams: production values. Tests shrink them.
var (
	// timeUnit scales the header seconds into call timers.
	timeUnit = time.Second
	// bodyReadDeadline bounds reading an inference request body.
	bodyReadDeadline = 60 * time.Second
	// writeDeadline is set before every Write and Flush of a response.
	writeDeadline = 60 * time.Second
	// testHookAfterLease runs right after an inference request took its
	// lease (tests: panics, cancels).
	testHookAfterLease atomic.Pointer[func()]
)

const (
	requestTimeoutDef = 600 // seconds: the router call total
	idleTimeoutMin    = 30
	idleTimeoutMax    = 600
	releaseBodyMax    = 1 << 10
	headerDigitsMax   = 6
)

// NewHTTPServer is Uplink's listener configuration: header read 10 s and
// keep-alive idle 120 s. Inference routes add their own body-read and
// per-write deadlines.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

type ctxKey int

const keyIDCtxKey ctxKey = 1

// withKeyID stores the authenticated key id in the request context.
func withKeyID(r *http.Request, keyID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), keyIDCtxKey, keyID))
}

// keyIDFrom is the key id requireAPIKey authenticated (never a header).
func keyIDFrom(r *http.Request) string {
	v, _ := r.Context().Value(keyIDCtxKey).(string)
	return v
}

func isSharedKey(keyID string) bool {
	return keyID == keymaker.MasterKeyID || keyID == keymaker.PromptKeyID
}

func writeTypedError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "code": status, "message": message}})
}

// strictHeaderInt parses one of the X-Uplink-* integer headers: sent at
// most once, digits only (no sign, no whitespace), at most 6 digits. The
// range check happens on the int, before any time.Duration.
func strictHeaderInt(r *http.Request, name string) (n int, present bool, errMsg string) {
	vals := r.Header.Values(name)
	if len(vals) == 0 {
		return 0, false, ""
	}
	bad := name + " must be a whole number of seconds"
	if len(vals) > 1 {
		return 0, true, name + " must be sent once"
	}
	v := vals[0]
	if v == "" || len(v) > headerDigitsMax {
		return 0, true, bad
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, true, bad
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, true, bad
	}
	return n, true, ""
}

// callLimits are one inference request's effective session and timer values.
type callLimits struct {
	durationSec int // 0: pool default
	totalSec    int // always requestTimeoutDef
	idleSec     int
	anyHeader   bool
}

// parseCallLimits validates the two X-Uplink headers (session duration
// and idle timeout).
func (s *Server) parseCallLimits(r *http.Request) (callLimits, string) {
	dmax := s.durationMax()
	idleDef := s.forwardIdle()
	if idleDef < idleTimeoutMin || idleDef > idleTimeoutMax {
		idleDef = 180
	}
	cl := callLimits{totalSec: requestTimeoutDef, idleSec: idleDef}
	if n, ok, msg := strictHeaderInt(r, "X-Uplink-Session-Duration"); msg != "" {
		return cl, msg
	} else if ok {
		cl.anyHeader = true
		if n < 600 || n > dmax {
			return cl, fmt.Sprintf("X-Uplink-Session-Duration must be 600…%d seconds", dmax)
		}
		cl.durationSec = n
	}
	if n, ok, msg := strictHeaderInt(r, "X-Uplink-Idle-Timeout"); msg != "" {
		return cl, msg
	} else if ok {
		cl.anyHeader = true
		if n <= 0 {
			return cl, "X-Uplink-Idle-Timeout must be at least 1 second"
		}
		cl.idleSec = min(max(n, idleTimeoutMin), idleTimeoutMax)
	}
	return cl, ""
}

// readBodyWithDeadline reads the request body under bodyReadDeadline, then
// clears the read deadline.
func readBodyWithDeadline(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadDeadline))
	b, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		// Keep the expired deadline and drop the connection: clearing it
		// would let the server wait for the rest of a dribbled body.
		w.Header().Set("Connection", "close")
		return b, err
	}
	_ = rc.SetReadDeadline(time.Time{})
	return b, nil
}

// deadlineWriter resets the write deadline before every Write and Flush,
// so the wait between upstream bytes is never cut by a stale deadline,
// while a client that stops reading is cut.
type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func newDeadlineWriter(w http.ResponseWriter) *deadlineWriter {
	return &deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
}

func (d *deadlineWriter) arm() { _ = d.rc.SetWriteDeadline(time.Now().Add(writeDeadline)) }

func (d *deadlineWriter) WriteHeader(code int) { d.arm(); d.ResponseWriter.WriteHeader(code) }

func (d *deadlineWriter) Write(b []byte) (int, error) { d.arm(); return d.ResponseWriter.Write(b) }

func (d *deadlineWriter) Flush() { d.arm(); _ = d.rc.Flush() }

func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// clear removes the write deadline when the response is done.
func (d *deadlineWriter) clear() { _ = d.rc.SetWriteDeadline(time.Time{}) }

// bodyStream reports the request's "stream" field.
func bodyStream(body []byte) bool {
	var v struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Stream
}

func writeTimeout504(w http.ResponseWriter, kind string) {
	writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": map[string]any{
		"type": "timeout", "code": http.StatusGatewayTimeout, "message": "upstream " + kind + " timeout",
	}})
}

// handleSessionRelease removes the calling key's hold on a model's
// session; the session closes only when no holder and no request is left.
func (s *Server) handleSessionRelease(w http.ResponseWriter, r *http.Request) {
	keyID := keyIDFrom(r)
	if keyID == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "missing key identity")
		return
	}
	if !s.cfg.SessionRelease {
		writeTypedError(w, http.StatusForbidden, "release_disabled", "session release is turned off")
		return
	}
	if isSharedKey(keyID) {
		writeTypedError(w, http.StatusForbidden, "shared_key", "shared keys hold no sessions of their own")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, releaseBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Model == "" || dec.More() {
		writeOpenAIError(w, http.StatusBadRequest, `body must be {"model": "<name or id>"} (at most 1 KiB)`)
		return
	}
	modelID, err := s.catalog.Resolve(req.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "unknown model")
		return
	}
	out := s.pool.Release(modelID, keyID)
	result := "closed"
	switch {
	case !out.Released:
		result = "not_held"
	case !out.Closed:
		result = out.Reason
	}
	s.auditAgent("session_release", keyID, "", result, s.modelLabel(modelID))
	body := map[string]any{"released": out.Released}
	if out.Released {
		body["closed"] = out.Closed
	}
	if out.Reason != "" {
		body["reason"] = out.Reason
	}
	writeJSON(w, http.StatusOK, body)
}

// acquireSession wraps pool.Acquire with its typed error answers.
func (s *Server) acquireSession(w http.ResponseWriter, keyID, reqModel, modelID string, cl callLimits) (*pool.Lease, pool.AcquireResult, bool) {
	lease, res, err := s.pool.Acquire(modelID, pool.AcquireOpts{KeyID: keyID, DurationSec: cl.durationSec})
	if errors.Is(err, pool.ErrNoEligibleProvider) {
		// Only providers excluded by UPLINK_PROVIDER_EXCLUDE serve the model.
		writeTypedError(w, http.StatusServiceUnavailable, "no_eligible_provider", "no eligible provider for this model")
		return nil, res, false
	}
	if errors.Is(err, pool.ErrNoAvailableProvider) {
		// Every eligible provider is avoided right now: nothing was opened.
		s.auditProviderUnavailable(modelID, "no_available")
		writeNoAvailableProvider(w, err)
		return nil, res, false
	}
	if err != nil {
		warnf("api: open session for %s (%s): %v", reqModel, modelID, err)
		writeOpenAIError(w, http.StatusBadGateway, "failed to open session") // detail logged above; it names router internals
		return nil, res, false
	}
	if res.Opened {
		s.store.TrackSessionOpen(res.SessionID, keyID, modelID)
	}
	return lease, res, true
}

// forwardOpts turns the call limits into router timers.
func forwardOpts(cl callLimits, stream bool) router.ForwardOptions {
	return router.ForwardOptions{
		Idle:   time.Duration(cl.idleSec) * timeUnit,
		Total:  time.Duration(cl.totalSec) * timeUnit,
		Stream: stream,
	}
}

// setTimeoutHeaders writes the effective values (no ids) when the client
// sent any of the X-Uplink headers; without them responses are unchanged.
func setTimeoutHeaders(w http.ResponseWriter, cl callLimits) {
	if !cl.anyHeader {
		return
	}
	w.Header().Set("X-Uplink-Timeouts", "idle="+strconv.Itoa(cl.idleSec)+";total="+strconv.Itoa(cl.totalSec))
}

// writeNoAvailableProvider answers ErrNoAvailableProvider: a typed 503 with
// a fixed message (no ids) and, when a cool-down is known, Retry-After in
// whole seconds.
func writeNoAvailableProvider(w http.ResponseWriter, err error) {
	var na *pool.NoAvailableError
	if errors.As(err, &na) && na.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(na.RetryAfter/time.Second)))
	}
	writeTypedError(w, http.StatusServiceUnavailable, "no_available_provider", "no provider is available for this model right now; retry later")
}

// auditProviderUnavailable records a fail-fast refusal on the ops stream,
// aggregated per resolved model per minute (never the request's own model
// string): model, outcome and, on the summary line, a count.
func (s *Server) auditProviderUnavailable(modelID, outcome string) {
	label := s.modelLabel(modelID)
	s.opsAgg.Record(AuditStreamOps, label, AuditEvent{
		At: time.Now().UTC(), Kind: "provider_unavailable", Result: outcome,
		Message: "provider_unavailable " + outcome + " " + label,
	})
}
