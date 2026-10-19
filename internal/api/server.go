// Package api wires the UPLINK HTTP surface:
//
//	/v1/*      OpenAI-compatible, Bearer sk-… auth (master or generated keys)
//	/admin/*   management API, HttpOnly session cookie (no Basic)
//	/gui       embedded single-page management UI
//
// The former /node/* passthrough to the proxy-router admin API is removed:
// scoped router credentials must not be amplified through Uplink admin.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
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
	sessions  *adminauth.SessionStore
	lockout   *adminauth.Lockout
	audit     *AuditLog
	outflow   *OutflowAlert
}

func NewServer(cfg *config.Config, st *store.Store, cat *catalog.Catalog, pl *pool.Pool, rc *router.Client, hk *housekeep.Runner) *Server {
	lo, err := adminauth.NewLockout(cfg.TrustedProxyCIDRs)
	if err != nil {
		log.Printf("adminauth: trusted proxy CIDR: %v (using RemoteAddr only)", err)
		lo, _ = adminauth.NewLockout(nil)
	}
	s := &Server{
		cfg:       cfg,
		store:     st,
		catalog:   cat,
		pool:      pl,
		router:    rc,
		housekeep: hk,
		masterKey: keymaker.MasterKey(cfg.APIKeySeed),
		promptKey: keymaker.PromptKey(cfg.APIKeySeed),
		sessions:  adminauth.NewSessionStore(),
		lockout:   lo,
		audit:     NewAuditLog(500),
		outflow:   &OutflowAlert{},
	}
	s.applyBreakGlass()
	s.ensurePasswordFingerprint()
	// Boot audit: effective router MOR allowance (wei + source; no secrets).
	s.audit.RecordFull("allowance", "boot", "", cfg.MORAllowanceSource,
		"MOR allowance wei="+allowanceWeiOrUnknown(cfg.MORAllowanceWei)+" source="+cfg.MORAllowanceSource)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI-compatible surface.
	mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleInference("/v1/chat/completions")))
	mux.HandleFunc("POST /v1/embeddings", s.requireAPIKey(s.handleInference("/v1/embeddings")))
	mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	mux.HandleFunc("GET /v1/usage", s.requireAPIKey(s.handleUsageV1))

	// Auth (public login; session-gated logout / 2FA enroll).
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("POST /admin/sessions/revoke-all", s.requireAdmin(s.handleRevokeAllSessions))
	mux.HandleFunc("GET /admin/2fa/status", s.requireSession(s.handleTOTPStatus))
	mux.HandleFunc("POST /admin/2fa/enroll/start", s.requireSession(s.handleTOTPEnrollStart))
	mux.HandleFunc("POST /admin/2fa/enroll/confirm", s.requireSession(s.handleTOTPEnrollConfirm))
	mux.HandleFunc("POST /admin/2fa/remove", s.requireAdmin(s.handleTOTPRemove))
	mux.HandleFunc("POST /admin/2fa/recovery/regen", s.requireAdmin(s.handleRecoveryRegen))
	mux.HandleFunc("GET /admin/audit", s.requireAdmin(s.handleAudit))

	// Admin surface (session cookie).
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
	mux.HandleFunc("GET /admin/security", s.requireAdmin(s.handleSecurity))

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
		r.Header.Set("X-Uplink-Key-Id", keyID)
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

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

func allowanceWeiOrUnknown(wei string) string {
	if wei == "" {
		return "unknown"
	}
	return wei
}
