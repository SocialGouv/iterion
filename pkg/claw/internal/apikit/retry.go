package apikit

import (
	"context"
	"sync/atomic"
	"time"
)

// RetryConfig holds the parameters for exponential backoff with jitter.
type RetryConfig struct {
	MaxRetries     uint32
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// DefaultRetryConfig returns the default retry configuration matching Rust defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     8,
		InitialBackoff: time.Second,
		MaxBackoff:     128 * time.Second,
	}
}

// BackoffForAttempt calculates the exponential backoff duration for the given
// attempt number (1-indexed). Returns a BackoffOverflow error if the shift
// would overflow.
func (c RetryConfig) BackoffForAttempt(attempt uint32) (time.Duration, error) {
	shift := attempt
	if shift > 0 {
		shift--
	}
	if shift >= 32 {
		return 0, &ApiError{
			Kind:            ErrBackoffOverflow,
			OverflowAttempt: attempt,
			BaseDelay:       c.InitialBackoff,
		}
	}
	multiplier := uint32(1) << shift
	delay := c.InitialBackoff * time.Duration(multiplier)
	// Guard against multiplication overflow for exotic configs with very
	// large InitialBackoff values. Mirrors Rust's checked_mul fallback to
	// max_backoff. With the default 1s base the max product is 2^31 s which
	// fits comfortably in time.Duration (int64 nanos), but a caller-supplied
	// base of e.g. 1h at attempt 20 would overflow.
	if delay < 0 || delay > c.MaxBackoff {
		delay = c.MaxBackoff
	}
	return delay, nil
}

// jitterCounter is a process-wide counter for decorrelating jitter.
var jitterCounter atomic.Uint64

// JitteredBackoffForAttempt returns the backoff with additive jitter in [0, base].
func (c RetryConfig) JitteredBackoffForAttempt(attempt uint32) (time.Duration, error) {
	base, err := c.BackoffForAttempt(attempt)
	if err != nil {
		return 0, err
	}
	return base + JitterForBase(base), nil
}

// JitterForBase returns a random jitter duration in [0, base] using splitmix64.
func JitterForBase(base time.Duration) time.Duration {
	baseNanos := uint64(base.Nanoseconds())
	if baseNanos == 0 {
		return 0
	}
	rawNanos := uint64(time.Now().UnixNano())
	tick := jitterCounter.Add(1) - 1

	mixed := rawNanos + tick + 0x9E3779B97F4A7C15 // splitmix64 offset
	mixed = (mixed ^ (mixed >> 30)) * 0xBF58476D1CE4E5B9
	mixed = (mixed ^ (mixed >> 27)) * 0x94D049BB133111EB
	mixed ^= mixed >> 31

	jitterNanos := mixed % (baseNanos + 1)
	return time.Duration(jitterNanos)
}

// Splitmix64 applies the splitmix64 finalizer to the input value.
// Exported for test vector verification against Rust outputs.
func Splitmix64(input uint64) uint64 {
	mixed := input + 0x9E3779B97F4A7C15
	mixed = (mixed ^ (mixed >> 30)) * 0xBF58476D1CE4E5B9
	mixed = (mixed ^ (mixed >> 27)) * 0x94D049BB133111EB
	mixed ^= mixed >> 31
	return mixed
}

// SendWithRetry executes fn with retry logic. It retries on retryable ApiErrors
// up to MaxRetries times with jittered exponential backoff. Non-retryable errors
// and context cancellation break the loop immediately.
func SendWithRetry[T any](ctx context.Context, cfg RetryConfig, fn func(ctx context.Context) (T, error)) (T, error) {
	var lastErr *ApiError
	var zero T

	for attempt := uint32(1); ; attempt++ {
		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}

		apiErr, ok := err.(*ApiError)
		if !ok {
			// Non-ApiError: don't retry
			return zero, err
		}

		if !apiErr.IsRetryable() {
			return zero, apiErr
		}

		lastErr = apiErr

		if attempt > cfg.MaxRetries {
			break
		}

		delay, backoffErr := cfg.JitteredBackoffForAttempt(attempt)
		if backoffErr != nil {
			return zero, backoffErr
		}

		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	return zero, &ApiError{
		Kind:      ErrRetriesExhausted,
		Attempts:  cfg.MaxRetries + 1,
		LastError: lastErr,
	}
}
