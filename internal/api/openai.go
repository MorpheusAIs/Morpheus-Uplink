package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// maxRequestBody caps inference request bodies (large contexts are fine,
// abuse is not).
const maxRequestBody = 16 << 20

// injectedRoutingFields are client-supplied keys that must never influence
// session open / routing. Uplink owns session_id via the pool; these would
// only confuse or attempt to hijack the path to the router.
var injectedRoutingFields = []string{
	"session_id",
	"model_id",
	"chat_id",
	"provider_url",
	"node_url",
}

// validateInferenceBody checks the JSON body before any session open.
// requireMessages is true for /v1/chat/completions (non-empty messages[]).
// Unknown provider extras (venice_parameters, thinking, tools, …) are left
// in the body and forwarded unchanged.
func validateInferenceBody(body []byte, requireMessages bool) (model string, errMsg string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return "", "request must be a JSON object"
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", "request must be a JSON object"
	}
	for _, f := range injectedRoutingFields {
		if _, ok := raw[f]; ok {
			return "", fmt.Sprintf("injected routing field %q is not allowed", f)
		}
	}
	if m, ok := raw["model"]; ok {
		_ = json.Unmarshal(m, &model)
	}
	if model == "" {
		return "", "request must be JSON with a \"model\" field"
	}
	if requireMessages {
		msgRaw, ok := raw["messages"]
		if !ok {
			return "", "request must include a non-empty \"messages\" array"
		}
		var messages []json.RawMessage
		if err := json.Unmarshal(msgRaw, &messages); err != nil || len(messages) == 0 {
			return "", "request must include a non-empty \"messages\" array"
		}
	}
	return model, ""
}

// handleInference proxies an OpenAI-style POST (chat completions or
// embeddings) through the session pool to the router.
func (s *Server) handleInference(routerPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID := keyIDFrom(r)

		body, err := readBodyWithDeadline(w, r, maxRequestBody)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "failed to read request body")
			return
		}
		dw := newDeadlineWriter(w)
		defer dw.clear()
		w = dw

		requireMessages := routerPath == "/v1/chat/completions"
		reqModel, errMsg := validateInferenceBody(body, requireMessages)
		if errMsg != "" {
			writeOpenAIError(w, http.StatusBadRequest, errMsg)
			return
		}

		modelID, err := s.catalog.Resolve(reqModel)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, err.Error())
			return
		}

		cl, msg := s.parseCallLimits(r)
		if msg != "" {
			writeOpenAIError(w, http.StatusBadRequest, msg)
			return
		}
		durationSec := cl.durationSec
		stream := bodyStream(body)

		lease, ensured, ok := s.acquireSession(r.Context(), w, keyID, reqModel, modelID, cl)
		if !ok {
			return
		}
		// The lease ends however the handler exits (panic, cancel, retry).
		defer func() { lease.Done() }()
		if h := testHookAfterLease.Load(); h != nil {
			(*h)()
		}
		sessionID := ensured.SessionID
		setTimeoutHeaders(w, cl)
		fo := forwardOpts(cl, stream)

		// First attempt probes: a non-2xx is not written to the client, so
		// it can be classified (see classifyForwardError) and either fail
		// over to another provider, reopen a dead session, or be relayed
		// with the router's real status.
		res, err := s.router.ForwardWith(r.Context(), w, routerPath, sessionID, r.Header, body, true, fo)
		if err != nil {
			warnf("api: forward attempt 1 (model %s, session %s): %v", reqModel, pool.ShortID(sessionID), err)
			var te *router.TimeoutError
			switch dec := classifyForwardError(err); {
			case errors.As(err, &te):
				writeTimeout504(w, te.Kind)
				return
			case dec == decTransport:
				writeOpenAIError(w, http.StatusBadGateway, "inference failed") // detail logged above
				return
			case dec == decSessionDead:
				lease.Done()
				s.pool.Invalidate(modelID)
				var ok bool
				lease, ensured, ok = s.acquireSession(r.Context(), w, keyID, reqModel, modelID, cl)
				if !ok {
					return
				}
				sessionID = ensured.SessionID
				res, err = s.router.ForwardWith(r.Context(), w, routerPath, sessionID, r.Header, body, false, fo)
				if err != nil {
					warnf("api: forward attempt 2 (model %s, session %s): %v", reqModel, pool.ShortID(sessionID), err)
					if errors.As(err, &te) {
						writeTimeout504(w, te.Kind)
						return
					}
					writeOpenAIError(w, http.StatusBadGateway, "inference failed") // detail logged above
					return
				}
			case dec == decFailover && s.cfg.SessionFailover:
				// Our own lease on the failed session ends first, so the
				// failover sees only other requests' in-flight [C9].
				lease.Done()
				var retryID string
				res, retryID, lease, err = s.failoverAndRetry(w, r, routerPath, body, keyID, reqModel, modelID, sessionID, durationSec, fo, err)
				if err != nil {
					return // already answered
				}
				// The answer came from the replacement session: a
				// mid-stream failure below must act on that one.
				sessionID = retryID
			default:
				relayRouterError(w, err)
				return
			}
		}
		switch {
		case res.MidStreamProviderError:
			// Only an upstream cut is a provider fault: cool-down + retire.
			s.afterStreamProviderError(reqModel, modelID, sessionID, durationSec, res.MidStreamStatus)
		case res.Cut != router.CutNone:
			// Our client or our own timer ended it: logged, never a
			// provider fault (no cool-down, retire or failover).
			warnf("api: stream ended early (model %s, session %s): %s", reqModel, pool.ShortID(sessionID), res.Cut)
		}
		s.store.RecordUsage(keyID, modelID, res.PromptTokens, res.CompletionTokens) // by resolved id (#56)
	}
}

