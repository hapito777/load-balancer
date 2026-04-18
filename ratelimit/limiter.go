package ratelimit

import (
	"sync"
	"time"
)

// bucket is a token bucket for a single IP.
type bucket struct {
	tokens   float64
	lastSeen time.Time
	mu       sync.Mutex
}

// Limiter implements per-IP token-bucket rate limiting.
type Limiter struct {
	rps     float64
	burst   float64
	mu      sync.Mutex
	buckets map[string]*bucket
	stop    chan struct{}
}

// New creates a Limiter with the given requests-per-second and burst capacity.
func New(rps float64, burst int) *Limiter {
	l := &Limiter{
		rps:     rps,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Allow returns true if the given IP is within the rate limit.
func (l *Limiter) Allow(ip string) bool {
	l.mu.Lock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: time.Now()}
		l.buckets[ip] = b
	}
	l.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now

	// Refill tokens based on elapsed time.
	b.tokens += elapsed * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Stop shuts down the background cleanup goroutine.
func (l *Limiter) Stop() {
	close(l.stop)
}

// cleanup removes stale bucket entries every minute to prevent unbounded growth.
func (l *Limiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-5 * time.Minute)
			l.mu.Lock()
			for ip, b := range l.buckets {
				b.mu.Lock()
				stale := b.lastSeen.Before(cutoff)
				b.mu.Unlock()
				if stale {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}
