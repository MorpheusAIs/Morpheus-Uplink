package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// requireSession is cookie+CSRF without the enrollment gate (for logout,
// 2FA status and enroll). These routes need an admin cookie session: any
// Authorization header is refused on session-only routes (any scheme or
// case, empty, or repeated; the master key with MASTER_KEY_ADMIN=true
// included) with the same 401 as an unauthenticated request.
// requireAdmin keeps the MASTER_KEY_ADMIN Bearer path and its audit record.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) > 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, sess, ok := s.cookieSession(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.checkCSRF(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminAuthKey{}, adminAuth{session: sess, raw: raw})))
	}
}

// handleTOTPEnrollStart (POST /admin/2fa/enroll/start {password}) needs the
// admin password in the body (lockout applies) before a secret is issued.
func (s *Server) handleTOTPEnrollStart(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeCredBody(w, r, &req); err != nil {
		http.Error(w, `{"password":"..."} required`, http.StatusBadRequest)
		return
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		s.failAuth(w, "2fa_enroll", ip, "bad_password", "unauthorized")
		return
	}
	key, err := adminauth.GenerateTOTP("Morpheus-Uplink", "admin")
	if err != nil {
		http.Error(w, "totp generate failed", http.StatusInternalServerError)
		return
	}
	salt, err := adminauth.RandomBytes(16)
	if err != nil {
		http.Error(w, "salt failed", http.StatusInternalServerError)
		return
	}
	aesKey, err := adminauth.DeriveTOTPKey(s.cfg.APIKeySeed, salt)
	if err != nil {
		http.Error(w, "derive failed", http.StatusInternalServerError)
		return
	}
	enc, err := adminauth.EncryptSecret(aesKey, []byte(key.Secret()))
	if err != nil {
		http.Error(w, "encrypt failed", http.StatusInternalServerError)
		return
	}
	expires := time.Now().UTC().Add(10 * time.Minute)
	err = s.store.UpdateAuthState(func(a *store.AuthState) error {
		if a.Enrolled {
			return errAlreadyEnrolled
		}
		a.TOTPSalt = store.EncodeB64(salt)
		a.PendingSecretEnc = store.EncodeB64(enc)
		a.PendingExpires = expires
		return nil
	})
	if errors.Is(err, errAlreadyEnrolled) {
		http.Error(w, "already enrolled", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	qr := adminauth.QRPNGBase64(key.URL())
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":    key.Secret(),
		"otpauth":   key.URL(),
		"qrPngB64":  qr, // empty if QR lib failed (stdlib fallback: show secret)
		"expiresAt": expires,
	})
}

var (
	errAlreadyEnrolled = errors.New("already enrolled")
	errNoPending       = errors.New("no pending enrollment")
	errBadCode         = errors.New("invalid code")
)

// handleTOTPEnrollConfirm (POST /admin/2fa/enroll/confirm {password, code})
// checks the password, then the first code against the pending secret and,
// in the same store update, stores the secret, turns the 2FA switch on and
// issues recovery codes. On success every admin session is revoked and the
// caller gets a new one.
func (s *Server) handleTOTPEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decodeCredBody(w, r, &req); err != nil || req.Code == "" {
		http.Error(w, `{"password":"...","code":"..."} required`, http.StatusBadRequest)
		return
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		s.failAuth(w, "2fa_enroll", ip, "bad_password", "unauthorized")
		return
	}
	codes, hashes, err := s.issueRecoveryCodes()
	if err != nil {
		http.Error(w, "recovery codes failed", http.StatusInternalServerError)
		return
	}
	err = s.store.UpdateAuthState(func(a *store.AuthState) error {
		if a.Enrolled {
			return errAlreadyEnrolled
		}
		if a.PendingSecretEnc == "" || time.Now().UTC().After(a.PendingExpires) {
			return errNoPending
		}
		blob, err := store.DecodeB64(a.PendingSecretEnc)
		if err != nil {
			return err
		}
		salt, err := store.DecodeB64(a.TOTPSalt)
		if err != nil {
			return err
		}
		aesKey, err := adminauth.DeriveTOTPKey(s.cfg.APIKeySeed, salt)
		if err != nil {
			return err
		}
		secret, err := adminauth.DecryptSecret(aesKey, blob)
		if err != nil {
			return err
		}
		ok, step, verr := adminauth.ValidateTOTP(string(secret), req.Code, 0)
		if verr != nil || !ok {
			return errBadCode
		}
		enc, err := adminauth.EncryptSecret(aesKey, secret) // fresh nonce for the stored blob
		if err != nil {
			return err
		}
		a.TOTPBlob = store.EncodeB64(enc)
		a.RecoveryCodes = hashes
		a.Enrolled = true
		a.TwoFactorOn = true
		a.LastTOTPStep = step
		a.PendingSecretEnc = ""
		a.PendingExpires = time.Time{}
		return nil
	})
	switch {
	case errors.Is(err, errBadCode):
		s.failAuth(w, "login", ip, "bad_totp_enroll", "invalid code")
		return
	case errors.Is(err, errAlreadyEnrolled):
		http.Error(w, "already enrolled", http.StatusConflict)
		return
	case errors.Is(err, errNoPending):
		http.Error(w, "no pending enrollment", http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "enroll failed", http.StatusInternalServerError)
		return
	}
	s.lockout.Success(ip)
	// Only now (password and code both checked, enrolment stored): sign out
	// every other admin session and re-issue the caller's.
	n := s.sessions.RevokeAll()
	if raw, err := s.sessions.Create(ip); err == nil {
		adminauth.SetSessionCookie(w, raw, s.cfg.DevHTTP, s.sessions.Absolute())
	} else {
		adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	}
	s.auditEvent("2fa_enroll", ip, "ok", "sessions_revoked="+strconv.Itoa(n))
	s.auditEvent("recovery_codes_issued", ip, "ok", "count="+strconv.Itoa(len(codes)))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"recoveryCodes": codes, // shown once; only salted hashes are stored
	})
}

