package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		"on":                auth.On(),
		"need2fa":           auth.On() && !auth.Enrolled,
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

// auditEvent writes one event. Unauthenticated failures (login,
// login_lockout, admin_bearer_reject) are aggregated per (client address,
// event) per minute; other events (a signed-in admin's failures included,
// which sit behind the session and the lockout) are written as is.
func (s *Server) auditEvent(event, ip, result, reason string) {
	st := AuditStreamFor(event, result)
	if st == AuditStreamOps && unauthAggregated[event] {
		s.opsAgg.Record(st, ip, auditEventOf(event, ip, result, reason))
		return
	}
	s.writeAudit(st, auditEventOf(event, ip, result, reason))
}

// unauthAggregated: failure kinds anyone can trigger without a session.
var unauthAggregated = map[string]bool{"login": true, "login_lockout": true, "admin_bearer_reject": true}

// auditAgent writes an event an API key can trigger (failure/ops stream),
// aggregated per (key id, event) per minute.
func (s *Server) auditAgent(event, keyID, ip, result, reason string) {
	ev := auditEventOf(event, ip, result, reason)
	ev.Actor = keyID
	s.opsAgg.Record(AuditStreamOps, keyID, ev)
}

// auditSystem writes a system event (no client): high-value unless its
// kind or result routes it to ops. quiet: the caller logged it already.
func (s *Server) auditSystem(kind, actor, result, message string, quiet bool) {
	s.writeAudit(AuditStreamFor(kind, result), AuditEvent{At: time.Now().UTC(), Kind: kind, Actor: actor, Result: result, Message: message, quiet: quiet})
}

func auditEventOf(event, ip, result, reason string) AuditEvent {
	msg := event + " " + result
	if reason != "" {
		msg += " " + reason
	}
	return AuditEvent{At: time.Now().UTC(), Kind: event, Actor: ip, IP: ip, Result: result, Message: msg, reason: reason}
}

// auditAggregated is auditEvent for loopable events: at most one line per
// (event, key) per minute, repeats summarised with a count. key groups
// events (for example a session) and is never written.
func (s *Server) auditAggregated(event, key, ip, result, reason string) {
	s.auditAgg.Record(AuditStreamFor(event, result), key, auditEventOf(event, ip, result, reason))
}

// writeAudit sends one event to the audit sink. Ended aggregation windows
// are written first, so summaries keep their order with later events.
func (s *Server) writeAudit(stream AuditStream, ev AuditEvent) {
	s.FlushAudit()
	s.auditSink.WriteAudit(stream, ev)
}

// FlushAudit writes the summaries of ended aggregation windows. main calls
// it once per aggregation window; it is also run on every audit write and
// before the audit list is read.
func (s *Server) FlushAudit() {
	s.auditAgg.Flush()
	s.opsAgg.Flush()
}

// PruneAudit applies the audit streams' retention (boot and daily).
func (s *Server) PruneAudit() {
	if s.auditFile != nil {
		s.auditFile.Prune()
	}
}

// AuditFlushInterval is how often FlushAudit should run.
const AuditFlushInterval = aggWindow

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
			ip := s.lockout.ClientIP(r)
			path := sanitizeAuditPath(r.URL.Path)
			// Per-IP-only lockout, checked before the key compare: it never
			// reads or feeds the global trip, so a password flood cannot
			// lock out the master Bearer and a Bearer flood cannot lock
			// out the password login.
			if ok, retry := s.lockout.AllowBearer(ip); !ok {
				s.auditEvent("admin_bearer_reject", ip, "blocked", "lockout path="+path)
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			if s.cfg.MasterKeyAdmin && keymakerEqual(token, s.masterKey) {
				s.lockout.SuccessBearer(ip)
				// Aggregated per client address per minute (loopable).
				s.auditAggregated("admin_key_use", ip, ip, "ok", path)
				next(w, r.WithContext(context.WithValue(r.Context(), adminAuthKey{}, adminAuth{bearer: true})))
				return
			}
			// Never the token or any part or hash of it: path and address.
			s.lockout.FailureBearer(ip)
			s.auditEvent("admin_bearer_reject", ip, "fail", "path="+path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		raw, sess, ok := s.cookieSession(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), adminAuthKey{}, adminAuth{session: sess, raw: raw}))

		if !s.checkCSRF(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		auth := s.store.GetAuthState()
		if auth.On() && !auth.Enrolled {
			// 2FA is on but not enrolled (after a reset): only login + enroll
			// endpoints are allowed (handled separately without this gate).
			http.Error(w, "2fa enrollment required", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// adminAuth is how a request on an admin route was authenticated.
type adminAuth struct {
	bearer  bool               // master key Bearer (MASTER_KEY_ADMIN=true)
	session *adminauth.Session // cookie session (copy taken at lookup)
	raw     string             // cookie token, for StampStepUp (never logged)
}

type adminAuthKey struct{}

func adminAuthFrom(r *http.Request) adminAuth {
	a, _ := r.Context().Value(adminAuthKey{}).(adminAuth)
	return a
}

// cookieSession looks up the admin cookie session (a poll request does not
// extend idle) and records a change of client address once per change in
// the audit. The address is not bound: the session stays valid.
func (s *Server) cookieSession(r *http.Request) (string, *adminauth.Session, bool) {
	raw := adminauth.ReadSessionCookie(r, s.cfg.DevHTTP)
	poll := r.Header.Get("X-Uplink-Poll") == "1"
	sess, ok := s.sessions.Lookup(raw, !poll)
	if !ok {
		return "", nil, false
	}
	ip := s.lockout.ClientIP(r)
	if prev, changed := s.sessions.ObserveIP(raw, ip); changed {
		reason := "previous=" + prev
		// Aggregated per session per minute; the key is the session hash
		// (never written).
		s.auditAggregated("admin_session_ip_change", sess.Hash, ip, "ok", reason)
		sess.IP = ip
	}
	return raw, sess, true
}

// sanitizeAuditPath keeps a path printable ASCII and at most 256 bytes.
func sanitizeAuditPath(p string) string {
	b := make([]byte, 0, len(p))
	for i := 0; i < len(p) && len(b) < 256; i++ {
		c := p[i]
		if c < 0x21 || c > 0x7e {
			c = '_'
		}
		b = append(b, c)
	}
	return string(b)
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
	// Everything is wiped, the 2FA switch included: the install comes back
	// with 2FA off (password-only login), as on a fresh deploy.
	auth = store.AuthState{BreakGlassDone: true, PasswordFingerprint: adminauth.PasswordFingerprint(s.cfg.APIKeySeed, s.cfg.AdminPassword)}
	_ = s.store.SetAuthState(auth)
	s.sessions.RevokeAll()
	s.lockout.Reset()
	s.auditEvent("2fa_reset_breakglass", "", "ok", "wiped")
}

func (s *Server) breakGlassWarn() bool {
	return s.cfg.Reset2FA && s.store.GetAuthState().BreakGlassDone
}
