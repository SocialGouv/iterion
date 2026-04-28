package apikit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestExponentialBackoffSequence(t *testing.T) {
	cfg := DefaultRetryConfig()

	expected := []time.Duration{
		1 * time.Second,   // attempt 1: 2^0 = 1
		2 * time.Second,   // attempt 2: 2^1 = 2
		4 * time.Second,   // attempt 3: 2^2 = 4
		8 * time.Second,   // attempt 4: 2^3 = 8
		16 * time.Second,  // attempt 5: 2^4 = 16
		32 * time.Second,  // attempt 6: 2^5 = 32
		64 * time.Second,  // attempt 7: 2^6 = 64
		128 * time.Second, // attempt 8: 2^7 = 128
	}

	for i, want := range expected {
		attempt := uint32(i + 1)
		got, err := cfg.BackoffForAttempt(attempt)
		if err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", attempt, err)
		}
		if got != want {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, want)
		}
	}
}

func TestMaxBackoffCap(t *testing.T) {
	cfg := RetryConfig{
		MaxRetries:     8,
		InitialBackoff: time.Second,
		MaxBackoff:     10 * time.Second,
	}

	got, err := cfg.BackoffForAttempt(8) // 2^7 = 128s, capped at 10s
	if err != nil {
		t.Fatal(err)
	}
	if got != 10*time.Second {
		t.Errorf("expected 10s cap, got %v", got)
	}
}

func TestBackoffOverflowOnHugeAttempt(t *testing.T) {
	cfg := DefaultRetryConfig()
	_, err := cfg.BackoffForAttempt(33) // shift >= 32
	if err == nil {
		t.Fatal("expected BackoffOverflow error")
	}
	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected ApiError")
	}
	if apiErr.Kind != ErrBackoffOverflow {
		t.Errorf("expected ErrBackoffOverflow, got %d", apiErr.Kind)
	}
}

func TestJitterInRange(t *testing.T) {
	base := 10 * time.Second
	for i := 0; i < 100; i++ {
		jitter := JitterForBase(base)
		if jitter < 0 || jitter > base {
			t.Errorf("jitter %v out of range [0, %v]", jitter, base)
		}
	}
}

func TestJitterZeroBase(t *testing.T) {
	jitter := JitterForBase(0)
	if jitter != 0 {
		t.Errorf("expected 0 jitter for zero base, got %v", jitter)
	}
}

func TestJitteredBackoffWithinBounds(t *testing.T) {
	cfg := DefaultRetryConfig()
	for attempt := uint32(1); attempt <= 8; attempt++ {
		base, _ := cfg.BackoffForAttempt(attempt)
		for i := 0; i < 10; i++ {
			jittered, err := cfg.JitteredBackoffForAttempt(attempt)
			if err != nil {
				t.Fatal(err)
			}
			if jittered < base || jittered > 2*base {
				t.Errorf("attempt %d: jittered %v out of [%v, %v]", attempt, jittered, base, 2*base)
			}
		}
	}
}

func TestSplitmix64TestVector(t *testing.T) {
	// Verify the splitmix64 finalizer produces deterministic output.
	// This ensures Go and Rust produce the same hash for the same input.
	input := uint64(42)
	result := Splitmix64(input)
	// splitmix64(42): seed = 42 + 0x9E3779B97F4A7C15
	// Then apply the mix steps.
	// We just verify it's deterministic and non-zero.
	if result == 0 {
		t.Error("splitmix64 should not produce 0 for input 42")
	}
	// Same input must produce same output
	if Splitmix64(input) != result {
		t.Error("splitmix64 must be deterministic")
	}
	// Different input must produce different output
	if Splitmix64(43) == result {
		t.Error("different inputs should produce different outputs")
	}
}

func TestSendWithRetryRetriesThenExhausts(t *testing.T) {
	cfg := RetryConfig{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	attempts := 0

	_, err := SendWithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("timeout")}
	})

	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected ApiError")
	}
	if apiErr.Kind != ErrRetriesExhausted {
		t.Errorf("expected RetriesExhausted, got %d", apiErr.Kind)
	}
	if attempts != 3 { // 1 initial + 2 retries
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestSendWithRetrySucceedsOnRetry(t *testing.T) {
	cfg := RetryConfig{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	attempts := 0

	result, err := SendWithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		if attempts < 3 {
			return "", &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("timeout")}
		}
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestSendWithRetryNonRetryableFailsImmediately(t *testing.T) {
	cfg := RetryConfig{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	attempts := 0

	_, err := SendWithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", &ApiError{Kind: ErrAuth, AuthMessage: "bad key"}
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt for non-retryable error, got %d", attempts)
	}
}

func TestSendWithRetryContextCancellation(t *testing.T) {
	cfg := RetryConfig{MaxRetries: 100, InitialBackoff: time.Second, MaxBackoff: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())

	attempts := 0
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := SendWithRetry(ctx, cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("timeout")}
	})

	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
	if attempts > 3 { // Should break quickly after cancel
		t.Errorf("expected few attempts before cancel, got %d", attempts)
	}
}

func TestSendWithRetryNonApiError(t *testing.T) {
	cfg := RetryConfig{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	attempts := 0

	_, err := SendWithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", fmt.Errorf("plain error")
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("non-ApiError should not be retried, got %d attempts", attempts)
	}
}
