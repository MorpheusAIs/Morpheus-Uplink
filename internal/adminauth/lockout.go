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
	bearerIP       map[string]*ipState // admin Bearer failures: per IP only, no global trip
	global         []time.Time         // recent failure timestamps
	globalUntil    time.Time
	globalBackoff  time.Duration
	globalLastTrip time.Time

	trusted []*net.IPNet
	now     func() time.Time
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
		bearerIP:      map[string]*ipState{},
		globalBackoff: time.Minute,
		now:           time.Now,
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
	now := l.now()
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
	now := l.now()

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
	return ensureIn(l.perIP, ip, now)
}

func ensureIn(m map[string]*ipState, ip string, now time.Time) *ipState {
	st := m[ip]
	if st == nil {
		if len(m) >= maxTrackedIPs {
			evictOne(m, now)
		}
		st = &ipState{}
		m[ip] = st
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
	l.bearerIP = map[string]*ipState{}
	l.global = nil
	l.globalUntil = time.Time{}
	l.globalBackoff = time.Minute
}

// AllowBearer is the admin Bearer branch's lockout check: per client
// address only. It never reads the global trip, so a password-login flood
// cannot lock out the master Bearer from other addresses.
func (l *Lockout) AllowBearer(ip string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.bearerIP[ip]
	if st == nil {
		return true, 0
	}
	if st.failures > 0 && now.Sub(st.lastFail) >= time.Hour && now.After(st.lockedUntil) {
		delete(l.bearerIP, ip)
		return true, 0
	}
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	return true, 0
}

// FailureBearer records a rejected admin Bearer for one address (same
// per-IP schedule as Failure: 5 free, then 2^(n-5) s up to 15 min). It
// never feeds the global window.
func (l *Lockout) FailureBearer(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := ensureIn(l.bearerIP, ip, now)
	st.failures++
	st.lastFail = now
	if st.failures <= 5 {
		return 0
	}
	shift := min(st.failures-5, 10)
	d := min(time.Second<<uint(shift), 15*time.Minute)
	st.lockedUntil = now.Add(d)
	return d
}

// SuccessBearer clears an address's Bearer failures.
func (l *Lockout) SuccessBearer(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.bearerIP, ip)
}

// GlobalLocked reports whether the global password-login trip is active.
func (l *Lockout) GlobalLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now().Before(l.globalUntil)
}

// evictOne drops one unlocked entry to bound memory: a stale one among the
// first few looked at, else the first unlocked one. Locked entries are
// kept. Bounded work per call, so a flood of new addresses stays cheap.
func evictOne(m map[string]*ipState, now time.Time) {
	const look = 64
	var unlocked string
	have := false
	i := 0
	for k, v := range m {
		if now.After(v.lockedUntil) {
			if v.failures == 0 || now.Sub(v.lastFail) >= time.Hour {
				delete(m, k)
				return
			}
			if !have {
				unlocked, have = k, true
			}
		}
		if i++; i >= look && have {
			break
		}
	}
	if have {
		delete(m, unlocked)
	}
}

// SetClock replaces the clock (tests).
func (l *Lockout) SetClock(now func() time.Time) {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}

// TrackedBearerIPs is the size of the Bearer failure map (tests).
func (l *Lockout) TrackedBearerIPs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bearerIP)
}