// errAnswered marks a request whose error reply was already written.
var errAnswered = errors.New("answered")

// failoverAndRetry moves the model to another provider and sends the
// request once more. Exactly one retry: a provider failure on it cools
// that provider too and is relayed, with no second failover. On success it
// returns the session the retry ran on.
func (s *Server) failoverAndRetry(w http.ResponseWriter, r *http.Request, routerPath string, body []byte,
	keyID, reqModel, modelID, failedID string, durationSec int, fo router.ForwardOptions, firstErr error) (*router.ForwardResult, string, *pool.Lease, error) {
	class := statusClass(firstErr)
	fr, ferr := s.pool.Failover(modelID, failedID, pool.FailoverOpts{KeyID: keyID, DurationSec: durationSec, Reopen: true})
	if fr.CloseFailed {
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "close_failed")
	}
	if ferr != nil {
		outcome := "reopen_failed"
		switch {
		case errors.Is(ferr, pool.ErrNoAlternate):
			outcome = "no_alternate"
		case errors.Is(ferr, pool.ErrNoAvailableProvider):
			outcome = "no_available" // every eligible provider is avoided; nothing opened
		}
		warnf("api: failover (model %s, session %s): %v", reqModel, pool.ShortID(failedID), ferr)
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, outcome)
		relayRouterError(w, firstErr) // the original status and body
		return nil, "", nil, errAnswered
	}
	if fr.Opened {
		s.store.TrackSessionOpen(fr.SessionID, keyID, modelID)
	}
	lease := fr.Lease // taken with the replacement, in one critical section
	if lease == nil {
		// The replacement is already closing (released or swept).
		relayRouterError(w, firstErr)
		return nil, "", nil, errAnswered
	}
	res, err := s.router.ForwardWith(r.Context(), w, routerPath, fr.SessionID, r.Header, body, true, fo)
	if err != nil {
		var te *router.TimeoutError
		if errors.As(err, &te) {
			lease.Done()
			writeTimeout504(w, te.Kind)
			return nil, "", nil, errAnswered
		}
	}
	if fr.Replaced {
		// Another request did the failover (and its audit); this one only
		// retried on the replacement.
		if err != nil {
			if classifyForwardError(err) == decFailover {
				s.pool.MarkSessionCooling(modelID, fr.SessionID)
			}
			lease.Done()
			relayRouterError(w, err)
			return nil, "", nil, errAnswered
		}
		return res, fr.SessionID, lease, nil
	}
	if err != nil {
		warnf("api: forward retry after failover (model %s, session %s): %v", reqModel, pool.ShortID(fr.SessionID), err)
		if classifyForwardError(err) == decFailover {
			s.pool.MarkSessionCooling(modelID, fr.SessionID)
		}
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "retry_failed")
		lease.Done()
		relayRouterError(w, err)
		return nil, "", nil, errAnswered
	}
	s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "retried_ok")
	return res, fr.SessionID, lease, nil
}

