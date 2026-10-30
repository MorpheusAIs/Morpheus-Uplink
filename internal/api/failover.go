package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// forwardDecision is what the handler does with a failed first attempt.
type forwardDecision int

const (
	// decTransport: Uplink could not reach its own router. Another provider
	// cannot help; answer 502.
	decTransport forwardDecision = iota
	// decSessionDead: the router rejected the session id; reopen.
	decSessionDead
	// decRelay: not a provider fault (a client error, TEE check, queue
	// cancel); relay the router's real status and body, no cool-down.
	decRelay
	// decFailover: the provider failed; move to another provider.
	decFailover
)

func (d forwardDecision) String() string {
	return [...]string{"transport", "session_dead", "relay", "failover"}[d]
}

// Patterns that mean "this request was fine, the provider is impaired"
// when a provider model error arrives with a 4xx the router defaulted to
// (the router reports 400 when the provider gave no status).
var impairedPatterns = []string{
	"rate limit", "rate_limit", "too many requests",
	"capacity", "overloaded",
	"model unavailable", "model is unavailable", "service unavailable",
	`"statuscode":429`, `"statuscode":503`,
}

// Errors that are not the provider backend's fault and that another
// provider would not fix for this request.
var noFailoverPatterns = []string{
	"tee attestation failed",
	"tee verification failed",
	"request cancelled while waiting in queue",
}

// streamCutPatterns are upstream stream cuts, matched only inside a
// providerModelError body the router returned.
var streamCutPatterns = []string{
	"error reading stream",
	"unexpected eof",
	"connection reset",
	"broken pipe",
}

// classifyForwardError decides what to do with a failed probe Forward.
// The order matters (see docs/06-developers.md, Provider failover).
func classifyForwardError(err error) forwardDecision {
	var se *router.StatusError
	if !errors.As(err, &se) {
		return decTransport
	}
	if isSessionDeadError(err) {
		return decSessionDead
	}
	body := strings.ToLower(se.Body)
	compact := strings.Join(strings.Fields(body), "")
	for _, p := range noFailoverPatterns {
		if strings.Contains(body, p) {
			return decRelay
		}
	}
	pme := strings.Contains(body, "providermodelerror")
	if pme {
		// The provider's stream to the router broke before the first byte
		// (text the upstream reported, never our own socket errors) [F1].
		for _, p := range streamCutPatterns {
			if strings.Contains(body, p) {
				return decFailover
			}
		}
	}
	switch c := se.Code; {
	case pme && (c == 402 || c == 401 || c == 403 || c == 404):
		return decFailover
	case c == http.StatusTooManyRequests || c >= 500:
		return decFailover
	case pme && (c == 400 || c == 413 || c == 422):
		for _, p := range impairedPatterns {
			if strings.Contains(body, p) || strings.Contains(compact, p) {
				return decFailover
			}
		}
	}
	return decRelay
}

