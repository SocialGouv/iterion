package api

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfterDelay(t *testing.T) {
	// Integer seconds.
	if got := retryAfterDelay(http.Header{"Retry-After": {"5"}}); got != 5*time.Second {
		t.Errorf("seconds: got %v, want 5s", got)
	}
	// Absent header → 0 (caller uses its own backoff).
	if got := retryAfterDelay(http.Header{}); got != 0 {
		t.Errorf("absent: got %v, want 0", got)
	}
	// Zero / negative seconds → 0.
	if got := retryAfterDelay(http.Header{"Retry-After": {"0"}}); got != 0 {
		t.Errorf("zero: got %v, want 0", got)
	}
	// Unparseable → 0.
	if got := retryAfterDelay(http.Header{"Retry-After": {"soon"}}); got != 0 {
		t.Errorf("garbage: got %v, want 0", got)
	}
	// HTTP-date in the future → positive, roughly the delta.
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfterDelay(http.Header{"Retry-After": {future}}); got <= 0 || got > 31*time.Second {
		t.Errorf("future date: got %v, want ~30s", got)
	}
	// HTTP-date in the past → 0.
	past := time.Now().Add(-30 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfterDelay(http.Header{"Retry-After": {past}}); got != 0 {
		t.Errorf("past date: got %v, want 0", got)
	}
}
