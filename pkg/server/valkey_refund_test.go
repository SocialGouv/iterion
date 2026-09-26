package server

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestValkeyRefundRestoresAToken pins the Valkey half of #1726 — the impl
// the cloud deployment actually runs: a consumed per-minute token comes
// back across replicas, an emptied bucket refills to exactly one
// allowance, and an expired key is not minted into a budget. The
// in-memory twin (TestAuthRateLimiter_RefundRestoresAToken) cannot speak
// for the Lua script; this test does.
func TestValkeyRefundRestoresAToken(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	newLimiter := func() *valkeyAuthRateLimiter {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		return newValkeyAuthRateLimiter(rdb)
	}
	// Two replicas over one Valkey, like two pods: A consumes and refunds,
	// B must see the token back. A slow rate keeps refill noise out.
	a, b := newLimiter(), newLimiter()
	cfg := authBucketCfg{rate: 1.0 / 60.0, burst: 1}

	if ok, _ := a.allow("orglaunch:t1", cfg); !ok {
		t.Fatal("first allow refused on a full bucket")
	}
	if ok, _ := b.allow("orglaunch:t1", cfg); ok {
		t.Fatal("second allow (other replica) passed an emptied bucket")
	}
	a.refund("orglaunch:t1", cfg)
	if ok, _ := b.allow("orglaunch:t1", cfg); !ok {
		t.Fatal("allow after refund refused — the token did not come back across Valkey")
	}
	if ok, _ := a.allow("orglaunch:t1", cfg); ok {
		t.Fatal("allow after the refunded token was spent passed an empty bucket")
	}

	// A key the limiter never held refunds nothing — and the next allow on
	// a fresh key starts from a FULL bucket, not burst+1.
	b.refund("orglaunch:ghost", cfg)
	if ok, _ := a.allow("orglaunch:ghost", cfg); !ok {
		t.Fatal("allow on a refunded-but-never-held key refused")
	}
	if ok, _ := b.allow("orglaunch:ghost", cfg); ok {
		t.Fatal("the ghost refund minted a second token")
	}
}
