package adminauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Lockout implements per-IP and global failure backoff per the design pack.
type Lockout struct {
	mu sync.Mutex

	perIP          map[string]*ipState
	global         []time.Time // recent failure timestamps
	globalUntil    time.Time
	globalBackoff  time.Duration
	globalLastTrip time.Time

	trusted []*net.IPNet
}

const maxTrackedIPs = 10000
const globalDecayAfter = time.Hour

type ipState struct {
	failures    int
	lockedUntil time.Time
	lastFail    time.Time
}

func NewLockout(trustedCIDRs []string) (*Lockout, error) {
	l := &Lockout{
		perIP:         map[string]*ipState{},
		globalBackoff: time.Minute,
	}
	for _, c := range trustedCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		l.trusted = append(l.trusted, n)
	}
	return l, nil
}

// ClientIP resolves the client bucket per pack: if RemoteAddr ∈ TRUSTED_PROXY_CIDR,
// strip trusted hops from the right of XFF and take the new rightmost; else RemoteAddr.
func (l *Lockout) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || !l.isTrusted(remote) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	// Strip trusted hops from the right.
	for len(parts) > 0 {
		p := strings.TrimSpace(parts[len(parts)-1])
		ip := net.ParseIP(p)
		if ip == nil || !l.isTrusted(ip) {
			return p
		}
		parts = parts[:len(parts)-1]
	}
	return host
}

func (l *Lockout) isTrusted(ip net.IP) bool {
	for _, n := range l.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Allow returns false if locked (password must not be checked).
func (l *Lockout) Allow(ip string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Before(l.globalUntil) {
		return false, l.globalUntil.Sub(now)
	}
	st := l.perIP[ip]
	if st == nil {
		return true, 0
	}
	// Reset after 1 h clean.
	if st.failures > 0 && now.Sub(st.lastFail) >= time.Hour && now.After(st.lockedUntil) {
		delete(l.perIP, ip)
		return true, 0
	}
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	return true, 0
}

func (l *Lockout) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.perIP, ip)
}

// Failure records a bad password / TOTP / recovery attempt. Returns backoff if locked.
func (l *Lockout) Failure(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	// Global window: >20 failures in 15 min → backoff starting 1 min, doubling to 15.
	cutoff := now.Add(-15 * time.Minute)
	trimmed := l.global[:0]
	for _, t := range l.global {
		if t.After(cutoff) {
			trimmed = append(trimmed, t)
		}
	}
	l.global = append(trimmed, now)
	// Decay global backoff after a clean hour without trips.
	if !l.globalLastTrip.IsZero() && now.Sub(l.globalLastTrip) >= globalDecayAfter {
		l.globalBackoff = time.Minute
		l.globalLastTrip = time.Time{}
	}
	if len(l.global) > 20 {
		if l.globalBackoff < time.Minute {
			l.globalBackoff = time.Minute
		}
		l.globalUntil = now.Add(l.globalBackoff)
		l.globalLastTrip = now
		d := l.globalBackoff
		if l.globalBackoff < 15*time.Minute {
			l.globalBackoff *= 2
			if l.globalBackoff > 15*time.Minute {
				l.globalBackoff = 15 * time.Minute
			}
		}
		st := l.ensureIP(ip, now)
		st.failures++
		st.lastFail = now
		return d
	}

	st := l.ensureIP(ip, now)
	st.failures++
	st.lastFail = now
	// 5 free failures; lockout starts on the 6th (2^(n-5) s).
	if st.failures <= 5 {
		return 0
	}
	// 2^(n-5) s capped at 15 min
	shift := st.failures - 5
	if shift > 10 {
		shift = 10
	}
	d := time.Second << uint(shift)
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	st.lockedUntil = now.Add(d)
	return d
}

func (l *Lockout) ensureIP(ip string, now time.Time) *ipState {
	st := l.perIP[ip]
	if st == nil {
		if len(l.perIP) >= maxTrackedIPs {
			// Evict a stale unlocked entry to bound memory.
			for k, v := range l.perIP {
				if now.After(v.lockedUntil) && (v.failures == 0 || now.Sub(v.lastFail) >= time.Hour) {
					delete(l.perIP, k)
					break
				}
			}
			// Still full: drop an arbitrary unlocked key.
			if len(l.perIP) >= maxTrackedIPs {
				for k, v := range l.perIP {
					if now.After(v.lockedUntil) {
						delete(l.perIP, k)
						break
					}
				}
			}
		}
		st = &ipState{}
		l.perIP[ip] = st
	}
	if st.failures > 0 && now.Sub(st.lastFail) >= time.Hour && now.After(st.lockedUntil) {
		st.failures = 0
		st.lockedUntil = time.Time{}
	}
	return st
}

// Reset clears all lockout state (break-glass).
func (l *Lockout) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perIP = map[string]*ipState{}
	l.global = nil
	l.globalUntil = time.Time{}
	l.globalBackoff = time.Minute
}
