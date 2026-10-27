package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

// StepUpMaxAge: GET /admin/keys?reveal=1 reveals only when the session
// logged in or stepped up within this long.
const StepUpMaxAge = 30 * time.Minute

const (
	errMsgTOTPOnly = "authenticator code required (recovery codes are not accepted here)"
	errMsgStepUp   = "step_up_required"
	// errMsgBadPassword: wrong password on an enrolment call made inside a
	// live session, so the GUI can tell it from a lost session (401
	// "unauthorized"). /admin/login already tells a wrong password apart.
	errMsgBadPassword   = "invalid password"
	maxCredentialBodyKB = 4
)

// decodeCredBody reads a small JSON credential body. Credentials are only
// ever read from a POST body, never from a query string.
func decodeCredBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialBodyKB<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// handleStepUp (POST /admin/step-up {password, code}) re-checks the admin
// credentials on the current cookie session (requireSession: cookie only,
// any Authorization header refused, CSRF-checked). Order: lockout, body,
// password, then (2FA enrolled) a TOTP code only, consumed replay-safe via
// LastTOTPStep. A recovery code is refused before anything is consumed.
// With 2FA not enrolled the password alone is checked. Every bad attempt
// counts toward the per-IP lockout. Success stamps StepUpAt on the session
// and writes step_up.
func (s *Server) handleStepUp(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return
	}
	auth := adminAuthFrom(r)
	if auth.session == nil || auth.raw == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decodeCredBody(w, r, &req); err != nil {
		http.Error(w, `body must be {"password":"...","code":"..."}`, http.StatusBadRequest)
		return
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		s.failAuth(w, "step_up", ip, "bad_password", "unauthorized")
		return
	}
	method := "password"
	if s.store.GetAuthState().Enrolled {
		code := strings.TrimSpace(req.Code)
		if code == "" {
			s.failAuth(w, "step_up", ip, "code_required", "unauthorized")
			return
		}
		if adminauth.LooksLikeRecoveryCode(code) {
			s.failAuth(w, "step_up", ip, "recovery_code_refused", errMsgTOTPOnly)
			return
		}
		res := s.consumeTOTP(code, nil)
		if !res.ok {
			s.failAuth(w, "step_up", ip, res.reason, "unauthorized")
			return
		}
		method = "totp"
	}
	at, ok := s.sessions.StampStepUp(auth.raw)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.lockout.Success(ip)
	s.auditEvent("step_up", ip, "ok", "method="+method)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"stepUpAt":    at,
		"validForSec": int(StepUpMaxAge.Seconds()),
	})
}

// revealRequested is true when any reveal query value asks for the full
// keys (reveal=1 or reveal=true).
func revealRequested(r *http.Request) bool {
	for _, v := range r.URL.Query()["reveal"] {
		if v == "1" || v == "true" {
			return true
		}
	}
	return false
}

// refuseRevealOverBearer answers 403 to GET /admin/keys?reveal=1 sent with
// any Authorization header, whatever MASTER_KEY_ADMIN is set to, before any
// key compare: full keys are revealed only to a stepped-up cookie session.
// The masked listing (no reveal) keeps the Bearer path of requireAdmin.
func (s *Server) refuseRevealOverBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if revealRequested(r) && len(r.Header.Values("Authorization")) > 0 {
			if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && keymaker.Equal(token, s.masterKey) {
				ip := s.lockout.ClientIP(r)
				s.auditAggregated("admin_key_use", ip, ip, "blocked", "reveal_refused")
			}
			http.Error(w, "key reveal needs an admin session", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
