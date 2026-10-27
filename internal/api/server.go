// Package api wires the UPLINK HTTP surface:
//
//	/v1/*      OpenAI-compatible, Bearer sk-… auth (master or generated keys)
//	/admin/*   management API, HttpOnly session cookie (no Basic)
//	/node/*    passthrough to the raw proxy-router :8082 API, admin-gated
//	/gui       embedded single-page management UI
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

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
	auditSink AuditSink
	auditFile *auditSink
	auditAgg  *auditAggregator // loopable high-value events
	opsAgg    *auditAggregator // failure/ops events
	outflow   *OutflowAlert
	inflight  *inflightLimiter

	settings   *runtimeSettings // S6: effective operator settings
	settingsMu sync.Mutex       // serialises setting changes

	walletMu   sync.Mutex
	lastWallet string // last address the router returned (never blanked)

}

func NewServer(cfg *config.Config, st *store.Store, cat *catalog.Catalog, pl *pool.Pool, rc *router.Client, hk *housekeep.Runner) *Server {
	lo, err := adminauth.NewLockout(cfg.TrustedProxyCIDRs)
	if err != nil {
		warnf("adminauth: trusted proxy CIDR: %v (using RemoteAddr only)", err)
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
		inflight:  newInflightLimiter(cfg.InflightMax),
		outflow:   &OutflowAlert{},
	}
	s.auditFile = openAuditSink(s.audit, cfg.DataDir, time.Duration(auditRetentionDays(cfg))*24*time.Hour)
	s.auditSink = s.auditFile
	emit := func(st AuditStream, ev AuditEvent) { s.auditSink.WriteAudit(st, ev) }
	s.auditAgg = newAuditAggregator(aggWindow, aggMaxKeys, aggMaxKeys*16, emit)
	s.opsAgg = newAuditAggregator(aggWindow, opsAggMaxKeys, opsAggMaxKeys, emit)
	var persisted store.Settings
	if st != nil {
		persisted = st.GetSettings()
	}
	var notes []string
	s.settings, notes = resolveSettings(cfg, persisted)
	for _, n := range notes {
		s.auditSystem("setting_ignored", "boot", "ignored", n, true)
	}
	s.applySettings()
	s.applyBreakGlass()
	s.ensurePasswordFingerprint()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI-compatible surface.
	mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleInference("/v1/chat/completions")))
	mux.HandleFunc("POST /v1/embeddings", s.requireAPIKey(s.handleInference("/v1/embeddings")))
	mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	mux.HandleFunc("GET /v1/usage", s.requireAPIKey(s.handleUsageV1))
	mux.HandleFunc("POST /v1/sessions/release", s.requireAPIKey(s.handleSessionRelease))

	// Auth (public login; session-gated logout / 2FA enroll).
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("POST /admin/sessions/revoke-all", s.requireAdmin(s.handleRevokeAllSessions))
	mux.HandleFunc("GET /admin/2fa/status", s.requireSession(s.handleTOTPStatus))
	mux.HandleFunc("POST /admin/2fa/enroll/start", s.requireSession(s.handleTOTPEnrollStart))
	mux.HandleFunc("POST /admin/2fa/enroll/confirm", s.requireSession(s.handleTOTPEnrollConfirm))
	mux.HandleFunc("POST /admin/step-up", s.requireSession(s.handleStepUp))
	mux.HandleFunc("POST /admin/2fa/remove", s.requireAdmin(s.handleTOTPRemove))
	mux.HandleFunc("POST /admin/2fa/recovery/regen", s.requireAdmin(s.handleRecoveryRegen))
	mux.HandleFunc("POST /admin/2fa/reset", s.requireAdmin(s.handleTOTPReset))
	mux.HandleFunc("GET /admin/audit", s.requireAdmin(s.handleAudit))

	// Admin surface (session cookie).
	mux.HandleFunc("GET /admin/keys", s.refuseRevealOverBearer(s.requireAdmin(s.handleListKeys)))
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
	mux.HandleFunc("GET /admin/models", s.requireAdmin(s.handleAdminModels))
	mux.HandleFunc("POST /admin/disk/prune-usage", s.requireAdmin(s.handlePruneUsage))
	mux.HandleFunc("GET /admin/security", s.requireAdmin(s.handleSecurity))
	mux.HandleFunc("GET /admin/settings", s.requireConsoleSession(s.handleSettingsGet))
	mux.HandleFunc("POST /admin/settings", s.requireConsoleSession(s.handleSettingsSet))

	// GUI + health.
	mux.Handle("/gui/", http.StripPrefix("/gui", gui.Handler()))
	mux.HandleFunc("GET /gui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /healthcheck", s.handleHealth)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gui/", http.StatusFound)
	})

	// Raw router passthrough (admin-gated, deny list in nodeproxy.go). It is
	// dispatched on the raw request path, ahead of ServeMux path cleaning, so
	// every /node/ request is checked by the passthrough itself.
	node := s.requireAdminHandler(s.nodeProxy())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/node/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		if strings.HasPrefix(rawRequestPath(r), "/node/") {
			node.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
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
		next(w, withKeyID(r, keyID))
	}
}

func (s *Server) requireAdminHandler(next http.Handler) http.Handler {
	return s.requireAdmin(next.ServeHTTP)
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

func auditRetentionDays(cfg *config.Config) int {
	if cfg == nil || cfg.AuditRetentionDays <= 0 {
		return 90
	}
	return cfg.AuditRetentionDays
}