// relayRouterError answers with the router's real status. A provider model
// error body is reshaped into the OpenAI error shape (clients read
// error.message) with the provider's own body kept under
// "providerModelError"; a body that already has an "error" field is sent
// as is. A 401/403 without a provider model error is the router rejecting
// Uplink's own credential, not the client: that stays a 502.
func relayRouterError(w http.ResponseWriter, err error) {
	var se *router.StatusError
	if !errors.As(err, &se) {
		// Transport detail (router host, URL) stays in the log.
		warnf("api: inference failed: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, "inference failed")
		return
	}
	// The router text goes to the ops log with URLs and 32-byte ids masked
	// (warnf); the client gets it scrubbed (scrubRouterText) and capped.
	warnf("api: router error %d on %s %s: %s", se.Code, se.Method, se.Path, se.Body)
	code := se.Code
	if code < 400 || code > 599 {
		code = http.StatusBadGateway
	}
	var obj map[string]json.RawMessage
	isObj := json.Unmarshal([]byte(se.Body), &obj) == nil && obj != nil
	_, hasPME := obj["providerModelError"]
	if !hasPME && (code == http.StatusUnauthorized || code == http.StatusForbidden) {
		writeOpenAIError(w, http.StatusBadGateway, "inference failed: router rejected the request ("+fmt.Sprint(se.Code)+")")
		return
	}
	if isObj && !hasPME {
		if raw := obj["error"]; len(raw) > 0 && raw[0] == '{' {
			// Already OpenAI-shaped: relayed with every string scrubbed,
			// when it fits the cap (else reshaped below).
			if body, ok := scrubbedJSON([]byte(se.Body)); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write(body)
				return
			}
		}
	}
	typ := "invalid_request_error"
	if hasPME || code >= 500 || code == http.StatusTooManyRequests {
		typ = "provider_error"
	}
	msg := routerErrorMessage(obj, se.Body)
	var pme any
	if hasPME {
		if p, ok := scrubbedJSON(obj["providerModelError"]); ok {
			pme = p
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(relayBody(msg, typ, code, hasPME, pme))
}

// relayBody is the final reshaped error body, capped as a whole at
// maxRelayBody: when the body with the scrubbed providerModelError is too
// big, the minimal {error:{message}} form replaces it, and the message is
// shortened until that fits too (JSON escaping can grow it).
func relayBody(msg, typ string, code int, hasPME bool, pme any) []byte {
	build := func(msg string, pme any) []byte {
		out := map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
		if hasPME {
			if pme == nil {
				pme = map[string]any{"error": map[string]any{"message": msg}}
			}
			out["providerModelError"] = pme
		}
		var b bytes.Buffer
		_ = json.NewEncoder(&b).Encode(out)
		return b.Bytes()
	}
	body := build(msg, pme)
	if len(body) <= maxRelayBody {
		return body
	}
	for {
		body = build(msg, nil)
		if len(body) <= maxRelayBody || msg == "" {
			return body
		}
		msg = truncate(strings.TrimSuffix(msg, "…"), len(strings.TrimSuffix(msg, "…"))/2)
		if msg == "…" {
			msg = ""
		}
	}
}

// routerErrorMessage pulls a readable message out of a router error body.
func routerErrorMessage(obj map[string]json.RawMessage, body string) string {
	for _, k := range []string{"providerModelError", "error", "message"} {
		if s := nestedMessage(obj[k], 3); s != "" {
			return truncate(scrubRouterText(s), 1000)
		}
	}
	if s := strings.TrimSpace(body); s != "" {
		return truncate(scrubRouterText(s), 1000)
	}
	return "the router returned an error"
}

// nestedMessage is raw itself when it is a string, else the first message,
// error or detail inside it (a few levels deep).
func nestedMessage(raw json.RawMessage, depth int) string {
	if len(raw) == 0 || depth == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"message", "error", "detail"} {
		if s := nestedMessage(m[k], depth-1); s != "" {
			return s
		}
	}
	return ""
}

// statusClass is the audit form of a status: the code itself for 4xx
// (402 vs 429 matters), "5xx" otherwise.
func statusClass(err error) string {
	var se *router.StatusError
	if errors.As(err, &se) {
		if se.Code >= 500 {
			return "5xx"
		}
		return fmt.Sprint(se.Code)
	}
	return "stream"
}

// auditFailover records one provider_failover event: model, short session
// id, failed provider, status class and outcome. Never bodies or secrets.
func (s *Server) auditFailover(model, sessionID, provider, class, outcome string) {
	sid := pool.ShortID(sessionID)
	prov := "unknown"
	if provider != "" {
		prov = pool.ShortID(provider)
	}
	slog.Warn("audit", "event", "provider_failover", "result", outcome, "model", model,
		"session", sid, "provider", prov, "status", class)
	s.auditSystem("provider_failover", "system", outcome,
		fmt.Sprintf("provider_failover %s model=%s session=%s provider=%s status=%s", outcome, model, sid, prov, class), true)
}