// afterStreamProviderError handles a provider failure after part of an
// answer was streamed: the client already got an SSE error event; here the
// provider starts its cool-down and the session is retired (only when
// another provider exists) so the next request opens elsewhere. In the
// background: the response is finished.
func (s *Server) afterStreamProviderError(reqModel, modelID, sessionID string, durationSec, status int) {
	if !s.cfg.SessionFailover {
		return
	}
	class := fmt.Sprint(status)
	switch {
	case status == 0:
		class = "stream_cut" // upstream ended without [DONE] or finish_reason
	case status >= 500:
		class = "5xx"
	}
	go func() {
		fr, err := s.pool.Failover(modelID, sessionID, pool.FailoverOpts{DurationSec: durationSec})
		if fr.Replaced {
			return
		}
		if fr.CloseFailed {
			s.auditFailover(reqModel, sessionID, fr.FailedProvider, class, "close_failed")
		}
		outcome := "stream_retired"
		if err != nil {
			outcome = "no_alternate"
			if !errors.Is(err, pool.ErrNoAlternate) {
				outcome = "reopen_failed"
			}
		}
		s.auditFailover(reqModel, sessionID, fr.FailedProvider, class, outcome)
	}()
}

// isSessionDeadError is true when the router rejected the session id itself
// (expired / missing). Other failures (provider, payload, capacity) keep the
// pooled session so we do not try to stake again mid-window.
func isSessionDeadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	needles := []string{
		"session expired",
		"session not found",
		"missing session",
		"no session",
		"unknown session",
		"invalid session",
		"session closed",
		"session is closed",
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// handleModels lists catalog models in OpenAI list format, with Morpheus
// metadata tucked into an extension field.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.catalog.List()
	if err != nil {
		warnf("api: model catalog: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, "model catalog unavailable")
		return
	}
	s.writeModelList(w, models)
}

// handleAdminModels is the same list for the admin GUI (session cookie). The
// GUI only holds masked API keys, so it must not call /v1/models itself.
// The catalog comes from ACTIVE_MODELS_URL, not the router.
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.catalog.List()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "model catalog fetch failed: " + err.Error(),
			"kind":  "catalog_fetch",
		})
		return
	}
	s.writeModelList(w, models)
}

func (s *Server) writeModelList(w http.ResponseWriter, models []catalog.Model) {
	defaultDur := s.durationDefault()
	if defaultDur <= 0 {
		defaultDur = 600
	}
	nameCount := map[string]int{}
	for _, m := range models {
		nameCount[strings.ToLower(m.Name)]++
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		bid, _ := m.LowestBid()
		data = append(data, map[string]any{
			"id":       m.SendableID(nameCount[strings.ToLower(m.Name)] > 1),
			"object":   "model",
			"owned_by": "morpheus",
			"morpheus": map[string]any{
				"name":               m.Name,
				"blockchainId":       m.ID,
				"tags":               m.Tags,
				"modelType":          m.ModelType,
				"priceMorPerHour":    m.LowestMorPerHour(),
				"pricePerSecondWei":  bid.PricePerSecond,
				"sessionDurationSec": defaultDur,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// handleUsageV1 returns usage rows for the calling API key.
//
// WARN: Master Bearer is an intentional full-instance usage reader (ops may
// poll without admin session). Prompt and generated keys never see other
// keys' rows — filter is keyId == caller. keyId is a public identifier
// (master / prompt / generated id), never an sk- secret.
func (s *Server) handleUsageV1(w http.ResponseWriter, r *http.Request) {
	caller := r.Header.Get("X-Uplink-Key-Id")
	if caller == "" {
		// Fail closed: never fall back to unfiltered store.Usage().
		warnf("api: /v1/usage missing X-Uplink-Key-Id after auth")
		writeOpenAIError(w, http.StatusUnauthorized, "missing key identity")
		return
	}
	rows := s.store.Usage()
	if caller != keymaker.MasterKeyID {
		filtered := make([]store.UsageRow, 0, len(rows))
		for _, row := range rows {
			if row.KeyID == caller {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": s.usageViews(rows)})
}
