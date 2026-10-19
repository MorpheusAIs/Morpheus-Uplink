// Package router is a thin client for the proxy-router (C-Node) admin API
// on :8082. All requests use HTTP Basic Auth (COOKIE_CONTENT credentials).
package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL string
	// HTTP is long-timeout because chat completions can stream for minutes.
	HTTP *http.Client

	mu   sync.Mutex
	user string
	pass string
	// authFile, when set, is re-read after a router 401 so a credential
	// replaced in place is picked up without a restart.
	authFile string
	// First 401 of the current failure streak (zero when the last
	// authenticated call succeeded).
	authFailSince time.Time
	authFailMsg   string
	authOKAt      time.Time
	now           func() time.Time
}

func New(baseURL, user, pass string) *Client {
	return &Client{
		BaseURL: baseURL,
		user:    user,
		pass:    pass,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		now:     time.Now,
	}
}

// WithAuthFile records the "user:password" file to re-read after a 401.
func (c *Client) WithAuthFile(path string) *Client {
	c.mu.Lock()
	c.authFile = strings.TrimSpace(path)
	c.mu.Unlock()
	return c
}

// User returns the scoped router user name (never the password).
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

// reloadAuth re-reads the auth file. Reports whether the credential changed.
// The file content is never logged.
func (c *Client) reloadAuth() bool {
	c.mu.Lock()
	path := c.authFile
	c.mu.Unlock()
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	if !ok || user == "" || pass == "" || strings.EqualFold(user, "admin") {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if user == c.user && pass == c.pass {
		return false
	}
	c.user, c.pass = user, pass
	return true
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

// AuthFailure reports whether the router is currently rejecting the scoped
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
	KindAuth     = "router_auth"      // router rejected the scoped credential
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
		return "router rejected Uplink's scoped credential (401); Uplink re-reads it and restarts itself if the rejection persists"
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
	if err == nil && status == http.StatusUnauthorized && c.reloadAuth() {
		// Credential file changed under us: retry once with the new one.
		status, raw, err = c.roundTrip(method, path, payload)
	}
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
// "directPayment": false explicitly.
type OpenSessionRequest struct {
	SessionDuration int  `json:"sessionDuration"`
	Failover        bool `json:"failover"`
}

// MarshalJSON pins directPayment to false.
func (r OpenSessionRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SessionDuration int  `json:"sessionDuration"`
		Failover        bool `json:"failover"`
		DirectPayment   bool `json:"directPayment"`
	}{r.SessionDuration, r.Failover, false})
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
}

// Forward proxies an inference call (chat completions, embeddings, audio)
// to the router with the session_id header, streaming the response to w.
//
// If probeOnly is true and the router responds with a non-2xx status, the
// response is NOT written to w and the caller may retry (e.g. with a fresh
// session) — otherwise everything is relayed as-is.
func (c *Client) Forward(w http.ResponseWriter, path, sessionID string, header http.Header, body []byte, probeOnly bool) (*ForwardResult, error) {
	req, err := c.newRequest(http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ct := header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("session_id", sessionID)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	res := &ForwardResult{StatusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusUnauthorized {
		c.noteAuth(resp.StatusCode, "inference rejected")
		c.reloadAuth()
	} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.noteAuth(resp.StatusCode, "")
	}
	if probeOnly && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		// Drain a little for the error message, then let the caller retry.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return res, fmt.Errorf("router status %d: %s", resp.StatusCode, string(raw))
	}

	contentType := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", contentType)

	streaming := isEventStream(contentType)
	if streaming {
		w.WriteHeader(resp.StatusCode)
		res.PromptTokens, res.CompletionTokens = flushCopy(w, resp.Body)
		return res, nil
	}

	// Buffered path: capture the body so we can parse token usage.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
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

// flushCopy copies src to w, flushing after every chunk so SSE tokens reach
// the client immediately. It watches data: lines as they pass and returns
// the token counts from the last chunk that carries a usage object. The
// proxy-router sends that chunk before [DONE] on every stream.
func flushCopy(w http.ResponseWriter, src io.Reader) (promptTokens, completionTokens int64) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var pending []byte
	discardUntilNL := false
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			pending = append(pending, buf[:n]...)
			for {
				nl := bytes.IndexByte(pending, '\n')
				if nl < 0 {
					break
				}
				line := pending[:nl]
				pending = pending[nl+1:]
				if discardUntilNL {
					discardUntilNL = false
					continue
				}
				if p, c, ok := sseUsage(line); ok {
					promptTokens, completionTokens = p, c
				}
			}
			if len(pending) > maxSSELine {
				// Drop an oversized line instead of buffering without bound.
				pending = pending[:0]
				discardUntilNL = true
			}
		}
		if err != nil {
			if !discardUntilNL {
				if p, c, ok := sseUsage(pending); ok {
					promptTokens, completionTokens = p, c
				}
			}
			return
		}
	}
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
