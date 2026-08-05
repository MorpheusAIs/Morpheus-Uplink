// Package router is a thin client for the proxy-router (C-Node) admin API
// on :8082. All requests use HTTP Basic Auth (COOKIE_CONTENT credentials).
package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	User    string
	Pass    string
	// HTTP is long-timeout because chat completions can stream for minutes.
	HTTP *http.Client
}

func New(baseURL, user, pass string) *Client {
	return &Client{
		BaseURL: baseURL,
		User:    user,
		Pass:    pass,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.User, c.Pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) doJSON(method, path string, reqBody any, out any) error {
	var body io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: parse response: %w", method, path, err)
		}
	}
	return nil
}

type OpenSessionRequest struct {
	SessionDuration int  `json:"sessionDuration"`
	Failover        bool `json:"failover"`
	DirectPayment   bool `json:"directPayment,omitempty"`
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
	return c.doJSON(http.MethodPost, "/blockchain/sessions/"+sessionID+"/close", nil, nil)
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
	// Usage tokens parsed from a buffered (non-streaming) JSON response.
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
		flushCopy(w, resp.Body)
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
// the client immediately.
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
