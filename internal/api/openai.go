package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
)

// maxRequestBody caps inference request bodies (large contexts are fine,
// abuse is not).
const maxRequestBody = 16 << 20

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
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
			writeOpenAIError(w, http.StatusBadRequest, "request must be JSON with a \"model\" field")
			return
		}

		modelID, err := s.catalog.Resolve(req.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, err.Error())
			return
		}

		durationSec := 0
		if v := r.Header.Get("X-Uplink-Session-Duration"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				durationSec = n
			}
		}
		ensured, err := s.pool.EnsureDuration(modelID, durationSec)
		if err != nil {
			log.Printf("api: open session for %s (%s): %v", req.Model, modelID, err)
			writeOpenAIError(w, http.StatusBadGateway, "failed to open session: "+err.Error())
			return
		}
		if ensured.Opened {
			s.store.RecordSessionOpen(keyID, req.Model, int64(ensured.DurationSec))
		}
		sessionID := ensured.SessionID

		// First attempt probes: on a router error we retry once with a
		// fresh session (covers expired/broken sessions) before giving up.
		res, err := s.router.Forward(w, routerPath, sessionID, r.Header, body, true)
		if err != nil {
			log.Printf("api: forward attempt 1 (model %s, session %s): %v", req.Model, sessionID, err)
			s.pool.Invalidate(modelID)
			ensured, err = s.pool.EnsureDuration(modelID, durationSec)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "failed to reopen session: "+err.Error())
				return
			}
			if ensured.Opened {
				s.store.RecordSessionOpen(keyID, req.Model, int64(ensured.DurationSec))
			}
			sessionID = ensured.SessionID
			res, err = s.router.Forward(w, routerPath, sessionID, r.Header, body, false)
			if err != nil {
				log.Printf("api: forward attempt 2 (model %s, session %s): %v", req.Model, sessionID, err)
				writeOpenAIError(w, http.StatusBadGateway, "inference failed: "+err.Error())
				return
			}
		}
		s.store.RecordUsage(keyID, req.Model, res.PromptTokens, res.CompletionTokens)
	}
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
		rate := m.LowestMorPerHour()
		data = append(data, map[string]any{
			"id":       m.Name,
			"object":   "model",
			"owned_by": "morpheus",
			"morpheus": map[string]any{
				"blockchainId":       m.ID,
				"tags":               m.Tags,
				"modelType":          m.ModelType,
				"priceMorPerHour":    rate,
				"stakeMorForDefault": m.StakeMORForDuration(defaultDur),
				"sessionDurationSec": defaultDur,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}
