package server

import (
	"bytes"
	"container/list"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// peekJSONField reads a single string field from a JSON request body
// without consuming it for downstream handlers. Used by the auth
// rate-limiter to extract `email` for the per-account tier without
// breaking the standard json.Unmarshal in handleLogin.
//
// Cheap: reads at most ~4 KB; restores the body before returning.
// Returns "" on any parse failure — the downstream handler will
// surface the real 400 / 401 to the caller.
func peekJSONField(r *http.Request, field string) string {
	if r.Body == nil {
		return ""
	}
	const limit = 4 << 10
	buf, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return ""
	}
	// Restore the body so the downstream handler reads the same bytes.
	r.Body = io.NopCloser(bytes.NewReader(buf))
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// authRateLimiterBackend is the token-bucket rate limiter. The in-memory impl
// (authRateLimiter) is per-pod (with replicas the effective limit is N× — an
// acceptable soft defense); the Valkey impl (valkey_stores.go) makes it exact
// across replicas via an atomic Lua token-bucket.
type authRateLimiterBackend interface {
	allow(key string, cfg authBucketCfg) (bool, time.Duration)
}

// authRateLimiter enforces a per-key token-bucket rate limit on the
// `/api/auth/*` endpoints. Buckets are keyed by client IP (login,
// register, refresh) and additionally by email for login (defence
// against distributed brute-force against one account).
//
// In-process impl: the local-mode studio doesn't have Redis; the LRU cap
// bounds memory under any abuse pattern. (F-C1)
type authRateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*authBucket
	order     *list.List
	maxKeys   int
	now       func() time.Time
	defaultBy authBucketCfg
}

type authBucket struct {
	tokens   float64
	last     time.Time
	cfg      authBucketCfg
	listElem *list.Element
}

type authBucketCfg struct {
	rate  float64 // tokens added per second
	burst float64 // max tokens
}

// newAuthRateLimiter builds the limiter with sensible defaults. The
// maxKeys cap bounds the bucket map under any IP-cycling attack.
func newAuthRateLimiter() *authRateLimiter {
	return &authRateLimiter{
		buckets:   make(map[string]*authBucket),
		order:     list.New(),
		maxKeys:   10000,
		now:       time.Now,
		defaultBy: authBucketCfg{rate: 1.0 / 6.0, burst: 10}, // 10/min sustained, burst 10
	}
}

// allow decrements one token from the named bucket if available.
// Returns (true, 0) on success, (false, retryAfter) when throttled.
func (r *authRateLimiter) allow(key string, cfg authBucketCfg) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		// Insert; evict oldest if cap exceeded.
		b = &authBucket{tokens: cfg.burst, last: now, cfg: cfg}
		b.listElem = r.order.PushFront(key)
		r.buckets[key] = b
		for len(r.buckets) > r.maxKeys {
			oldest := r.order.Back()
			if oldest == nil {
				break
			}
			delete(r.buckets, oldest.Value.(string))
			r.order.Remove(oldest)
		}
	} else {
		// Refill since last check.
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * cfg.rate
			if b.tokens > cfg.burst {
				b.tokens = cfg.burst
			}
		}
		b.last = now
		// Move to front (MRU).
		r.order.MoveToFront(b.listElem)
	}
	if b.tokens < 1 {
		// Compute retry-after in seconds-to-1-token at rate.
		need := 1 - b.tokens
		retry := time.Duration(need/cfg.rate*float64(time.Second)) + 100*time.Millisecond
		return false, retry
	}
	b.tokens--
	return true, 0
}

// limitRoute wraps an HTTP handler with per-IP rate limiting. The
// `keyExtra` arg adds a per-route tier (e.g. login also rate-limits
// by email so distributed attempts against one account get caught).
func (s *Server) limitRoute(cfg authBucketCfg, keyExtra func(*http.Request) string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.authLimiter == nil {
				next(w, r)
				return
			}
			keys := []string{s.clientIP(r)}
			if keyExtra != nil {
				if extra := keyExtra(r); extra != "" {
					keys = append(keys, extra)
				}
			}
			for _, k := range keys {
				if ok, retry := s.authLimiter.allow(k, cfg); !ok {
					w.Header().Set("Retry-After", retrySeconds(retry))
					httpError(w, http.StatusTooManyRequests, "rate limit exceeded; retry in %s", retry.Truncate(time.Second))
					if s.logger != nil {
						s.logger.Info("auth: rate-limit hit on key %q (retry in %s)", k, retry.Truncate(time.Second))
					}
					return
				}
			}
			next(w, r)
		}
	}
}

func retrySeconds(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return itoa(secs)
}

// itoa avoids importing strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
