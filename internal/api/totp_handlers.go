package api

import (
	"crypto/subtle"
	"encoding/json"
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
		if _, ok := s.sessions.Lookup(raw, true); !ok {
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
	auth := s.store.GetAuthState()
	if auth.Enrolled {
		http.Error(w, "already enrolled", http.StatusConflict)
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
	auth.TOTPSalt = store.EncodeB64(salt)
	auth.PendingSecretEnc = store.EncodeB64(enc)
	auth.PendingExpires = time.Now().UTC().Add(10 * time.Minute)
	if err := s.store.SetAuthState(auth); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	qr := adminauth.QRPNGBase64(key.URL())
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":    key.Secret(),
		"otpauth":   key.URL(),
		"qrPngB64":  qr, // empty if QR lib failed (stdlib fallback: show secret)
		"expiresAt": auth.PendingExpires,
	})
}

func (s *Server) handleTOTPEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if ok, retry := s.lockout.Allow(ip); !ok {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		http.Error(w, `{"code":"..."} required`, http.StatusBadRequest)
		return
	}
	auth := s.store.GetAuthState()
	if auth.Enrolled {
		http.Error(w, "already enrolled", http.StatusConflict)
		return
	}
	if auth.PendingSecretEnc == "" || time.Now().UTC().After(auth.PendingExpires) {
		http.Error(w, "no pending enrollment", http.StatusBadRequest)
		return
	}
	blob, err := store.DecodeB64(auth.PendingSecretEnc)
	if err != nil {
		http.Error(w, "pending corrupt", http.StatusInternalServerError)
		return
	}
	salt, _ := store.DecodeB64(auth.TOTPSalt)
	aesKey, err := adminauth.DeriveTOTPKey(s.cfg.APIKeySeed, salt)
	if err != nil {
		http.Error(w, "derive failed", http.StatusInternalServerError)
		return
	}
	secretBytes, err := adminauth.DecryptSecret(aesKey, blob)
	if err != nil {
		http.Error(w, "decrypt failed", http.StatusInternalServerError)
		return
	}
	secret := string(secretBytes)
	ok, step, err := adminauth.ValidateTOTP(secret, req.Code, 0)
	if err != nil || !ok {
		d := s.lockout.Failure(ip)
		s.auditEvent("login", ip, "fail", "bad_totp_enroll")
		if d > 0 {
			s.auditEvent("login_lockout", ip, "blocked", "lockout")
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			http.Error(w, "too many failed logins", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	s.lockout.Success(ip)
	// Fresh encrypt for stored blob (new nonce).
	enc, err := adminauth.EncryptSecret(aesKey, secretBytes)
	if err != nil {
		http.Error(w, "encrypt failed", http.StatusInternalServerError)
		return
	}
	codes, hashes, err := generateRecoveryCodes(10)
	if err != nil {
		http.Error(w, "recovery codes failed", http.StatusInternalServerError)
		return
	}
	auth.TOTPBlob = store.EncodeB64(enc)
	auth.RecoveryHashes = hashes
	auth.Enrolled = true
	auth.LastTOTPStep = step
	auth.PendingSecretEnc = ""
	auth.PendingExpires = time.Time{}
	if err := s.store.SetAuthState(auth); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	s.auditEvent("2fa_enroll", s.lockout.ClientIP(r), "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"recoveryCodes": codes, // shown once
	})
}

func generateRecoveryCodes(n int) (plain []string, hashes []string, err error) {
	for i := 0; i < n; i++ {
		c, err := adminauth.RandomBase32(10)
		if err != nil {
			return nil, nil, err
		}
		plain = append(plain, c)
		hashes = append(hashes, adminauth.HashString(c))
	}
	return plain, hashes, nil
}

func (s *Server) handleTOTPRemove(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if ok, retry := s.lockout.Allow(ip); !ok {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		d := s.lockout.Failure(ip)
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
	if !auth.Enrolled {
		http.Error(w, "not enrolled", http.StatusBadRequest)
		return
	}
	ok, _ := s.verifySecondFactor(req.Code, &auth, ip)
	if !ok {
		d := s.lockout.Failure(ip)
		if d > 0 {
			s.auditEvent("login_lockout", ip, "blocked", "lockout")
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			http.Error(w, "too many failed logins", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	auth.TOTPBlob = ""
	auth.RecoveryHashes = nil
	auth.Enrolled = false
	auth.LastTOTPStep = 0
	auth.PendingSecretEnc = ""
	_ = s.store.SetAuthState(auth)
	s.auditEvent("2fa_remove", ip, "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRecoveryRegen(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	if ok, retry := s.lockout.Allow(ip); !ok {
		s.auditEvent("login_lockout", ip, "blocked", "lockout")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if subtleCompare(req.Password, s.cfg.AdminPassword) != 1 {
		d := s.lockout.Failure(ip)
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
	if !auth.Enrolled {
		http.Error(w, "not enrolled", http.StatusBadRequest)
		return
	}
	ok, _ := s.verifySecondFactor(req.Code, &auth, ip)
	if !ok {
		d := s.lockout.Failure(ip)
		if d > 0 {
			s.auditEvent("login_lockout", ip, "blocked", "lockout")
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			http.Error(w, "too many failed logins", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	codes, hashes, err := generateRecoveryCodes(10)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	auth.RecoveryHashes = hashes
	_ = s.store.SetAuthState(auth)
	s.auditEvent("recovery_codes_regen", ip, "ok", "")
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
	})
}
