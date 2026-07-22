package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Valkey-backed implementations of the per-pod state stores (forge CSRF state,
// board-MCP run tokens, auth rate-limit buckets), so the server is horizontally
// scalable: any replica can serve any request. Selected at wiring time when a
// Redis/Valkey backend is configured; otherwise the in-memory impls are used.

// valkeyOpTimeout bounds a single Redis round-trip so a Valkey outage degrades
// (state-not-found / rate-limit-allow) instead of hanging the request.
const valkeyOpTimeout = 3 * time.Second

func valkeyCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), valkeyOpTimeout)
}

// --- forge CSRF state -------------------------------------------------------

const forgeStateKeyPrefix = "iterion:forge:state:"

type valkeyForgeStateStore struct {
	rdb redis.UniversalClient
	ttl time.Duration
}

func newValkeyForgeStateStore(rdb redis.UniversalClient, ttl time.Duration) *valkeyForgeStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &valkeyForgeStateStore{rdb: rdb, ttl: ttl}
}

func (s *valkeyForgeStateStore) put(p forgePending) error {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal forge oauth state: %w", err)
	}
	ctx, cancel := valkeyCtx()
	defer cancel()
	// Never log or wrap the state token itself — it authenticates the callback.
	if err := s.rdb.Set(ctx, forgeStateKeyPrefix+p.State, b, s.ttl).Err(); err != nil {
		return fmt.Errorf("persist forge oauth state (provider=%s, team=%s): %w", p.Provider, p.TenantID, err)
	}
	return nil
}

func (s *valkeyForgeStateStore) take(state string) (forgePending, bool) {
	ctx, cancel := valkeyCtx()
	defer cancel()
	// GETDEL is the atomic one-time consume (Redis ≥ 6.2 / Valkey). redis.Nil
	// (missing/expired) or any error → treat as not found.
	b, err := s.rdb.GetDel(ctx, forgeStateKeyPrefix+state).Bytes()
	if err != nil {
		return forgePending{}, false
	}
	var p forgePending
	if json.Unmarshal(b, &p) != nil {
		return forgePending{}, false
	}
	return p, true
}

// --- board-MCP run tokens ---------------------------------------------------

const boardTokenKeyPrefix = "iterion:board:tok:"

type valkeyBoardMCPTokenStore struct {
	rdb    redis.UniversalClient
	logger *iterlog.Logger
}

func newValkeyBoardMCPTokenStore(rdb redis.UniversalClient, logger *iterlog.Logger) *valkeyBoardMCPTokenStore {
	return &valkeyBoardMCPTokenStore{rdb: rdb, logger: logger}
}

// boardTokenPayload is the stored grant. It replaced a bare `[]string` of
// capabilities when the owning ticket had to ride along (so board.create
// over the HTTP transport can auto-stamp parent_id); lookup still accepts
// the legacy array shape written by an older replica mid-rollout.
type boardTokenPayload struct {
	Caps          []string `json:"caps"`
	SourceIssueID string   `json:"src,omitempty"`
}

func (s *valkeyBoardMCPTokenStore) Register(token string, caps []string, sourceIssueID string) error {
	b, err := json.Marshal(boardTokenPayload{Caps: caps, SourceIssueID: strings.TrimSpace(sourceIssueID)})
	if err != nil {
		return fmt.Errorf("marshal board MCP caps: %w", err)
	}
	ctx, cancel := valkeyCtx()
	defer cancel()
	// Redis TTL replaces the in-memory sweep; the run's lifetime cap.
	// Caps are safe to include in the error; the token is not.
	if err := s.rdb.Set(ctx, boardTokenKeyPrefix+token, b, boardMCPDefaultTTL).Err(); err != nil {
		return fmt.Errorf("store board MCP token (caps=%v): %w", caps, err)
	}
	return nil
}

// Revoke is best-effort: it has no production caller today and the Redis
// key's boardMCPDefaultTTL is the backstop that reaps a missed delete, so a
// failure is logged (token prefix only) rather than propagated.
func (s *valkeyBoardMCPTokenStore) Revoke(token string) {
	ctx, cancel := valkeyCtx()
	defer cancel()
	if err := s.rdb.Del(ctx, boardTokenKeyPrefix+token).Err(); err != nil && s.logger != nil {
		prefix := token
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		s.logger.Warn("board MCP: revoke token %s…: %v (best-effort — Redis TTL %s is the backstop)", prefix, err, boardMCPDefaultTTL)
	}
}

func (s *valkeyBoardMCPTokenStore) lookup(token string) (boardMCPGrant, bool) {
	ctx, cancel := valkeyCtx()
	defer cancel()
	b, err := s.rdb.Get(ctx, boardTokenKeyPrefix+token).Bytes()
	if err != nil {
		return boardMCPGrant{}, false
	}
	var payload boardTokenPayload
	if json.Unmarshal(b, &payload) != nil {
		// Legacy shape: a bare capability array (pre-parent_id rollout).
		var caps []string
		if json.Unmarshal(b, &caps) != nil {
			return boardMCPGrant{}, false
		}
		payload.Caps = caps
	}
	grant := boardMCPGrant{
		Capabilities:  boardops.Capabilities{},
		SourceIssueID: payload.SourceIssueID,
	}
	for _, c := range payload.Caps {
		grant.Capabilities[c] = true
	}
	// ExpiresAt left zero: Redis already evicted the key if it lapsed, and the
	// handler's IsZero() check then skips the (redundant) local TTL recheck.
	return grant, true
}

