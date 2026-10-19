// Package api wires the UPLINK HTTP surface:
//
//	/v1/*      OpenAI-compatible, Bearer sk-… auth (master or generated keys)
//	/admin/*   management API, Basic auth (admin / ADMIN_PASSWORD)
//	/gui       embedded single-page management UI
//
// The former /node/* passthrough to the proxy-router admin API is removed:
// scoped router credentials must not be amplified through Uplink admin.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/gui"
	"github.com/absgrafx/morpheus-uplink/internal/housekeep"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

type Server struct {
	cfg       *config.Config
	store     *store.Store
	catalog   *catalog.Catalog
	pool      *pool.Pool
	router    *router.Client
	housekeep *housekeep.Runner
	masterKey string
	promptKey string
}

func NewServer(cfg *config.Config, st *store.Store, cat *catalog.Catalog, pl *pool.Pool, rc *router.Client, hk *housekeep.Runner) *Server {
	return &Server{
		cfg:       cfg,
		store:     st,
		catalog:   cat,
		pool:      pl,
		router:    rc,
		housekeep: hk,
		masterKey: keymaker.MasterKey(cfg.APIKeySeed),
		promptKey: keymaker.PromptKey(cfg.APIKeySeed),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI-compatible surface.
	mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleInference("/v1/chat/completions")))
	mux.HandleFunc("POST /v1/embeddings", s.requireAPIKey(s.handleInference("/v1/embeddings")))
	mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	mux.HandleFunc("GET /v1/usage", s.requireAPIKey(s.handleUsageV1))

	// Admin surface.
	mux.HandleFunc("GET /admin/keys", s.requireAdmin(s.handleListKeys))
	mux.HandleFunc("POST /admin/keys", s.requireAdmin(s.handleCreateKey))
	mux.HandleFunc("POST /admin/keys/import", s.requireAdmin(s.handleImportKey))
	mux.HandleFunc("DELETE /admin/keys/{id}", s.requireAdmin(s.handleRevokeKey))
	mux.HandleFunc("GET /admin/status", s.requireAdmin(s.handleStatus))
	mux.HandleFunc("GET /admin/usage", s.requireAdmin(s.handleUsage))
	mux.HandleFunc("POST /admin/pool/close-all", s.requireAdmin(s.handlePoolCloseAll))
	mux.HandleFunc("POST /admin/pool/close/{id}", s.requireAdmin(s.handlePoolCloseOne))
	mux.HandleFunc("POST /admin/housekeep", s.requireAdmin(s.handleHousekeep))
	mux.HandleFunc("GET /admin/estimate-stake", s.requireAdmin(s.handleEstimateStake))
	mux.HandleFunc("GET /admin/estimate-stakes", s.requireAdmin(s.handleEstimateStakes))
	mux.HandleFunc("POST /admin/disk/prune-usage", s.requireAdmin(s.handlePruneUsage))

	// GUI + health.
	mux.Handle("/gui/", http.StripPrefix("/gui", gui.Handler()))
	mux.HandleFunc("GET /gui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /healthcheck", s.handleHealth)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gui/", http.StatusFound)
	})

	return mux
}

// requireAPIKey authenticates Bearer sk-… keys for the inference surface.
// Every valid key — master, prompt, or generated — may prompt; the scope
// difference only matters on the admin surface.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || token == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		keyID := ""
		switch {
		case keymaker.Equal(token, s.masterKey):
			keyID = keymaker.MasterKeyID
		case keymaker.Equal(token, s.promptKey):
			keyID = keymaker.PromptKeyID
		default:
			keyID = s.store.LookupKey(token)
		}
		if keyID == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		// Overwrite is intentional: any client-supplied X-Uplink-Key-Id is ignored
		// so callers cannot forge another key's identity for scoped handlers.
		r.Header.Set("X-Uplink-Key-Id", keyID)
		next(w, r)
	}
}

// checkAdmin grants the admin surface to either the Basic-auth admin
// password (humans, GUI) or the Bearer master key (automation) — never the
// prompt key or generated keys.
func (s *Server) checkAdmin(r *http.Request) bool {
	if user, pass, ok := r.BasicAuth(); ok {
		return user == "admin" &&
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) == 1
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return keymaker.Equal(token, s.masterKey)
	}
	return false
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAdmin(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="uplink-admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Version is set from main at startup (build-time ldflags).
var Version = "dev"

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	routerOK := true
	if _, err := s.router.Healthcheck(); err != nil {
		routerOK = false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "healthy",
		"version": Version,
		"router":  routerOK,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOpenAIError mimics the OpenAI error envelope so SDKs surface a
// readable message.
func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}
