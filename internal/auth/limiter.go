package auth

import (
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Limiter throttles logins: exponential backoff per username (no hard lockout)
// plus a token bucket per client IP. State is in memory and resets on restart.
type Limiter struct {
	mu    sync.Mutex
	users map[string]*userState
	ips   map[string]*bucket
	sem   chan struct{} // caps concurrent Argon2 verifications
	now   func() time.Time
}

type userState struct {
	failures int
	last     time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	freeFailures = 3
	maxBackoff   = 15 * time.Minute
	ipRate       = 20.0 / 60.0 // tokens per second
	ipBurst      = 20.0
)

func NewLimiter() *Limiter {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	return &Limiter{
		users: map[string]*userState{},
		ips:   map[string]*bucket{},
		sem:   make(chan struct{}, n),
		now:   time.Now,
	}
}

// Allow reports whether a login attempt may proceed, and if not, how long to wait.
func (l *Limiter) Allow(username, ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if u := l.users[strings.ToLower(username)]; u != nil && u.failures >= freeFailures {
		d := backoff(u.failures)
		if wait := u.last.Add(d).Sub(now); wait > 0 {
			return false, wait
		}
	}
	b := l.ips[ip]
	if b == nil {
		b = &bucket{tokens: ipBurst, last: now}
		l.ips[ip] = b
	}
	b.tokens = math.Min(ipBurst, b.tokens+now.Sub(b.last).Seconds()*ipRate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1-b.tokens)/ipRate*float64(time.Second)) + time.Second
	}
	b.tokens--
	return true, 0
}

func backoff(failures int) time.Duration {
	d := time.Second << min(failures-freeFailures, 20)
	return min(d, maxBackoff)
}

func (l *Limiter) Fail(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := strings.ToLower(username)
	u := l.users[k]
	if u == nil {
		u = &userState{}
		l.users[k] = u
	}
	u.failures++
	u.last = l.now()
}

func (l *Limiter) Success(username string) {
	l.mu.Lock()
	delete(l.users, strings.ToLower(username))
	l.mu.Unlock()
}

// Acquire reserves an Argon2 slot; returns false if all slots are busy.
func (l *Limiter) Acquire() bool {
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *Limiter) Release() { <-l.sem }

// Sweep drops stale entries; call periodically.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, u := range l.users {
		if now.Sub(u.last) > maxBackoff*2 {
			delete(l.users, k)
		}
	}
	for k, b := range l.ips {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.ips, k)
		}
	}
}
