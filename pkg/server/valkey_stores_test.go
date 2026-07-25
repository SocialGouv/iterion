package server

import (
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
	s.put(p)

	got, ok := s.take("abc")
	if !ok || got.State != "abc" || got.UserID != "u1" {
		t.Fatalf("take = %+v, ok=%v", got, ok)
	}
	// One-time consume: the second take misses.
	if _, ok := s.take("abc"); ok {
		t.Errorf("second take should miss (GETDEL consumed it)")
	}
	// TTL: a fresh entry expires after the window.
	s.put(forgePending{State: "ttl", IssuedAt: time.Now()})
	mr.FastForward(11 * time.Minute)
	if _, ok := s.take("ttl"); ok {
		t.Errorf("entry should have expired via TTL")
	}
	// Unknown key misses cleanly.
	if _, ok := s.take("nope"); ok {
		t.Errorf("unknown key should miss")
	}
}

func TestValkeyBoardMCPTokenStore_RegisterLookupRevoke(t *testing.T) {
	mr, rdb := newTestRedis(t)
	s := newValkeyBoardMCPTokenStore(rdb)

	s.Register("tok1", []string{"board.read", "board.move"}, "")
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
	s.Register("tok2", []string{"board.read"}, "")
	mr.FastForward(boardMCPDefaultTTL + time.Minute)
	if _, ok := s.lookup("tok2"); ok {
		t.Errorf("token should have expired via TTL")
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
