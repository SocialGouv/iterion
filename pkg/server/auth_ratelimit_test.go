package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// peekJSONField is the cheap helper /api/auth/login uses to extract
// `email` for the per-account rate-limit tier without breaking
// downstream json.Unmarshal. Its three branches (nil body, parse
// failure, value extracted) each have non-trivial consequences for
// distributed brute-force protection.

func TestPeekJSONField_ExtractedValue(t *testing.T) {
	body := []byte(`{"email":"alice@example.com","password":"x"}`)
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	got := peekJSONField(r, "email")
	if got != "alice@example.com" {
		t.Errorf("got %q", got)
	}
	// Body must be restored for downstream readers.
	rest := make([]byte, len(body))
	n, _ := r.Body.Read(rest)
	if n != len(body) || string(rest) != string(body) {
		t.Errorf("body not restored: got %q", string(rest[:n]))
	}
}

func TestPeekJSONField_MissingField(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(body))
	if got := peekJSONField(r, "email"); got != "" {
		t.Errorf("missing field should return empty, got %q", got)
	}
}

func TestPeekJSONField_NilBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.Body = nil
	if got := peekJSONField(r, "email"); got != "" {
		t.Errorf("nil body should return empty, got %q", got)
	}
}

func TestPeekJSONField_MalformedJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader([]byte(`{not json`)))
	if got := peekJSONField(r, "email"); got != "" {
		t.Errorf("malformed JSON should return empty, got %q", got)
	}
}

func TestPeekJSONField_NonStringValue(t *testing.T) {
	// Numeric / boolean fields don't satisfy the type assertion → "".
	r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader([]byte(`{"email":42}`)))
	if got := peekJSONField(r, "email"); got != "" {
		t.Errorf("non-string field should return empty, got %q", got)
	}
}

// ---- authRateLimiter ----

func TestAuthRateLimiter_AllowsWithinBurst(t *testing.T) {
	rl := newAuthRateLimiter()
	rl.now = func() time.Time { return time.Unix(0, 0) }
	cfg := authBucketCfg{rate: 1, burst: 3}
	for i := 0; i < 3; i++ {
		ok, retry := rl.allow("k", cfg)
		if !ok {
			t.Fatalf("attempt %d should be allowed, got retry=%v", i, retry)
		}
	}
}

func TestAuthRateLimiter_RejectsBeyondBurst(t *testing.T) {
	rl := newAuthRateLimiter()
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	cfg := authBucketCfg{rate: 1, burst: 2}
	rl.allow("k", cfg)
	rl.allow("k", cfg)
	ok, retry := rl.allow("k", cfg)
	if ok {
		t.Error("third attempt should be throttled at burst=2")
	}
	if retry <= 0 {
		t.Errorf("retry should be > 0, got %v", retry)
	}
}

func TestAuthRateLimiter_RefillsOverTime(t *testing.T) {
	rl := newAuthRateLimiter()
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	cfg := authBucketCfg{rate: 1, burst: 1}
	if ok, _ := rl.allow("k", cfg); !ok {
		t.Fatal("first call should be allowed")
	}
	if ok, _ := rl.allow("k", cfg); ok {
		t.Fatal("second call without time progression should be throttled")
	}
	// Advance time by 2 seconds → 2 tokens refilled, capped at burst=1.
	now = now.Add(2 * time.Second)
	if ok, _ := rl.allow("k", cfg); !ok {
		t.Error("call after refill should be allowed")
	}
}

func TestAuthRateLimiter_KeysAreIndependent(t *testing.T) {
	rl := newAuthRateLimiter()
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	cfg := authBucketCfg{rate: 1, burst: 1}
	if ok, _ := rl.allow("ip-A", cfg); !ok {
		t.Fatal("first key should be allowed")
	}
	// Different key starts with a fresh bucket.
	if ok, _ := rl.allow("ip-B", cfg); !ok {
		t.Error("second key should have its own bucket")
	}
}

func TestAuthRateLimiter_LRUEvictionUnderPressure(t *testing.T) {
	rl := newAuthRateLimiter()
	rl.maxKeys = 3
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	cfg := authBucketCfg{rate: 1, burst: 1}
	rl.allow("a", cfg)
	rl.allow("b", cfg)
	rl.allow("c", cfg)
	rl.allow("d", cfg) // should evict "a" (least recently used)
	if _, present := rl.buckets["a"]; present {
		t.Error("expected 'a' to be evicted")
	}
	if _, present := rl.buckets["d"]; !present {
		t.Error("expected 'd' to be present")
	}
}

// ---- retrySeconds / itoa ----

func TestRetrySeconds_RoundsUpToOneOrMore(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "1"},                  // floor at 1 second
		{time.Millisecond * 100, "1"}, // sub-second → 1
		{time.Second * 5, "5"},
		{time.Second*42 + time.Millisecond*500, "42"}, // floor
	}
	for _, c := range cases {
		t.Run(c.d.String(), func(t *testing.T) {
			if got := retrySeconds(c.d); got != c.want {
				t.Errorf("retrySeconds(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

func TestItoa(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{42, "42"},
		{100, "100"},
		{12345, "12345"},
		{-1, "-1"},
		{-42, "-42"},
	}
	for _, c := range cases {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
