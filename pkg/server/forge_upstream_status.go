package server

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgeUpstreamStatus is THE table every forge-facing handler consults before
// falling back to its own fault status. It exists because the fall-through is
// what lies: a rate limit answered 500 tells the operator, the logs, Sentry
// and every alert that iterion broke, when the truth is that a third party is
// throttling us — measured 34 times in 60 calls on the OAuth-app route.
//
//	*forge.PermissionError            422  a NAMED grant the operator can approve
//	ErrPermissionsNotGranted          422  an installation approved too narrowly
//	ErrUnauthorized                   422  the credential was rejected — reconnect, don't retry
//	*forge.NotFoundError / ErrNotFound 404 the forge has no such thing under this credential
//	ErrForbidden                      403  refused, with no permission named
//	*forge.StatusError, 429           429  + Retry-After when the forge sent one
//	*forge.StatusError, 5xx           502  the forge's own fault
//	*forge.StatusError, other 4xx     4xx  mirrored: it answers the request we sent
//	a transport failure (*url.Error)  502  no answer at all, still not ours
//	anything else                       0  NOT an answer from the forge
//
// A 0 means "iterion's own" — a marshal error, a seal that will not open, a
// store write — and the caller answers it with the fault status it already
// used. Only that arm may be a 500.
//
// The second result is the Retry-After to echo, empty unless the forge named
// one: a delay iterion invented would be worse than none.
func forgeUpstreamStatus(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var pe *forge.PermissionError
	switch {
	case errors.As(err, &pe), errors.Is(err, forge.ErrPermissionsNotGranted):
		// The credential is valid and short of a grant the call needs: a
		// configuration the operator closes, never an outage to retry.
		return http.StatusUnprocessableEntity, ""
	case errors.Is(err, forge.ErrNotFound):
		return http.StatusNotFound, ""
	case errors.Is(err, forge.ErrForbidden):
		return http.StatusForbidden, ""
	case errors.Is(err, forge.ErrUnauthorized):
		// The token iterion holds was rejected. Reconnecting fixes it;
		// retrying never does, which is exactly what a 5xx would invite.
		return http.StatusUnprocessableEntity, ""
	}
	var se *forge.StatusError
	if errors.As(err, &se) {
		switch {
		case se.RateLimited():
			return http.StatusTooManyRequests, retryAfterHeader(se.RetryAfter)
		case se.Upstream5xx():
			return http.StatusBadGateway, ""
		case se.Code >= 400:
			// The forge is answering the request we sent — an operator can
			// act on 400/409/422; mirroring is more useful than flattening.
			return se.Code, ""
		default:
			return http.StatusBadGateway, ""
		}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// No answer at all — an unreachable host, a TLS failure, a timeout.
		return http.StatusBadGateway, ""
	}
	return 0, ""
}

// writeForgeUpstreamError answers err with the status forgeUpstreamStatus
// gives it, echoing the forge's Retry-After. Reports false — writing
// nothing — when the failure is not an answer from the forge, so the caller
// keeps its own fault status for the errors that really are iterion's.
func writeForgeUpstreamError(w http.ResponseWriter, err error, format string, args ...any) bool {
	code, retryAfter := forgeUpstreamStatus(err)
	if code == 0 {
		return false
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	httpError(w, code, format, args...)
	return true
}

// retryAfterHeader renders a delay as delta-seconds, the form every client
// reads. Zero (the forge said nothing) renders empty — never a guess.
func retryAfterHeader(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}
