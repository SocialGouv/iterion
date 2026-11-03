package server

import (
	"bytes"
	"container/list"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
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
	// refund gives one token back to the named bucket, capped at burst —
	// the undo of an allow a caller did not keep (#1726). A key the limiter
	// no longer holds (evicted under the LRU cap, or the Valkey key
	// expired) refunds nothing: recreating a bucket to hand it a token
	// would grant a fresh budget to a key whose history is gone.
	refund(key string, cfg authBucketCfg)
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

// refund returns one token to the named bucket, capped at burst. An
// unknown key refunds nothing (see the interface's contract).
func (r *authRateLimiter) refund(key string, cfg authBucketCfg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		return
	}
	now := r.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * cfg.rate
		if b.tokens > cfg.burst {
			b.tokens = cfg.burst
		}
	}
	b.last = now
	if b.tokens < cfg.burst {
		b.tokens++
	}
}

// rateBucketIPv6PrefixBits is how much of an IPv6 identity the limiter
// charges. The /64 is the smallest prefix an address plan hands to a
// single site — RFC 6177 and every residential, mobile and VPS
// allocation — so it is the smallest unit one client cannot multiply.
// Narrowing it hands the client its buckets back; widening it makes
// neighbours pay for each other.
const rateBucketIPv6PrefixBits = 64

// rateBucketIP is the bucket an identity is charged to: the identity
// itself for IPv4 and for anything that is not an address, its /64 for
// IPv6.
//
// An IPv6 client is not one address. A residential or VPS client holds a
// whole /64, and RFC 4941 privacy extensions rotate its source address
// without it asking, so a /128 key is a bucket the client replaces for
// free — register (2/min), refresh (30/min), the password-reset confirm
// whose body names no account, the desktop exchange and the config-share
// probe guard are all keyed on the IP alone, and all of them are
// unlimited for anybody holding a prefix. Ten thousand addresses out of
// one /64 also evict every other client's bucket from the limiter's LRU
// (maxKeys). The /64 is the unit the client cannot multiply.
//
// The AUDIT row keeps the full address (clientIP): a bucket groups
// clients on purpose, a row names the one client that acted.
func rateBucketIP(identity string) string {
	ip := net.ParseIP(identity)
	if ip == nil || ip.To4() != nil {
		// Not an address at all — a unix socket, the unknown-peer
		// sentinel; a zone-scoped peer lost its zone in addrHost and is
		// an address — or IPv4, whose single address is already a single
		// client: both are their own bucket.
		return identity
	}
	mask := net.CIDRMask(rateBucketIPv6PrefixBits, 8*net.IPv6len)
	return (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String()
}

// rateBucket is the ONE way a request becomes a rate-limit key. Every
// limiter call that keys on the client goes through it — limitRoute's
// tiers and the config-share per-IP guard — so a new site cannot forget
// the prefix the way it would forget a helper called at two places out
// of three.
func (s *Server) rateBucket(r *http.Request) string {
	return rateBucketIP(s.clientIP(r))
}

// limitRoute wraps an HTTP handler with per-IP rate limiting. `tier`
// namespaces the bucket, as every other caller of the limiter does
// ("wh:", "csip:", "orglaunch:"): the client IP alone would make the
// login, register, refresh and password-proof wrappers draw from ONE
// bucket per client, and allow() clamps a bucket's tokens to the burst of
// whichever route touched it last — so hammering the public refresh route
// would lock every client behind that address out of login. The
// `keyExtra` arg adds a second, already-namespaced tier (login also
// rate-limits by email so distributed attempts against one account get
// caught).
func (s *Server) limitRoute(tier string, cfg authBucketCfg, keyExtra func(*http.Request) string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.authLimiter == nil {
				next(w, r)
				return
			}
			keys := []string{tier + ":" + s.rateBucket(r)}
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
	return strconv.Itoa(secs)
}
