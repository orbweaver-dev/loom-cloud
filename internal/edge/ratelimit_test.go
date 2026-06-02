package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenBucket_AllowsWithinBurst(t *testing.T) {
	l := NewTokenBucketLimiter(1, 3) // 1/s, burst 3
	fixed := time.Unix(0, 0)
	l.now = func() time.Time { return fixed }

	// First 3 succeed (full bucket), 4th fails (no refill at t=0).
	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	assert.False(t, l.Allow("acme"))
}

func TestTokenBucket_RefillsOverTime(t *testing.T) {
	l := NewTokenBucketLimiter(2, 2) // 2/s, burst 2
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	assert.False(t, l.Allow("acme"), "burst exhausted")

	// Advance 1s → +2 tokens (capped at burst 2).
	now = now.Add(time.Second)
	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	assert.False(t, l.Allow("acme"))
}

func TestTokenBucket_RefillCappedAtBurst(t *testing.T) {
	l := NewTokenBucketLimiter(5, 2)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	// Idle a long time — refill must cap at Burst, not accumulate.
	now = now.Add(time.Hour)
	assert.True(t, l.Allow("acme"))
	assert.True(t, l.Allow("acme"))
	assert.False(t, l.Allow("acme"), "capped at burst=2 despite long idle")
}

func TestTokenBucket_PerSlugIsolation(t *testing.T) {
	l := NewTokenBucketLimiter(1, 1)
	fixed := time.Unix(0, 0)
	l.now = func() time.Time { return fixed }

	assert.True(t, l.Allow("acme"))
	assert.False(t, l.Allow("acme"))  // acme exhausted
	assert.True(t, l.Allow("globex")) // globex has its own bucket
}

func TestRouter_RateLimited429(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	pm := NewMemoryPortMap()
	pm.Set("acme", 1)
	limiter := NewTokenBucketLimiter(1, 1) // burst 1
	fixed := time.Unix(0, 0)
	limiter.now = func() time.Time { return fixed }

	r := &Router{
		PortMap:     pm,
		RateLimiter: limiter,
		Backend:     func(int) string { return backend.URL },
	}
	h, err := r.Handler("loom.dev")
	require.NoError(t, err)

	req := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		rq := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/", nil)
		rq.Host = "acme.loom.dev"
		h.ServeHTTP(rec, rq)
		return rec
	}

	first := req()
	assert.Equal(t, http.StatusOK, first.Code)
	body, _ := io.ReadAll(first.Body)
	assert.Equal(t, "ok", string(body))

	second := req()
	assert.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Equal(t, "1", second.Header().Get("Retry-After"))
}

func TestRouter_NoLimiter_Unlimited(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	pm := NewMemoryPortMap()
	pm.Set("acme", 1)
	r := &Router{PortMap: pm, Backend: func(int) string { return backend.URL }}
	h, _ := r.Handler("loom.dev")
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		rq := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/", nil)
		rq.Host = "acme.loom.dev"
		h.ServeHTTP(rec, rq)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
}

// interface compliance
var _ RateLimiter = (*TokenBucketLimiter)(nil)
