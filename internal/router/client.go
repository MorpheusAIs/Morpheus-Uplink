// Package router is a thin client for the proxy-router (C-Node) admin API
// on :8082. All requests use HTTP Basic Auth (COOKIE_CONTENT credentials).
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL string
	// HTTP carries the admin calls (sessions, bids, balance, health): a
	// fixed 60 s timeout, independent of any inference call.
	HTTP *http.Client
	// ForwardHTTP carries inference calls. It has no Client.Timeout: each
	// call is bounded by its own idle and total timers (ForwardOptions).
	ForwardHTTP *http.Client

	mu   sync.Mutex
	user string
	pass string
	// First 401 of the current failure streak (zero when the last
	// authenticated call succeeded).
	authFailSince time.Time
	authFailMsg   string
	authOKAt      time.Time
	now           func() time.Time
}

// AdminTimeout bounds every non-inference router call.
const AdminTimeout = 60 * time.Second

func New(baseURL, user, pass string) *Client {
	return &Client{
		BaseURL: baseURL,
		user:    user,
		pass:    pass,
		HTTP:    &http.Client{Timeout: AdminTimeout},
		// No Client.Timeout: bounded per call by ForwardOptions.
		ForwardHTTP: &http.Client{},
		now:         time.Now,
	}
}

// User returns the router user name (never the password).
func (c *Client) User() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.user
}

func (c *Client) creds() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.user, c.pass
}

func (c *Client) noteAuth(status int, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if status == http.StatusUnauthorized {
		if c.authFailSince.IsZero() {
			c.authFailSince = c.now()
		}
		c.authFailMsg = msg
		return
	}
	if status >= 200 && status < 300 {
		c.authFailSince = time.Time{}
		c.authFailMsg = ""
		c.authOKAt = c.now()
	}
}

// AuthFailure reports whether the router is currently rejecting the
// credential (HTTP 401), since when, and the router's message.
func (c *Client) AuthFailure() (since time.Time, msg string, failing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authFailSince, c.authFailMsg, !c.authFailSince.IsZero()
}

// StatusError is a non-2xx router reply.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// Error kinds for operator-facing messages.
const (
	KindNone     = ""
	KindAuth     = "router_auth"      // router rejected the credential
	KindNotReady = "router_not_ready" // router unreachable / starting
	KindOther    = "router_error"
)

// ErrorKind classifies a router client error.
func ErrorKind(err error) string {
	if err == nil {
		return KindNone
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch {
		case se.Code == http.StatusUnauthorized:
			return KindAuth
		case se.Code == http.StatusBadGateway || se.Code == http.StatusServiceUnavailable || se.Code == http.StatusGatewayTimeout:
			return KindNotReady
		}
		return KindOther
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return KindNotReady
	}
	s := strings.ToLower(err.Error())
	for _, n := range []string{"connection refused", "no such host", "i/o timeout", "connection reset", "eof"} {
		if strings.Contains(s, n) {
			return KindNotReady
		}
	}
	return KindOther
}

