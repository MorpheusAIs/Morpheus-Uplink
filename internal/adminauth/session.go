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
	SessionAbsolute = 8 * time.Hour
	SessionIdle     = 30 * time.Minute
	MaxSessions     = 10

	CookieHost = "__Host-uplink_admin"
	CookieDev  = "uplink_admin_dev"
)

type Session struct {
	Hash      string
	CreatedAt time.Time
	LastSeen  time.Time
	IP        string
}

type SessionStore struct {
	mu   sync.Mutex
	byHash map[string]*Session // hash -> session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{byHash: map[string]*Session{}}
}

func (s *SessionStore) Create(ip string) (rawToken string, err error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
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
	now := time.Now().UTC()
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
