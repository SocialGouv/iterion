package forge

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusError is a forge call the sentinels do not cover: the answer's own
// status, kept as a NUMBER instead of only as text in a message. It is the
// type that lets a handler tell "the forge is rate-limiting us" from "iterion
// is broken" — a 429 read as an untyped error becomes a 500, and the
// operator, the logs and the alerting all learn the wrong thing.
//
// 401/403/404 never arrive here: they carry their own sentinels and typed
// forms (ErrUnauthorized, ErrForbidden, *NotFoundError).
type StatusError struct {
	Provider Provider
	// Op is the call that failed ("create oauth app").
	Op string
	// Code is the upstream HTTP status, verbatim.
	Code int
	// RetryAfter is what the forge asked us to wait, when it said so
	// (Retry-After, delta-seconds or HTTP-date). Zero = it said nothing;
	// never a guess.
	RetryAfter time.Duration
}

// Error keeps the wording every log line and operator already reads.
func (e *StatusError) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(string(e.Provider))
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	fmt.Fprintf(&b, "HTTP %d", e.Code)
	return b.String()
}

// RateLimited reports the one upstream status a caller can act on by waiting.
func (e *StatusError) RateLimited() bool { return e.Code == http.StatusTooManyRequests }

// Upstream5xx reports a fault on the forge's side — never iterion's.
func (e *StatusError) Upstream5xx() bool { return e.Code >= 500 }

// ParseRetryAfter reads a Retry-After header value: delta-seconds, or an
// HTTP-date, whichever the forge sent. Anything unparsable, negative or
// already past yields 0 — "the forge said nothing" is the honest answer, and
// a guessed delay would be worse than none.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}