// ErrorHint is a short operator message for an error kind.
func ErrorHint(kind string) string {
	switch kind {
	case KindAuth:
		return "router rejected Uplink's credential (401); check that ROUTER_AUTH or COOKIE_CONTENT matches the router's COOKIE_CONTENT"
	case KindNotReady:
		return "router not reachable yet (starting or restarting)"
	case KindOther:
		return "router request failed"
	}
	return ""
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	user, pass := c.creds()
	req.SetBasicAuth(user, pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) doJSON(method, path string, reqBody any, out any) error {
	var payload []byte
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		payload = raw
	}
	status, raw, err := c.roundTrip(method, path, payload)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if path != "/healthcheck" {
		c.noteAuth(status, strings.TrimSpace(string(raw)))
	}
	if status < 200 || status >= 300 {
		return &StatusError{Method: method, Path: path, Code: status, Body: string(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: parse response: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) roundTrip(method, path string, payload []byte) (int, []byte, error) {
	// Guard for every session/bid path (and anything else): never ask the
	// router for a direct-payment session.
	if requestsDirectPayment(payload) {
		return 0, nil, ErrDirectPayment
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// OpenSessionRequest is the body for opening a session. There is no
// direct-payment field on purpose: Uplink always opens staking-mode sessions
// (stake locked, returned to the wallet) and always sends
// "directPayment": false explicitly. There is no failover field either:
// Uplink runs provider failover itself (internal/pool), so the router's
// in-prompt failover, which would close and reopen behind the pool, is
// always off ("failover": false).
type OpenSessionRequest struct {
	SessionDuration int `json:"sessionDuration"`
}

// MarshalJSON pins directPayment and failover to false.
func (r OpenSessionRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SessionDuration int  `json:"sessionDuration"`
		Failover        bool `json:"failover"`
		DirectPayment   bool `json:"directPayment"`
	}{r.SessionDuration, false, false})
}

// ErrDirectPayment is returned instead of sending any router request whose
// body asks for direct payment.
var ErrDirectPayment = errors.New("refusing to send a direct-payment request to the router")

// requestsDirectPayment reports whether a JSON body sets directPayment (any
// case, any nesting depth) to true or "true".
func requestsDirectPayment(payload []byte) bool {
	if len(payload) == 0 || !bytes.Contains(bytes.ReplaceAll(bytes.ToLower(payload), []byte("_"), nil), []byte("directpayment")) {
		return false
	}
	var v any
	if json.Unmarshal(payload, &v) != nil {
		// Unparseable body that mentions the field: refuse.
		return true
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if strings.EqualFold(k, "directPayment") || strings.EqualFold(k, "isDirectPaymentFromUser") || strings.EqualFold(k, "direct_payment") {
					switch b := val.(type) {
					case bool:
						if b {
							return true
						}
					case string:
						if strings.EqualFold(strings.TrimSpace(b), "true") {
							return true
						}
					case nil:
					default:
						return true
					}
				}
				if walk(val) {
					return true
				}
			}
		case []any:
			for _, e := range t {
				if walk(e) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// OpenSession opens a session by blockchain model ID; the router picks a
// provider using its bid rating and handles approval + stake.
func (c *Client) OpenSession(modelID string, req OpenSessionRequest) (string, error) {
	var res struct {
		SessionID string `json:"sessionID"`
	}
	err := c.doJSON(http.MethodPost, "/blockchain/models/"+modelID+"/session", req, &res)
	if err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", fmt.Errorf("open session for model %s: empty sessionID in response", modelID)
	}
	return res.SessionID, nil
}

// OpenSessionByBid opens a session on one specific bid (and so one specific
// provider). Used whenever a provider has to be avoided: avoidance is never
// left to the router's own selection. Same staking-only body.
func (c *Client) OpenSessionByBid(bidID string, durationSec int) (string, error) {
	bidID = strings.TrimSpace(bidID)
	if bidID == "" {
		return "", fmt.Errorf("empty bid id")
	}
	body := struct {
		SessionDuration int  `json:"sessionDuration"`
		DirectPayment   bool `json:"directPayment"`
		Failover        bool `json:"failover"`
	}{durationSec, false, false}
	var res struct {
		SessionID string `json:"sessionID"`
	}
	if err := c.doJSON(http.MethodPost, "/blockchain/bids/"+url.PathEscape(bidID)+"/session", body, &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", fmt.Errorf("open session for bid %s: empty sessionID in response", bidID)
	}
	return res.SessionID, nil
}

// RatedBid is one row of GET /blockchain/models/{id}/bids/rated.
type RatedBid struct {
	ID       string
	Provider string // lowercase hex address
	Score    float64
}

// RatedBids lists the model's active bids with the router's score, best
// first (stable on ties, in router order).
func (c *Client) RatedBids(modelID string) ([]RatedBid, error) {
	var res struct {
		Bids []struct {
			ID  string `json:"ID"`
			Bid struct {
				Id       string `json:"Id"`
				Provider string `json:"Provider"`
			} `json:"Bid"`
			Score float64 `json:"Score"`
		} `json:"bids"`
	}
	if err := c.doJSON(http.MethodGet, "/blockchain/models/"+url.PathEscape(modelID)+"/bids/rated", nil, &res); err != nil {
		return nil, err
	}
	out := make([]RatedBid, 0, len(res.Bids))
	for _, b := range res.Bids {
		// The bid id is Bid.Id: the router leaves the outer ID unset (a
		// zero hash), so it is only a fallback when it is non-zero.
		id := strings.TrimSpace(b.Bid.Id)
		if zeroHex(id) {
			id = strings.TrimSpace(b.ID)
		}
		prov := strings.ToLower(strings.TrimSpace(b.Bid.Provider))
		if zeroHex(id) || zeroHex(prov) {
			continue
		}
		out = append(out, RatedBid{ID: id, Provider: prov, Score: b.Score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// zeroHex reports a missing hex value: empty, "0x" alone, or all zeros
// (the zero hash or zero address).
func zeroHex(s string) bool {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	return strings.Trim(s, "0") == ""
}

func (c *Client) CloseSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("empty session id")
	}
	// Empty JSON body — some proxies reject POST with no body.
	path := "/blockchain/sessions/" + url.PathEscape(sessionID) + "/close"
	return c.doJSON(http.MethodPost, path, map[string]any{}, nil)
}

// Balance returns the router wallet's ETH and MOR balances (wei-scale ints).
func (c *Client) Balance() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.doJSON(http.MethodGet, "/blockchain/balance", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) Healthcheck() (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.doJSON(http.MethodGet, "/healthcheck", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ForwardResult reports what happened so the caller can decide on retry.
type ForwardResult struct {
	StatusCode int
	// Usage tokens from a buffered JSON body, or from the last SSE chunk
	// that carried a usage object.
	PromptTokens     int64
	CompletionTokens int64
	// MidStreamProviderError is set when a provider model error arrived
	// after part of an SSE answer was already sent. The error line was
	// replaced by an SSE error event and the stream ended; the caller
	// retires the session for later requests (no retry of this one).
	MidStreamProviderError bool
	MidStreamStatus        int
	// Cut says why a stream ended early (CutNone for a complete stream or
	// a buffered body). Only CutUpstream is a provider fault.
	Cut Cut
	// TimeoutKind names the Uplink timer ("idle" or "total") behind a
	// CutUplinkTimeout; the stream then ended with sseTimeoutTail.
	TimeoutKind string
}

// Cut is the reason a relayed stream ended before its end marker.
type Cut int

const (
	// CutNone: the stream ended normally ([DONE] or a finish_reason seen).
	CutNone Cut = iota
	// CutUpstream: reading the router body failed, or it ended without
	// [DONE] or a finish_reason, while the request was still live and no
	// Uplink timer fired, or the router sent a mid-stream provider model
	// error. The only provider fault.
	CutUpstream
	// CutClient: writing or flushing to our own client failed (hang-up,
	// broken pipe, connection reset, write deadline).
	CutClient
	// CutCanceled: the request context was cancelled (client gone).
	CutCanceled
	// CutUplinkTimeout: one of Uplink's own timers ended the call.
	CutUplinkTimeout
)

func (c Cut) String() string {
	return [...]string{"none", "upstream", "client", "canceled", "uplink_timeout"}[c]
}

// ErrIdleTimeout and ErrTotalTimeout are the context causes Uplink's own
// per-request timers cancel with; a cut carrying either is ours, never the
// provider's.
var (
	ErrIdleTimeout  = errors.New("uplink idle timeout")
	ErrTotalTimeout = errors.New("uplink total timeout")
)

// IsUplinkTimeout reports whether err (or ctx's cause) is one of Uplink's
// own timers: the two causes above, a context deadline, or the HTTP
// client's own timeout.
func IsUplinkTimeout(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrIdleTimeout) || errors.Is(cause, ErrTotalTimeout) || errors.Is(cause, context.DeadlineExceeded) {
			return true
		}
	}
	if err == nil {
		return false
	}
	if errors.Is(err, ErrIdleTimeout) || errors.Is(err, ErrTotalTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(err.Error(), "Client.Timeout exceeded")
}

// maxErrorBody bounds the router error body kept for classification and
// relayed to the client.
const maxErrorBody = 64 << 10

// Forward proxies an inference call (chat completions, embeddings, audio)
// to the router with the session_id header, streaming the response to w.
//
// If probeOnly is true and the router responds with a non-2xx status, the
// response is NOT written to w and the caller may retry (e.g. with a fresh
// session) — otherwise everything is relayed as-is.
//
// ctx is the request's context: when it ends (client gone, or one of
// Uplink's timers) the upstream request is cancelled too.
func (c *Client) Forward(ctx context.Context, w http.ResponseWriter, path, sessionID string, header http.Header, body []byte, probeOnly bool) (*ForwardResult, error) {
	return c.ForwardWith(ctx, w, path, sessionID, header, body, probeOnly, ForwardOptions{})
}

// ForwardOptions bound one inference call. Zero values switch a timer off.
type ForwardOptions struct {
	// Idle is the first-byte and between-reads timer, reset on every read.
	// For a non-stream call it applies only until the response headers;
	// after that Total governs (a buffered body has no bytes until the end).
	Idle time.Duration
	// Total bounds the whole call.
	Total time.Duration
	// Stream is the request's "stream" field.
	Stream bool
}

// TimeoutError is an Uplink timer that ended a call before anything was
// written to the client (the caller answers 504).
type TimeoutError struct{ Kind string } // "idle" or "total"

func (e *TimeoutError) Error() string { return "upstream " + e.Kind + " timeout" }

// callTimers runs the idle and total timers of one call. Both are
// time.AfterFunc timers, stopped when the call ends, so no goroutine
// outlives the request.
type callTimers struct {
	idle, total *time.Timer
	idleDur     time.Duration
	mu          sync.Mutex
	idleOff     bool
}

func startTimers(cancel context.CancelCauseFunc, o ForwardOptions) *callTimers {
	t := &callTimers{idleDur: o.Idle}
	if o.Total > 0 {
		t.total = time.AfterFunc(o.Total, func() { cancel(ErrTotalTimeout) })
	}
	if o.Idle > 0 {
		t.idle = time.AfterFunc(o.Idle, func() { cancel(ErrIdleTimeout) })
	}
	return t
}

// touch resets the idle timer (a read made progress).
func (t *callTimers) touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idle != nil && !t.idleOff {
		t.idle.Reset(t.idleDur)
	}
}

// idleStop switches the idle timer off (non-stream body after headers).
func (t *callTimers) idleStop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.idleOff = true
	if t.idle != nil {
		t.idle.Stop()
	}
}

func (t *callTimers) stop() {
	t.idleStop()
	if t.total != nil {
		t.total.Stop()
	}
}

// timedReader resets the idle timer on every read that returns bytes.
type timedReader struct {
	r io.Reader
	t *callTimers
}

func (tr timedReader) Read(b []byte) (int, error) {
	n, err := tr.r.Read(b)
	if n > 0 {
		tr.t.touch()
	}
	return n, err
}

// timeoutKind names the Uplink timer that ended ctx ("" when none did).
func timeoutKind(ctx context.Context) string {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrIdleTimeout):
		return "idle"
	case errors.Is(cause, ErrTotalTimeout):
		return "total"
	}
	return ""
}

// sseTimeoutTail ends a stream that an Uplink timer cut: the fixed error
// event, a finish_reason chunk naming the timer, and [DONE].
func sseTimeoutTail(kind string) []byte {
	ev, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "the response was cut by an Uplink " + kind + " timeout",
		"type":    "timeout",
		"code":    http.StatusGatewayTimeout,
	}})
	out := append(append([]byte("data: "), ev...), '\n', '\n')
	out = append(out, []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"uplink_`+kind+`_timeout"}]}`+"\n\n")...)
	return append(out, []byte("data: [DONE]\n\n")...)
}

// ForwardWith is Forward with per-call timers.
func (c *Client) ForwardWith(ctx context.Context, w http.ResponseWriter, path, sessionID string, header http.Header, body []byte, probeOnly bool, o ForwardOptions) (*ForwardResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timers := startTimers(cancel, o)
	defer timers.stop()
	req, err := c.newRequest(http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	if ct := header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("session_id", sessionID)

	hc := c.ForwardHTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if k := timeoutKind(ctx); k != "" {
			return nil, &TimeoutError{Kind: k}
		}
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	timers.touch()
	if !o.Stream && !isEventStream(resp.Header.Get("Content-Type")) {
		timers.idleStop() // buffered body: total governs from here
	}
	src := timedReader{r: resp.Body, t: timers}

	res := &ForwardResult{StatusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusUnauthorized {
		c.noteAuth(resp.StatusCode, "inference rejected")
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.noteAuth(resp.StatusCode, "")
	}
	if probeOnly && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		// Nothing is written to w: the caller classifies the typed error and
		// fails over, retries or relays the status and body itself.
		raw, rerr := io.ReadAll(io.LimitReader(src, maxErrorBody))
		if rerr != nil {
			if k := timeoutKind(ctx); k != "" {
				return nil, &TimeoutError{Kind: k}
			}
		}
		return res, &StatusError{Method: http.MethodPost, Path: path, Code: resp.StatusCode, Body: string(raw)}
	}

	contentType := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", contentType)

	streaming := isEventStream(contentType)
	if streaming {
		w.WriteHeader(resp.StatusCode)
		st := flushCopyCtx(ctx, w, src)
		res.PromptTokens, res.CompletionTokens = st.promptTokens, st.completionTokens
		res.MidStreamProviderError, res.MidStreamStatus = st.providerError, st.providerStatus
		res.Cut = st.cut
		if st.cut == CutUplinkTimeout {
			if res.TimeoutKind = timeoutKind(ctx); res.TimeoutKind != "" && st.clientWriteErr == nil {
				_, _ = w.Write(sseTimeoutTail(res.TimeoutKind))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
		return res, nil
	}

	// Buffered path: capture the body so we can parse token usage.
	raw, err := io.ReadAll(io.LimitReader(src, 32<<20))
	if err != nil {
		if k := timeoutKind(ctx); k != "" {
			return nil, &TimeoutError{Kind: k}
		}
		return nil, fmt.Errorf("read router response: %w", err)
	}
	var usage struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &usage) == nil {
		res.PromptTokens = usage.Usage.PromptTokens
		res.CompletionTokens = usage.Usage.CompletionTokens
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
	return res, nil
}

func isEventStream(contentType string) bool {
	return len(contentType) >= 17 && contentType[:17] == "text/event-stream"
}

// streamResult is what flushCopy saw while relaying an SSE stream.
type streamResult struct {
	promptTokens, completionTokens int64
	providerError                  bool // a provider fault ended the stream (see Cut)
	providerStatus                 int  // status the provider error carried; 0 for a bare upstream cut
	sawDone                        bool // a data: [DONE] line arrived
	sawFinish                      bool // a chunk carried a non-null finish_reason
	cut                            Cut
	upstreamReadErr                error // reading the router body failed (not io.EOF)
	clientWriteErr                 error // writing to our client failed
}

// flushCopy copies an SSE stream to w line by line, flushing after each
// read so tokens reach the client immediately. It watches data: lines for
// the usage object (the proxy-router sends that chunk before [DONE]).
//
// A provider model error that arrives after the first chunk cannot be
// failed over (part of the answer is already with the client). The router
// appends it as a bare JSON object ({"providerModelError":...}), which SSE
// clients would ignore; flushCopy writes an OpenAI-shaped SSE error event
// in its place and ends the stream.
//
// Only the upstream side can be a provider fault: an upstream EOF without
// [DONE] or a finish_reason while ctx is live (no client hang-up, no
// Uplink timer) is a cut; a write error to our client, a cancelled ctx or
// an Uplink timer is never one (see Cut).
func flushCopy(w http.ResponseWriter, src io.Reader) streamResult {
	return flushCopyCtx(context.Background(), w, src)
}

func flushCopyCtx(ctx context.Context, w http.ResponseWriter, src io.Reader) streamResult {
	var out streamResult
	flusher, _ := w.(http.Flusher)
	// ours classifies an end that came from our side of the stream.
	ours := func(err error) Cut {
		if IsUplinkTimeout(ctx, err) {
			return CutUplinkTimeout
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return CutCanceled
		}
		return CutNone
	}
	buf := make([]byte, 32*1024)
	var pending []byte
	passthrough := false // inside an oversized line: relay, do not parse
	write := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		if _, err := w.Write(b); err != nil {
			out.clientWriteErr = err
			if out.cut = ours(nil); out.cut == CutNone {
				out.cut = CutClient
			}
			return false
		}
		return true
	}
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// handle relays one complete line (with its newline). It returns false
	// when the stream must stop (client gone, or provider error replaced).
	handle := func(line []byte) bool {
		if status, ok := sseProviderError(line); ok {
			out.providerError, out.providerStatus, out.cut = true, status, CutUpstream
			_ = write(sseErrorEvent(status))
			return false
		}
		if p, c, ok := sseUsage(line); ok {
			out.promptTokens, out.completionTokens = p, c
		}
		if sseDone(line) {
			out.sawDone = true
		} else if !out.sawFinish && sseFinish(line) {
			out.sawFinish = true
		}
		return write(line)
	}
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for len(chunk) > 0 {
				nl := bytes.IndexByte(chunk, '\n')
				if passthrough {
					end := len(chunk)
					if nl >= 0 {
						end = nl + 1
						passthrough = false
					}
					if !write(chunk[:end]) {
						return out
					}
					chunk = chunk[end:]
					continue
				}
				if nl < 0 {
					pending = append(pending, chunk...)
					chunk = nil
					if len(pending) > maxSSELine {
						// Relay an oversized line unparsed instead of
						// buffering without bound.
						if !write(pending) {
							return out
						}
						pending = pending[:0]
						passthrough = true
					}
					break
				}
				line := append(pending, chunk[:nl+1]...)
				pending = nil
				chunk = chunk[nl+1:]
				if !handle(line) {
					flush()
					return out
				}
			}
			flush()
		}
		if err != nil {
			if len(pending) > 0 && !passthrough {
				if !handle(pending) {
					flush()
					return out
				}
			}
			if err != io.EOF {
				out.upstreamReadErr = err
			}
			if c := ours(err); c != CutNone {
				// Our client or our timer ended it: never a provider fault.
				out.cut = c
			} else if out.upstreamReadErr != nil || (!out.sawDone && !out.sawFinish) {
				// The provider side broke off mid-answer.
				// Tell the client (best effort), then record the cut.
				_ = write(sseErrorEvent(http.StatusBadGateway))
				out.providerError, out.providerStatus, out.cut = true, 0, CutUpstream
			}
			flush()
			return out
		}
	}
}

// sseDone reports a "data: [DONE]" line.
func sseDone(line []byte) bool {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(t[len("data:"):]), []byte("[DONE]"))
}

// sseFinish reports a data: chunk with a non-null finish_reason.
func sseFinish(line []byte) bool {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) || !bytes.Contains(t, []byte(`"finish_reason"`)) {
		return false
	}
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(bytes.TrimSpace(t[len("data:"):]), &chunk) != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return true
		}
	}
	return false
}

// sseProviderError reports a bare provider model error line (not a data:
// event) in a stream, with the status it carries (502 when it has none).
func sseProviderError(line []byte) (int, bool) {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || t[0] != '{' || !bytes.Contains(t, []byte(`"providerModelError"`)) {
		return 0, false
	}
	var body struct {
		StatusCode int `json:"statusCode"`
	}
	// Decode only the first JSON value: the router appends the object with
	// no newline, so anything after it would share the line.
	_ = json.NewDecoder(bytes.NewReader(t)).Decode(&body)
	if body.StatusCode < 400 || body.StatusCode > 599 {
		body.StatusCode = http.StatusBadGateway
	}
	return body.StatusCode, true
}

// sseErrorEvent is the OpenAI-shaped error event that ends a stream whose
// provider failed after the first chunk.
func sseErrorEvent(status int) []byte {
	ev, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "the provider failed during the response; send the request again",
		"type":    "provider_error",
		"code":    status,
	}})
	return append(append([]byte("data: "), ev...), '\n', '\n')
}

const maxSSELine = 1 << 20

// sseUsage parses one SSE line and reports the usage it carries, if any.
func sseUsage(line []byte) (int64, int64, bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return 0, 0, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' || !bytes.Contains(payload, []byte(`"usage"`)) {
		return 0, 0, false
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil || chunk.Usage == nil {
		return 0, 0, false
	}
	return chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
}
