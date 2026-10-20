package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

type loginRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"` // TOTP or recovery
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if ok, retry := s.lockout.Allow(ip); !ok {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		http.Error(w, `body must be {"password":"...","code":"...?"} `, http.StatusBadRequest)
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.cfg.AdminPassword)) != 1 {
		d := s.lockout.Failure(ip)
		s.auditEvent("login", ip, "fail", "bad_password")
		if d > 0 {
			s.auditEvent("login_lockout", ip, "blocked", "lockout")
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			http.Error(w, "too many failed logins", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	auth := s.store.GetAuthState()
	if auth.Enrolled {
		if req.Code == "" {
			// Correct password, missing code: prompt without burning a lockout strike.
			s.auditEvent("login", ip, "fail", "2fa_required")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		res := s.consumeSecondFactor(req.Code, nil)
		if !res.ok {
			s.failAuth(w, "login", ip, res.reason, "unauthorized")
			return
		}
		s.auditRecoveryUse(ip, res)
		auth = s.store.GetAuthState()
	}

	s.lockout.Success(ip)
	raw, err := s.sessions.Create(ip)
	if err != nil {
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	adminauth.SetSessionCookie(w, raw, s.cfg.DevHTTP)
	s.auditEvent("login", ip, "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"enrolled":          auth.Enrolled,
		"need2fa":           s.cfg.Admin2FARequired && !auth.Enrolled,
		"recoveryRemaining": auth.RecoveryRemaining(),
	})
}

// failAuth records a failed credential check, counts it toward the
// lockout, and answers 429 once the lockout trips, else 401.
func (s *Server) failAuth(w http.ResponseWriter, event, ip, reason, msg string) {
	d := s.lockout.Failure(ip)
	s.auditEvent(event, ip, "fail", reason)
	if d > 0 {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

// lockedOut answers 429 if ip is currently locked out.
func (s *Server) lockedOut(w http.ResponseWriter, ip string) bool {
	if ok, retry := s.lockout.Allow(ip); !ok {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return true
	}
	return false
}

func (s *Server) decryptTOTPSecret(blobB64, saltB64 string) (string, error) {
	blob, err := store.DecodeB64(blobB64)
	if err != nil {
		return "", err
	}
	salt, err := store.DecodeB64(saltB64)
	if err != nil {
		return "", err
	}
	key, err := adminauth.DeriveTOTPKey(s.cfg.APIKeySeed, salt)
	if err != nil {
		return "", err
	}
	pt, err := adminauth.DecryptSecret(key, blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	raw := adminauth.ReadSessionCookie(r, s.cfg.DevHTTP)
	if raw != "" {
		s.sessions.RevokeRaw(raw)
	}
	adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	s.auditEvent("session_logout", s.lockout.ClientIP(r), "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	n := s.sessions.RevokeAll()
	adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	s.auditEvent("session_revoke_all", s.lockout.ClientIP(r), "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (s *Server) auditEvent(event, ip, result, reason string) {
	attrs := []any{"event", event, "result", result, "ip", ip}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	slog.Info("audit", attrs...)
	msg := event + " " + result
	if reason != "" {
		msg += " " + reason
	}
	s.audit.RecordFull(event, ip, ip, result, msg)
}

// requireAdmin enforces cookie session (or optional master Bearer) + CSRF + 2FA gate.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reject Basic auth entirely.
		if _, _, ok := r.BasicAuth(); ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Optional master key admin (default off).
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if s.cfg.MasterKeyAdmin && keymakerEqual(token, s.masterKey) {
				s.auditEvent("admin_key_use", s.lockout.ClientIP(r), "ok", r.URL.Path)
				next(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		raw := adminauth.ReadSessionCookie(r, s.cfg.DevHTTP)
		poll := r.Header.Get("X-Uplink-Poll") == "1"
		sess, ok := s.sessions.Lookup(raw, !poll)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = sess

		if !s.checkCSRF(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		auth := s.store.GetAuthState()
		if s.cfg.Admin2FARequired && !auth.Enrolled {
			// Only login + enroll endpoints allowed (handled separately without this gate).
			http.Error(w, "2fa enrollment required", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

func keymakerEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ensurePasswordFingerprint logs password_change when env password rotates.
func (s *Server) ensurePasswordFingerprint() {
	fp := adminauth.PasswordFingerprint(s.cfg.APIKeySeed, s.cfg.AdminPassword)
	auth := s.store.GetAuthState()
	if auth.PasswordFingerprint == "" {
		auth.PasswordFingerprint = fp
		_ = s.store.SetAuthState(auth)
		return
	}
	if auth.PasswordFingerprint != fp {
		s.auditEvent("password_change", "", "ok", "")
		auth.PasswordFingerprint = fp
		_ = s.store.SetAuthState(auth)
	}
}

// applyBreakGlass handles UPLINK_RESET_2FA once-per-marker.
func (s *Server) applyBreakGlass() {
	auth := s.store.GetAuthState()
	if !s.cfg.Reset2FA {
		if auth.BreakGlassDone {
			auth.BreakGlassDone = false
			_ = s.store.SetAuthState(auth)
		}
		return
	}
	if auth.BreakGlassDone {
		s.auditEvent("2fa_reset_breakglass", "", "blocked", "already wiped; remove UPLINK_RESET_2FA")
		return
	}
	auth = store.AuthState{BreakGlassDone: true, PasswordFingerprint: adminauth.PasswordFingerprint(s.cfg.APIKeySeed, s.cfg.AdminPassword)}
	_ = s.store.SetAuthState(auth)
	s.sessions.RevokeAll()
	s.lockout.Reset()
	s.auditEvent("2fa_reset_breakglass", "", "ok", "wiped")
}

func (s *Server) breakGlassWarn() bool {
	return s.cfg.Reset2FA && s.store.GetAuthState().BreakGlassDone
}
