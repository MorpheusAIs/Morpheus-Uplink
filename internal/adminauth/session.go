package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// Fixed admin session lengths. Compile-time constants: the same for
	// every 2FA state, with no env var or setting to change them.
	SessionAbsolute = 12 * time.Hour
	SessionIdle     = 4 * time.Hour
	MaxSessions     = 10

	CookieHost = "__Host-uplink_admin"
	CookieDev  = "uplink_admin_dev"
)

type Session struct {
	Hash      string
	CreatedAt time.Time
	LastSeen  time.Time
	IP        string // client address last seen on this session
	// StepUpAt is the last successful POST /admin/step-up (zero if none).
	StepUpAt time.Time
}

// Fresh reports whether the session logged in or stepped up within maxAge
// of now (StepUpAt, or CreatedAt when it never stepped up).
func (s *Session) Fresh(now time.Time, maxAge time.Duration) bool {
	at := s.CreatedAt
	if s.StepUpAt.After(at) {
		at = s.StepUpAt
	}
	return now.Sub(at) <= maxAge
}

type SessionStore struct {
	mu     sync.Mutex
	byHash map[string]*Session // hash -> session
	now    func() time.Time    // injectable clock (tests); nil = time.Now
}

func (s *SessionStore) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	if now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}

// clockLocked is clock for callers that already hold s.mu.
func (s *SessionStore) clockLocked() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func NewSessionStore() *SessionStore {
	return &SessionStore{byHash: map[string]*Session{}}
}

// Idle and Absolute are the fixed session lengths (SessionIdle,
// SessionAbsolute).
func (s *SessionStore) Idle() time.Duration     { return SessionIdle }
func (s *SessionStore) Absolute() time.Duration { return SessionAbsolute }

// Now is the store's clock (UTC), for age checks made outside the store.
func (s *SessionStore) Now() time.Time { return s.clock() }

// SetClock replaces the clock (tests only; nil restores time.Now).
func (s *SessionStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

func (s *SessionStore) Create(ip string) (rawToken string, err error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clockLocked()
	// Evict oldest if at cap.
	for len(s.byHash) >= MaxSessions {
		var oldestHash string
		var oldestTime time.Time
		first := true
		for h, sess := range s.byHash {
			if first || sess.CreatedAt.Before(oldestTime) {
				oldestHash = h
				oldestTime = sess.CreatedAt
				first = false
			}
		}
		delete(s.byHash, oldestHash)
	}
	s.byHash[hash] = &Session{Hash: hash, CreatedAt: now, LastSeen: now, IP: ip}
	return hex.EncodeToString(raw), nil
}

func (s *SessionStore) Lookup(rawToken string, extendIdle bool) (*Session, bool) {
	raw, err := hex.DecodeString(rawToken)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byHash[hash]
	if sess == nil {
		return nil, false
	}
	now := s.clockLocked()
	if now.Sub(sess.CreatedAt) > SessionAbsolute {
		delete(s.byHash, hash)
		return nil, false
	}
	if now.Sub(sess.LastSeen) > SessionIdle {
		delete(s.byHash, hash)
		return nil, false
	}
	if extendIdle {
		sess.LastSeen = now
	}
	cp := *sess
	return &cp, true
}

func hashRaw(rawToken string) (string, bool) {
	raw, err := hex.DecodeString(rawToken)
	if err != nil || len(raw) != 32 {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

func (s *SessionStore) RevokeRaw(rawToken string) {
	raw, err := hex.DecodeString(rawToken)
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	delete(s.byHash, hash)
	s.mu.Unlock()
}

// StampStepUp records a successful step-up on the session. False when the
// session no longer exists.
func (s *SessionStore) StampStepUp(rawToken string) (time.Time, bool) {
	hash, ok := hashRaw(rawToken)
	if !ok {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byHash[hash]
	if sess == nil {
		return time.Time{}, false
	}
	sess.StepUpAt = s.clockLocked()
	return sess.StepUpAt, true
}

// ObserveIP records ip as the session's current client address. changed is
// true (with the previous address) only when it differs from the last one
// recorded, so each change is reported once. There is no binding: the
// session stays valid.
func (s *SessionStore) ObserveIP(rawToken, ip string) (prev string, changed bool) {
	hash, ok := hashRaw(rawToken)
	if !ok || ip == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byHash[hash]
	if sess == nil || sess.IP == ip {
		return "", false
	}
	prev, sess.IP = sess.IP, ip
	return prev, true
}

// RevokeAllExcept revokes every session but the one for keepRaw and returns
// how many were revoked.
func (s *SessionStore) RevokeAllExcept(keepRaw string) int {
	keep, _ := hashRaw(keepRaw)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for h := range s.byHash {
		if h != keep {
			delete(s.byHash, h)
			n++
		}
	}
	return n
}

func (s *SessionStore) RevokeAll() int {
	s.mu.Lock()
	n := len(s.byHash)
	s.byHash = map[string]*Session{}
	s.mu.Unlock()
	return n
}

func CookieName(devHTTP bool) string {
	if devHTTP {
		return CookieDev
	}
	return CookieHost
}

// SetSessionCookie sets the admin cookie; MaxAge is SessionAbsolute.
// Flags: HttpOnly, SameSite=Strict, Secure unless devHTTP.
func SetSessionCookie(w http.ResponseWriter, rawToken string, devHTTP bool) {
	c := &http.Cookie{
		Name:     CookieName(devHTTP),
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(SessionAbsolute.Seconds()),
	}
	if !devHTTP {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

func ClearSessionCookie(w http.ResponseWriter, devHTTP bool) {
	c := &http.Cookie{
		Name:     CookieName(devHTTP),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
	if !devHTTP {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

func ReadSessionCookie(r *http.Request, devHTTP bool) string {
	c, err := r.Cookie(CookieName(devHTTP))
	if err != nil {
		return ""
	}
	return c.Value
}
