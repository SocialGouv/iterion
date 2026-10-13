package forge

import (
	"encoding/json"
	"errors"
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
// forms (ErrUnauthorized, ErrForbidden, *NotFoundError) — except a 403 the
// forge marks as a rate limit (RateLimit), which is a wait, not a missing
// grant.
type StatusError struct {
	Provider Provider
	// Op is the call that failed ("create oauth app"). A rate-limited answer
	// the transport typed is named by its method and path as sent, until
	// WithOp gives it the caller's name.
	Op string
	// Code is the upstream HTTP status, verbatim.
	Code int
	// ResetAt is when the forge said a rate limit's wait ends (LimitResetAt),
	// fixed when the answer arrived. Zero = it said nothing; never a guess.
	ResetAt time.Time
	// Limit marks an answer the forge said is a rate limit even though its
	// status does not (GitHub answers its limits 403). A 429 is one by status.
	Limit bool
	// Detail is the forge's own message for a rate-limited answer (GitHub's
	// names the installation or the user), empty when it gave none.
	Detail string
	// Cause is the typed answer this error reads as a limit, when there is
	// one: a GraphQL errors array whose entry said RATE_LIMITED.
	Cause error
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
	switch {
	case e.RateLimited() && !e.ResetAt.IsZero():
		fmt.Fprintf(&b, ": rate limited until %s", e.ResetAt.UTC().Format(time.RFC3339))
	case e.Limit && e.Code != http.StatusTooManyRequests:
		b.WriteString(": rate limited")
	}
	if e.Detail != "" {
		b.WriteString(" (")
		b.WriteString(e.Detail)
		b.WriteString(")")
	}
	return b.String()
}

// RateLimited reports the one upstream answer a caller can act on by waiting:
// a 429, or an answer the forge marked as a limit.
func (e *StatusError) RateLimited() bool { return e.Code == http.StatusTooManyRequests || e.Limit }

// Unwrap exposes Cause, so errors.As still reaches the typed answer.
func (e *StatusError) Unwrap() error { return e.Cause }

// RateLimit reads a non-2xx answer the way the forges say "wait": a 429
// always; a 403 only when the answer says so — a Retry-After header (GitHub's
// secondary limit), a message naming a rate limit, or, when the answer carries
// no message to read, X-RateLimit-Remaining: 0 (the primary limit). The call
// that spends the last unit of budget carries Remaining: 0 whatever it
// answers, so a message naming another refusal keeps that refusal. A plain
// 403 is a missing grant and stays one. reset is LimitResetAt's.
func RateLimit(code int, hdr http.Header, body []byte) (limited bool, reset time.Time) {
	switch code {
	case http.StatusTooManyRequests:
		limited = true
	case http.StatusForbidden:
		switch {
		case strings.TrimSpace(hdr.Get("Retry-After")) != "",
			strings.Contains(strings.ToLower(string(body)), "rate limit"):
			limited = true
		case strings.TrimSpace(hdr.Get("X-RateLimit-Remaining")) == "0":
			limited = forgeMessage(body) == ""
		}
	}
	if !limited {
		return false, time.Time{}
	}
	return true, LimitResetAt(hdr, time.Now())
}

// maxForgeWait bounds the waits LimitResetAt believes. A rate limit's window
// is an hour on GitHub and a configured throttle period on GitLab; an instant
// a month away is a forge's garbage — an epoch in milliseconds, a year 3000 —
// and "the forge said nothing" is the honest reading of it.
const maxForgeWait = 31 * 24 * time.Hour

// LimitResetAt is when a rate-limited answer said its wait ends, read at now:
// the later of Retry-After and the reset (X-RateLimit-Reset, RateLimit-Reset)
// of a budget the answer does not say has units left. GitHub documents the two
// as independent "not before" rules, so the later binds; and it stamps its
// primary window's reset on every answer, so a secondary limit with budget
// left must not read as "wait for the window". Zero when the forge said
// nothing, or nothing believable (maxForgeWait): never a guess.
func LimitResetAt(hdr http.Header, now time.Time) time.Time {
	var at time.Time
	if wait := ParseRetryAfter(hdr.Get("Retry-After")); wait > 0 {
		at = now.Add(wait)
	}
	for _, h := range [...]struct{ remaining, reset string }{
		{"X-RateLimit-Remaining", "X-RateLimit-Reset"},
		{"RateLimit-Remaining", "RateLimit-Reset"},
	} {
		if left := strings.TrimSpace(hdr.Get(h.remaining)); left != "" && left != "0" {
			continue
		}
		if reset := parseRateLimitReset(hdr.Get(h.reset), now); reset.After(at) {
			at = reset
		}
	}
	return at
}

// RateLimitBudget is the budget a forge answer reports: what is left, out of
// how much, and when the window resets. Limit and ResetAt are zero when the
// answer does not say. Resource names a budget of the answer's API beyond its
// default one — GitHub's search budget behind a REST path — and is empty when
// the answer names the default (core, graphql) or nothing at all.
type RateLimitBudget struct {
	Remaining, Limit int64
	ResetAt          time.Time
	Resource         string
}

// RateLimitBudgetOf reads the budget GitHub reports on every answer, 2xx
// included — X-RateLimit-Remaining, -Limit and -Reset — read at now. ok is
// false when the answer reports no remaining count.
func RateLimitBudgetOf(hdr http.Header, now time.Time) (RateLimitBudget, bool) {
	remaining, err := strconv.ParseInt(strings.TrimSpace(hdr.Get("X-RateLimit-Remaining")), 10, 64)
	if err != nil || remaining < 0 {
		return RateLimitBudget{}, false
	}
	b := RateLimitBudget{Remaining: remaining, ResetAt: parseRateLimitReset(hdr.Get("X-RateLimit-Reset"), now)}
	if limit, err := strconv.ParseInt(strings.TrimSpace(hdr.Get("X-RateLimit-Limit")), 10, 64); err == nil && limit > 0 {
		b.Limit = limit
	}
	switch strings.TrimSpace(hdr.Get("X-RateLimit-Resource")) {
	case "", "core", "graphql": // the API's own budget: rest's is core, graphql's says graphql
	default:
		b.Resource = strings.TrimSpace(hdr.Get("X-RateLimit-Resource"))
	}
	return b, true
}

// parseRateLimitReset reads a reset header as an instant: a Unix epoch
// (GitHub, GitLab) or, for a small value, a delta in seconds from now (the
// IETF RateLimit-Reset form). Anything unparsable or not after now yields the
// zero time.
func parseRateLimitReset(v string, now time.Time) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	at := time.Unix(n, 0)
	if n < 1_000_000_000 {
		at = now.Add(time.Duration(n) * time.Second)
	}
	if !at.After(now) || at.Sub(now) > maxForgeWait {
		return time.Time{}
	}
	return at
}

