package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/skpjr001/fluid-config/internal/config"
)

// ---------- client IP ----------

// ClientIP returns the caller's address. X-Forwarded-For is only believed
// when the direct peer is a trusted proxy, and it is walked from the right:
// the first address that is not itself a trusted proxy is the client. The
// left-most entries are attacker-controlled and are never used.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !config.PrefixesContain(trusted, peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer // malformed chain: fall back to the trusted peer
		}
		a = a.Unmap()
		if !config.PrefixesContain(trusted, a) {
			return a
		}
	}
	return peer
}

// ---------- rate limiting ----------

// RateLimiter is a token bucket per key (route + source IP).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{buckets: map[string]*bucket{}, now: time.Now}
	return rl
}

// Allow takes one token; when refused it returns how long until one is free.
func (rl *RateLimiter) Allow(key string, lim config.RateLimit) (bool, time.Duration) {
	if lim.RequestsPerMinute <= 0 {
		return true, 0
	}
	rate := float64(lim.RequestsPerMinute) / 60
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(lim.Burst), last: now}
		rl.buckets[key] = b
	}
	b.tokens = math.Min(float64(lim.Burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Sweep drops buckets idle for longer than idle (they would be full anyway).
func (rl *RateLimiter) Sweep(idle time.Duration) {
	cutoff := rl.now().Add(-idle)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if b.last.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}

// ---------- tokens ----------

func ctEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
