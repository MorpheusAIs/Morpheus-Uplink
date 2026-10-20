package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// requireSession is cookie+CSRF without the Admin2FARequired gate (for enroll).
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if s.cfg.MasterKeyAdmin && keymakerEqual(token, s.masterKey) {
				next(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw := adminauth.ReadSessionCookie(r, s.cfg.DevHTTP)
		poll := r.Header.Get("X-Uplink-Poll") == "1"
		if _, ok := s.sessions.Lookup(raw, !poll); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.checkCSRF(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleTOTPEnrollStart(w http.ResponseWriter, r *http.Request) {
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

// handleTOTPEnrollConfirm checks the first code against the pending secret
// and, in the same store update, activates 2FA and issues recovery codes.
func (s *Server) handleTOTPEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		http.Error(w, `{"code":"..."} required`, http.StatusBadRequest)
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
	s.auditEvent("2fa_enroll", ip, "ok", "")
	s.auditEvent("recovery_codes_issued", ip, "ok", "count="+strconv.Itoa(len(codes)))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"recoveryCodes": codes, // shown once; only salted hashes are stored
	})
}

// passwordAndFactor runs the shared checks for the password + second factor
// endpoints (remove, regenerate, reset): lockout, body, password, then an
// atomic second-factor consume. then runs inside the same store update on
// success. Returns false after writing the error response.
func (s *Server) passwordAndFactor(w http.ResponseWriter, r *http.Request, event string, then func(*store.AuthState)) (factorResult, bool) {
	ip := s.lockout.ClientIP(r)
	if s.lockedOut(w, ip) {
		return factorResult{}, false
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	res := s.consumeSecondFactor(req.Code, then)
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

// removeRefusedMsg is returned when ADMIN_2FA_REQUIRED=true: removing 2FA
// would leave a deploy that requires it without it. Reset re-enrolls.
const removeRefusedMsg = "2FA is required on this deploy (ADMIN_2FA_REQUIRED=true), so it cannot be removed; use Reset 2FA to re-enroll"

// handleTOTPRemove (POST /admin/2fa/remove) needs the password plus a TOTP
// or recovery code. Refused with 409 when ADMIN_2FA_REQUIRED=true (checked
// before any code is consumed). Otherwise it wipes 2FA and, like reset,
// revokes every admin session including this one.
func (s *Server) handleTOTPRemove(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Admin2FARequired {
		s.auditEvent("2fa_remove", s.lockout.ClientIP(r), "blocked", "2fa_required")
		http.Error(w, removeRefusedMsg, http.StatusConflict)
		return
	}
	if _, ok := s.passwordAndFactor(w, r, "2fa_remove", wipe2FA); !ok {
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
// The next login is password-only; with ADMIN_2FA_REQUIRED=true that
// session can only enroll again (requireAdmin), and enrolling issues new
// recovery codes.
func (s *Server) handleTOTPReset(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.passwordAndFactor(w, r, "2fa_reset", wipe2FA); !ok {
		return
	}
	n := s.sessions.RevokeAll()
	adminauth.ClearSessionCookie(w, s.cfg.DevHTTP)
	s.auditEvent("2fa_reset", s.lockout.ClientIP(r), "ok", "sessions_revoked="+strconv.Itoa(n))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"reenroll": true,
		"required": s.cfg.Admin2FARequired,
	})
}

func (s *Server) handleRecoveryRegen(w http.ResponseWriter, r *http.Request) {
	codes, hashes, err := s.issueRecoveryCodes()
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	replace := func(a *store.AuthState) {
		a.RecoveryCodes = hashes
	}
	if _, ok := s.passwordAndFactor(w, r, "recovery_codes_regenerated", replace); !ok {
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
	writeJSON(w, http.StatusOK, map[string]any{"events": s.audit.Recent(500)})
}

// handleTOTPStatus is session-gated (not Admin2FARequired) so a password holder
// can learn enrollment state and open the enroll UI before other admin routes work.
func (s *Server) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	auth := s.store.GetAuthState()
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled":       auth.Enrolled,
		"required":       s.cfg.Admin2FARequired,
		"breakGlassWarn": s.breakGlassWarn(),
		"need2fa":        s.cfg.Admin2FARequired && !auth.Enrolled,
		// Security switches set to a non-boolean (names only); each fell back
		// to its safe value.
		"configWarn": s.cfg.MalformedBoolVars,
		// Count only; lets the GUI hide "Use a recovery code" when none are left.
		"recoveryRemaining": auth.RecoveryRemaining(),
	})
}