// RateLimitErr is the typed error for an answer RateLimit marks, or nil.
// op names the call as the caller would ("GET pull"); a transport that does
// not know it passes the method and the path, and WithOp renames it where
// the path says nothing.
func RateLimitErr(errPrefix, op string, code int, hdr http.Header, body []byte) error {
	limited, reset := RateLimit(code, hdr, body)
	if !limited {
		return nil
	}
	return &StatusError{Provider: Provider(errPrefix), Op: op, Code: code, ResetAt: reset, Limit: true, Detail: forgeMessage(body)}
}

// forgeMessage is the `message` of a JSON error body (GitHub, Forgejo and
// GitLab all use the key), trimmed and capped; empty for anything else — a
// proxy's HTML page is not a message.
func forgeMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	msg := strings.TrimSpace(e.Message)
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return msg
}

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
		if secs <= 0 || time.Duration(secs) > maxForgeWait/time.Second {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 && d <= maxForgeWait {
			return d
		}
	}
	return 0
}

// WithOp names a rate-limited answer the transport typed by the call its
// caller knows ("create oauth app"): the transport sees only the method and
// the path, and a GraphQL path is the same for every query. Any other error
// comes back unchanged.
func WithOp(err error, op string) error {
	var se *StatusError
	if errors.As(err, &se) && se.RateLimited() {
		se.Op = op
	}
	return err
}