// --- forge-publish run tokens ------------------------------------------------

const forgePublishTokenKeyPrefix = "iterion:forgepub:tok:"

// valkeyForgePublishTokenStore shares per-run forge-publish grants across
// server replicas (the run's POST is load-balanced to any pod while the
// grant was minted at launch time elsewhere).
type valkeyForgePublishTokenStore struct {
	rdb    redis.UniversalClient
	logger *iterlog.Logger
}

func newValkeyForgePublishTokenStore(rdb redis.UniversalClient, logger *iterlog.Logger) *valkeyForgePublishTokenStore {
	return &valkeyForgePublishTokenStore{rdb: rdb, logger: logger}
}

func (s *valkeyForgePublishTokenStore) Register(token string, g ForgePublishGrant) error {
	b, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshal forge publish grant: %w", err)
	}
	ctx, cancel := valkeyCtx()
	defer cancel()
	// Redis TTL replaces the in-memory sweep. The grant is safe to name in
	// the error (team/conn/repo); the token is not.
	if err := s.rdb.Set(ctx, forgePublishTokenKeyPrefix+token, b, forgePublishDefaultTTL).Err(); err != nil {
		return fmt.Errorf("store forge publish token (repo=%s): %w", g.Repo, err)
	}
	return nil
}

// Revoke is best-effort: the Redis TTL is the backstop that reaps a missed
// delete, so a failure is logged (token prefix only) rather than propagated.
func (s *valkeyForgePublishTokenStore) Revoke(token string) {
	ctx, cancel := valkeyCtx()
	defer cancel()
	if err := s.rdb.Del(ctx, forgePublishTokenKeyPrefix+token).Err(); err != nil && s.logger != nil {
		prefix := token
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		s.logger.Warn("forge publish: revoke token %s…: %v (best-effort — Redis TTL %s is the backstop)", prefix, err, forgePublishDefaultTTL)
	}
}

func (s *valkeyForgePublishTokenStore) lookup(token string) (ForgePublishGrant, bool) {
	ctx, cancel := valkeyCtx()
	defer cancel()
	b, err := s.rdb.Get(ctx, forgePublishTokenKeyPrefix+token).Bytes()
	if err != nil {
		return ForgePublishGrant{}, false
	}
	var g ForgePublishGrant
	if json.Unmarshal(b, &g) != nil {
		return ForgePublishGrant{}, false
	}
	// ExpiresAt left zero: Redis already evicted the key if it lapsed.
	return g, true
}

// --- auth rate limit (atomic token bucket) ----------------------------------

const rateLimitKeyPrefix = "iterion:rl:"

// rateLimitScript is an atomic token-bucket: refill by elapsed*rate (capped at
// burst), take one token if available, persist {tokens,last} with a TTL, and
// return {allowed, retry_ms}. Atomic across replicas (single EVAL).
var rateLimitScript = redis.NewScript(`
local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local ttl   = tonumber(ARGV[4])
local d = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(d[1])
local last   = tonumber(d[2])
if tokens == nil then tokens = burst; last = now end
local elapsed = (now - last) / 1000.0
if elapsed > 0 then
  tokens = math.min(burst, tokens + elapsed * rate)
  last = now
end
local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate * 1000) + 100
end
redis.call('HSET', key, 'tokens', tokens, 'last', last)
redis.call('PEXPIRE', key, ttl)
return {allowed, retry}
`)

type valkeyAuthRateLimiter struct {
	rdb redis.UniversalClient
	now func() time.Time
}

func newValkeyAuthRateLimiter(rdb redis.UniversalClient) *valkeyAuthRateLimiter {
	return &valkeyAuthRateLimiter{rdb: rdb, now: time.Now}
}

func (r *valkeyAuthRateLimiter) allow(key string, cfg authBucketCfg) (bool, time.Duration) {
	if cfg.rate <= 0 {
		return true, 0
	}
	ctx, cancel := valkeyCtx()
	defer cancel()
	// Key TTL = time to refill a full bucket + 1s margin, so idle keys self-evict.
	ttlMs := int64(cfg.burst/cfg.rate*1000) + 1000
	res, err := rateLimitScript.Run(ctx, r.rdb, []string{rateLimitKeyPrefix + key},
		cfg.rate, cfg.burst, r.now().UnixMilli(), ttlMs).Result()
	if err != nil {
		// Fail-open: a Valkey blip must not lock everyone out of login.
		return true, 0
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return true, 0
	}
	allowed, _ := arr[0].(int64)
	retryMs, _ := arr[1].(int64)
	if allowed == 1 {
		return true, 0
	}
	return false, time.Duration(retryMs) * time.Millisecond
}
