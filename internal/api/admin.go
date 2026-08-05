package api

import (
	"encoding/json"
	"net/http"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		// Both derived keys are re-derivable from the seed, so showing them
		// to the authenticated admin does not widen the secret surface.
		"masterKey": s.masterKey, // full access: prompt + admin
		"promptKey": s.promptKey, // inference only
		"keys":      s.store.ListKeys(),
	})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `body must be {"name": "..."}`, http.StatusBadRequest)
		return
	}
	full, rec, err := keymaker.NewKey(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.AddKey(rec); err != nil {
		http.Error(w, "failed to persist key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":    full, // shown once; only the hash is stored
		"record": rec,
	})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	removed, err := s.store.RevokeKey(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !removed {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"sessions": s.pool.Snapshot(),
	}
	if health, err := s.router.Healthcheck(); err == nil {
		status["router"] = health
	} else {
		status["routerError"] = err.Error()
	}
	if balance, err := s.router.Balance(); err == nil {
		status["balance"] = balance
	} else {
		status["balanceError"] = err.Error()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"usage": s.store.Usage()})
}

func (s *Server) handlePoolCloseAll(w http.ResponseWriter, r *http.Request) {
	s.pool.CloseAll()
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}
