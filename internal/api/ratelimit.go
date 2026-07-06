package api

// Per-principal token-bucket rate limiting (spec §13 error model: 429).

import (
	"net/http"
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	// perMin is the sustained request budget per principal per minute;
	// burst is the bucket capacity.
	perMin float64
	burst  float64
}

func newRateLimiter(perMin int) *rateLimiter {
	if perMin <= 0 {
		return nil // disabled
	}
	return &rateLimiter{
		buckets: map[string]*bucket{},
		perMin:  float64(perMin),
		burst:   float64(perMin), // one minute of burst headroom
	}
}

// allow refills lazily and consumes one token; false means 429.
func (rl *rateLimiter) allow(principal string) bool {
	if rl == nil {
		return true
	}
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[principal]
	if !ok {
		b = &bucket{tokens: rl.burst, lastSeen: now}
		rl.buckets[principal] = b
	}
	b.tokens += now.Sub(b.lastSeen).Minutes() * rl.perMin
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	// Opportunistic cleanup of idle buckets.
	if len(rl.buckets) > 10_000 {
		for k, v := range rl.buckets {
			if now.Sub(v.lastSeen) > 10*time.Minute {
				delete(rl.buckets, k)
			}
		}
	}
	return true
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r)
		if !s.limiter.allow(p.TenantID + "/" + p.ID) {
			s.Metrics.RateLimited.Add(1)
			w.Header().Set("Retry-After", "10")
			problem(w, http.StatusTooManyRequests, "rate limited",
				"per-principal request budget exceeded; retry shortly")
			return
		}
		next.ServeHTTP(w, r)
	})
}