// passwordAndFactor runs the shared checks for the password + second factor
// endpoints (remove, regenerate, reset): lockout, body, password, then an
// atomic second-factor consume. then runs inside the same store update on
// success. With allowRecovery false (regenerate) only a TOTP code is
// accepted, and a recovery code is refused before anything is consumed.
// Returns false after writing the error response.
func (s *Server) passwordAndFactor(w http.ResponseWriter, r *http.Request, event string, allowRecovery bool, then func(*store.AuthState)) (factorResult, bool) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return factorResult{}, false
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decodeCredBody(w, r, &req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return factorResult{}, false
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		s.failAuth(w, event, ip, "bad_password", "unauthorized")
		return factorResult{}, false
	}
	if !s.store.GetAuthState().Enrolled {
		http.Error(w, "not enrolled", http.StatusBadRequest)
		return factorResult{}, false
	}
	if !allowRecovery && adminauth.LooksLikeRecoveryCode(strings.TrimSpace(req.Code)) {
		// Refused before consumeFactor: the recovery code stays unused.
		s.failAuth(w, event, ip, "recovery_code_refused", errMsgTOTPOnly)
		return factorResult{}, false
	}
	var res factorResult
	if allowRecovery {
		res = s.consumeSecondFactor(req.Code, then)
	} else {
		res = s.consumeTOTP(req.Code, then)
	}
	if !res.ok {
		if res.reason == "not_enrolled" {
			http.Error(w, "not enrolled", http.StatusBadRequest)
			return res, false
		}
		s.failAuth(w, event, ip, res.reason, "unauthorized")
		return res, false
	}
	s.lockout.Success(ip)
	s.auditRecoveryUse(ip, res)
	return res, true
}

func wipe2FA(a *store.AuthState) {
	a.TOTPBlob = ""
	a.TOTPSalt = ""
	a.RecoveryCodes = nil
	a.Enrolled = false
	a.LastTOTPStep = 0
	a.PendingSecretEnc = ""
	a.PendingExpires = time.Time{}
}

// turnOff2FA wipes the secret and recovery codes and turns the switch off,
// all in one store update.
func turnOff2FA(a *store.AuthState) {
	wipe2FA(a)
	a.TwoFactorOn = false
}

// handleTOTPRemove (POST /admin/2fa/remove) turns 2FA off. It needs a valid
// session and CSRF (requireAdmin), the password plus a TOTP or recovery
// code (consumed; lockout applies), then wipes 2FA, turns the switch off
// and revokes every admin session including this one. The next login is
// password-only.
func (s *Server) handleTOTPRemove(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.passwordAndFactor(w, r, "2fa_remove", true, turnOff2FA); !ok {
		return
	}
	n := s.sessions.RevokeAll()
	adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	s.auditEvent("2fa_remove", s.lockout.ClientIP(r), "ok", "sessions_revoked="+strconv.Itoa(n))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTOTPReset (POST /admin/2fa/reset) needs the password plus a TOTP or
// recovery code. In one store update it wipes the TOTP secret and all
// recovery codes; then it revokes every admin session, including this one.
// The 2FA switch stays on, so the next (password-only) session can only
// enroll again (requireAdmin), and enrolling issues new recovery codes.
func (s *Server) handleTOTPReset(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.passwordAndFactor(w, r, "2fa_reset", true, wipe2FA); !ok {
		return
	}
	n := s.sessions.RevokeAll()
	adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	s.auditEvent("2fa_reset", s.lockout.ClientIP(r), "ok", "sessions_revoked="+strconv.Itoa(n))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"reenroll": true,
	})
}

// handleRecoveryRegen (POST /admin/2fa/recovery/regen) needs the password
// plus a current TOTP code; a recovery code is not accepted here.
func (s *Server) handleRecoveryRegen(w http.ResponseWriter, r *http.Request) {
	codes, hashes, err := s.issueRecoveryCodes()
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	replace := func(a *store.AuthState) {
		a.RecoveryCodes = hashes
	}
	if _, ok := s.passwordAndFactor(w, r, "recovery_codes_regenerated", false, replace); !ok {
		return
	}
	s.auditEvent("recovery_codes_regenerated", s.lockout.ClientIP(r), "ok", "count="+strconv.Itoa(len(codes)))
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.FlushAudit()
	writeJSON(w, http.StatusOK, map[string]any{"events": s.audit.Recent(500)})
}

// handleTOTPStatus is session-gated (not the enrollment gate) so a password
// holder can learn the 2FA state and open the enroll UI before other admin
// routes work.
func (s *Server) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	auth := s.store.GetAuthState()
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled":       auth.Enrolled,
		"on":             auth.On(),
		"breakGlassWarn": s.breakGlassWarn(),
		"need2fa":        auth.On() && !auth.Enrolled,
		// Security switches set to a non-boolean (names only); each fell back
		// to its safe value.
		"configWarn": s.cfg.MalformedBoolVars,
		// Count only; lets the GUI hide "Use a recovery code" when none are left.
		"recoveryRemaining": auth.RecoveryRemaining(),
	})
}
