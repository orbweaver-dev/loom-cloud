package edge

import (
	"sync"
	"time"
)

// Per-tenant edge rate limiting. The Router resolves a slug per
// request; when a RateLimiter is configured it gates each request
// on that slug's budget, returning 429 when a tenant exceeds its
// allowance. This is the "per-tenant edge logic" hook the Router's
// SubdomainResolver comment anticipated — it protects backends from
// a single noisy tenant without coupling to the tenant's own app.

// RateLimiter decides whether a request for a given slug may
// proceed. Implementations must be safe for concurrent calls.
type RateLimiter interface {
	// Allow reports whether a request for slug is within budget,
	// consuming one unit when it is.
	Allow(slug string) bool
}

// TokenBucketLimiter is a per-slug token bucket. Each slug gets its
// own bucket of capacity Burst that refills at RatePerSec tokens
// per second; a request consumes one token and is allowed only
// when a token is available.
type TokenBucketLimiter struct {
	// RatePerSec is the steady-state allowance (tokens added per
	// second). Must be > 0.
	RatePerSec float64
	// Burst is the bucket capacity — the most requests a freshly
	// idle tenant can make at once. Must be >= 1.
	Burst float64

	// now is the clock; nil → time.Now. Injected in tests for
	// deterministic refill behaviour.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewTokenBucketLimiter builds a limiter with the given steady-state
// rate and burst capacity.
func NewTokenBucketLimiter(ratePerSec, burst float64) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		RatePerSec: ratePerSec,
		Burst:      burst,
		buckets:    map[string]*tokenBucket{},
	}
}

func (l *TokenBucketLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Allow implements RateLimiter.
func (l *TokenBucketLimiter) Allow(slug string) bool {
	now := l.clock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*tokenBucket{}
	}
	b := l.buckets[slug]
	if b == nil {
		// New tenant starts with a full bucket.
		b = &tokenBucket{tokens: l.Burst, last: now}
		l.buckets[slug] = b
	}

	// Refill based on elapsed time, capped at Burst.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.RatePerSec
		if b.tokens > l.Burst {
			b.tokens = l.Burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
