package server

import (
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestValkeyForgeStateStore_TakeOnceAndTTL(t *testing.T) {
	mr, rdb := newTestRedis(t)
	s := newValkeyForgeStateStore(rdb, 10*time.Minute)

	p := forgePending{State: "abc", Provider: "github", TenantID: "t1", UserID: "u1", IssuedAt: time.Now()}
	if err := s.put(p); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok := s.take("abc")
	if !ok || got.State != "abc" || got.UserID != "u1" {
		t.Fatalf("take = %+v, ok=%v", got, ok)
	}
	// One-time consume: the second take misses.
	if _, ok := s.take("abc"); ok {
		t.Errorf("second take should miss (GETDEL consumed it)")
	}
	// TTL: a fresh entry expires after the window.
	if err := s.put(forgePending{State: "ttl", IssuedAt: time.Now()}); err != nil {
		t.Fatalf("put: %v", err)
	}
	mr.FastForward(11 * time.Minute)
	if _, ok := s.take("ttl"); ok {
		t.Errorf("entry should have expired via TTL")
	}
	// Unknown key misses cleanly.
	if _, ok := s.take("nope"); ok {
		t.Errorf("unknown key should miss")
	}
}

// A Valkey write failure must surface from put — silently dropping the state
// would doom the OAuth callback with an unexplained "state expired or invalid".
func TestValkeyForgeStateStore_PutSurfacesSetFailure(t *testing.T) {
	mr, rdb := newTestRedis(t)
	s := newValkeyForgeStateStore(rdb, 10*time.Minute)

	mr.SetError("valkey down")
	err := s.put(forgePending{State: "boom", Provider: "github", TenantID: "t1", IssuedAt: time.Now()})
	if err == nil {
		t.Fatal("put should surface the Set failure")
	}
	// The error must carry context but NEVER the state token.
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("error must not leak the state token: %v", err)
	}
	if !strings.Contains(err.Error(), "provider=github") || !strings.Contains(err.Error(), "team=t1") {
		t.Errorf("error should carry provider+team context: %v", err)
	}
}

func TestValkeyBoardMCPTokenStore_RegisterLookupRevoke(t *testing.T) {
	mr, rdb := newTestRedis(t)
	s := newValkeyBoardMCPTokenStore(rdb, nil)

	if err := s.Register("tok1", []string{"board.read", "board.move"}, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	g, ok := s.lookup("tok1")
	if !ok || !g.Capabilities["board.read"] || !g.Capabilities["board.move"] {
		t.Fatalf("lookup = %+v ok=%v", g.Capabilities, ok)
	}
	if g.Capabilities["board.create"] {
		t.Errorf("ungranted cap leaked")
	}
	s.Revoke("tok1")
	if _, ok := s.lookup("tok1"); ok {
		t.Errorf("revoked token should miss")
	}
	// TTL eviction.
	if err := s.Register("tok2", []string{"board.read"}, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.FastForward(boardMCPDefaultTTL + time.Minute)
	if _, ok := s.lookup("tok2"); ok {
		t.Errorf("token should have expired via TTL")
	}
}

// A Valkey write failure must surface from Register — otherwise the minter
// hands the run a token whose every CallTool 401s with no explanation.
func TestValkeyBoardMCPTokenStore_RegisterSurfacesSetFailure(t *testing.T) {
	mr, rdb := newTestRedis(t)
	s := newValkeyBoardMCPTokenStore(rdb, nil)

	mr.SetError("valkey down")
	err := s.Register("secret-token", []string{"board.read"}, "")
	if err == nil {
		t.Fatal("Register should surface the Set failure")
	}
	// Caps are safe in the error; the token is not.
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error must not leak the token: %v", err)
	}
	if !strings.Contains(err.Error(), "board.read") {
		t.Errorf("error should carry the caps context: %v", err)
	}
}

func TestValkeyAuthRateLimiter_AllowDenyRefill(t *testing.T) {
	_, rdb := newTestRedis(t)
	rl := newValkeyAuthRateLimiter(rdb)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return base }

	cfg := authBucketCfg{rate: 1.0, burst: 3} // 3 burst, 1 token/sec

	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("ip", cfg); !ok {
			t.Fatalf("request %d should be allowed (within burst)", i)
		}
	}
	// Bucket empty → throttled with a positive retry.
	ok, retry := rl.allow("ip", cfg)
	if ok || retry <= 0 {
		t.Fatalf("expected throttle, got ok=%v retry=%v", ok, retry)
	}
	// After 2s, ~2 tokens refilled → allowed again.
	rl.now = func() time.Time { return base.Add(2 * time.Second) }
	if ok, _ := rl.allow("ip", cfg); !ok {
		t.Errorf("should be allowed after refill")
	}
	// A different key has its own bucket.
	if ok, _ := rl.allow("other", cfg); !ok {
		t.Errorf("independent key should be allowed")
	}
}
