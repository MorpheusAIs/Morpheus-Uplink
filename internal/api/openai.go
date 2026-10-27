package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
		keyID := r.Header.Get("X-Uplink-Key-Id")
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "failed to read request body")
			return
		}
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

		durationSec := 0
		if v := r.Header.Get("X-Uplink-Session-Duration"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				if n < 600 {
					writeOpenAIError(w, http.StatusBadRequest, "X-Uplink-Session-Duration must be >= 600 seconds (protocol floor)")
					return
				}
				durationSec = n
			}
		}
		ensured, err := s.pool.EnsureDuration(modelID, durationSec)
		if err != nil {
			log.Printf("api: open session for %s (%s): %v", reqModel, modelID, err)
			writeOpenAIError(w, http.StatusBadGateway, "failed to open session") // detail logged above; it names router internals
			return
		}
		if ensured.Opened {
			s.store.TrackSessionOpen(ensured.SessionID, keyID, modelID)
		}
		sessionID := ensured.SessionID

		// First attempt probes: a non-2xx is not written to the client, so
		// it can be classified (see classifyForwardError) and either fail
		// over to another provider, reopen a dead session, or be relayed
		// with the router's real status.
		res, err := s.router.Forward(w, routerPath, sessionID, r.Header, body, true)
		if err != nil {
			log.Printf("api: forward attempt 1 (model %s, session %s): %v", reqModel, pool.ShortID(sessionID), err)
			switch dec := classifyForwardError(err); {
			case dec == decTransport:
				writeOpenAIError(w, http.StatusBadGateway, "inference failed") // detail logged above
				return
			case dec == decSessionDead:
				s.pool.Invalidate(modelID)
				ensured, err = s.pool.EnsureDuration(modelID, durationSec)
				if err != nil {
					log.Printf("api: reopen session for %s (%s): %v", reqModel, modelID, err)
					writeOpenAIError(w, http.StatusBadGateway, "failed to reopen session")
					return
				}
				if ensured.Opened {
					s.store.TrackSessionOpen(ensured.SessionID, keyID, modelID)
				}
				sessionID = ensured.SessionID
				res, err = s.router.Forward(w, routerPath, sessionID, r.Header, body, false)
				if err != nil {
					log.Printf("api: forward attempt 2 (model %s, session %s): %v", reqModel, pool.ShortID(sessionID), err)
					writeOpenAIError(w, http.StatusBadGateway, "inference failed") // detail logged above
					return
				}
			case dec == decFailover && s.cfg.SessionFailover:
				var retryID string
				res, retryID, err = s.failoverAndRetry(w, r, routerPath, body, keyID, reqModel, modelID, sessionID, durationSec, err)
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
		if res.MidStreamProviderError {
			s.afterStreamProviderError(reqModel, modelID, sessionID, durationSec, res.MidStreamStatus)
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
	keyID, reqModel, modelID, failedID string, durationSec int, firstErr error) (*router.ForwardResult, string, error) {
	class := statusClass(firstErr)
	fr, ferr := s.pool.Failover(modelID, failedID, durationSec, true)
	if fr.CloseFailed {
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "close_failed")
	}
	if ferr != nil {
		outcome := "reopen_failed"
		if errors.Is(ferr, pool.ErrNoAlternate) {
			outcome = "no_alternate"
		}
		log.Printf("api: failover (model %s, session %s): %v", reqModel, pool.ShortID(failedID), ferr)
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, outcome)
		relayRouterError(w, firstErr) // the original status and body
		return nil, "", errAnswered
	}
	if fr.Opened {
		s.store.TrackSessionOpen(fr.SessionID, keyID, modelID)
	}
	res, err := s.router.Forward(w, routerPath, fr.SessionID, r.Header, body, true)
	if fr.Replaced {
		// Another request did the failover (and its audit); this one only
		// retried on the replacement.
		if err != nil {
			if classifyForwardError(err) == decFailover {
				s.pool.MarkSessionCooling(modelID, fr.SessionID)
			}
			relayRouterError(w, err)
			return nil, "", errAnswered
		}
		return res, fr.SessionID, nil
	}
	if err != nil {
		log.Printf("api: forward retry after failover (model %s, session %s): %v", reqModel, pool.ShortID(fr.SessionID), err)
		if classifyForwardError(err) == decFailover {
			s.pool.MarkSessionCooling(modelID, fr.SessionID)
		}
		s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "retry_failed")
		relayRouterError(w, err)
		return nil, "", errAnswered
	}
	s.auditFailover(reqModel, failedID, fr.FailedProvider, class, "retried_ok")
	return res, fr.SessionID, nil
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
	if status >= 500 {
		class = "5xx"
	}
	go func() {
		fr, err := s.pool.Failover(modelID, sessionID, durationSec, false)
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
		log.Printf("api: model catalog: %v", err)
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
	defaultDur := s.cfg.SessionDurationSec
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
		log.Printf("api: /v1/usage missing X-Uplink-Key-Id after auth")
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
