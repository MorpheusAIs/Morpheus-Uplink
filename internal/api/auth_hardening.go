package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Login lockout with exponential backoff per client key (IP).
// Only Basic-auth password guesses count; Bearer master-key automation does not.
type loginGuard struct {
	mu   sync.Mutex
	hits map[string]*loginState
}

type loginState struct {
	failures    int
	lockedUntil time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{hits: map[string]*loginState{}}
}

func clientKey(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *loginGuard) allow(key string) (ok bool, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.hits[key]
	if st == nil {
		return true, 0
	}
	now := time.Now()
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	return true, 0
}

func (g *loginGuard) success(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hits, key)
}

// failure records a Basic-auth miss. Lockout starts after 5 failures.
func (g *loginGuard) failure(key string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.hits[key]
	if st == nil {
		st = &loginState{}
		g.hits[key] = st
	}
	st.failures++
	if st.failures < 5 {
		return 0
	}
	shift := st.failures - 5
	if shift > 10 {
		shift = 10
	}
	d := time.Second << shift
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	st.lockedUntil = time.Now().Add(d)
	return d
}
