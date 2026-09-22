package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
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
			writeOpenAIError(w, http.StatusBadGateway, "failed to open session: "+err.Error())
			return
		}
		if ensured.Opened {
			s.store.TrackSessionOpen(ensured.SessionID, keyID, reqModel)
		}
		sessionID := ensured.SessionID

		// First attempt probes. Only rotate the session when the router says
		// the session itself is dead — provider/prompt errors must not burn
		// another stake while a usable session is still open on-chain.
		res, err := s.router.Forward(w, routerPath, sessionID, r.Header, body, true)
		if err != nil {
			log.Printf("api: forward attempt 1 (model %s, session %s): %v", reqModel, sessionID, err)
			if !isSessionDeadError(err) {
				writeOpenAIError(w, http.StatusBadGateway, "inference failed: "+err.Error())
				return
			}
			s.pool.Invalidate(modelID)
			ensured, err = s.pool.EnsureDuration(modelID, durationSec)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "failed to reopen session: "+err.Error())
				return
			}
			if ensured.Opened {
				s.store.TrackSessionOpen(ensured.SessionID, keyID, reqModel)
			}
			sessionID = ensured.SessionID
			res, err = s.router.Forward(w, routerPath, sessionID, r.Header, body, false)
			if err != nil {
				log.Printf("api: forward attempt 2 (model %s, session %s): %v", reqModel, sessionID, err)
				writeOpenAIError(w, http.StatusBadGateway, "inference failed: "+err.Error())
				return
			}
		}
		s.store.RecordUsage(keyID, reqModel, res.PromptTokens, res.CompletionTokens)
	}
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
		writeOpenAIError(w, http.StatusBadGateway, "model catalog unavailable: "+err.Error())
		return
	}
	defaultDur := s.cfg.SessionDurationSec
	if defaultDur <= 0 {
		defaultDur = 600
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		bid, _ := m.LowestBid()
		data = append(data, map[string]any{
			"id":       m.Name,
			"object":   "model",
			"owned_by": "morpheus",
			"morpheus": map[string]any{
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
// WARN: Master Bearer is an intentional full-instance usage reader (ops:
// Seraph can poll without admin Basic). Prompt and generated keys never see
// other keys' rows — filter is keyId == caller. keyId is a public identifier
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
	writeJSON(w, http.StatusOK, map[string]any{"usage": rows})
}
