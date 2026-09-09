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
//
// # The ErrNotFound class, and every pkg/forge sentinel's side of it
//
// The 404 arm reads MEMBERSHIP — errors.Is(err, forge.ErrNotFound) — not the
// sentinel's name. Two shapes therefore coexist in pkg/forge with nothing in
// the text telling them apart, and the difference is load-bearing: the store
// misses are what every "…but could not be recorded: %w" wrap in the forge
// layer carries, so moving one into the class turns that wrap into a 404 —
// "the forge has no such thing" for a write iterion itself failed — with no
// message anywhere changing. TestForgeSentinelFamilies_ArePinned pins each
// row below, and sweeps pkg/forge for declarations so a NEW sentinel is red
// until its family is stated here.
//
//	IN the class → 404 here
//	  forge.ErrNotFound                    the class itself
//	  forge.ErrHookNotFound                "%w: hook"
//	  *forge.NotFoundError                 Unwrap() → ErrNotFound; every typed forge 404
//
//	OUTSIDE it, iterion's own state — a store miss the forge never answered
//	  forge.ErrConnectionNotFound          0 → the caller's fault status
//	  forge.ErrIntegrationNotFound         0
//	  forge.ErrOAuthAppNotFound            0
//	  forge.ErrBoardBindingNotFound        0
//	  forge.ErrProvisionApprovalNotFound   0
//
//	OUTSIDE it, and never sent at all — the one 0 the caller never sees
//	  forge.ErrLocalPreflight              0 here; writeForgeUpstreamError reads it through isIterionFault and answers 500
//
//	OUTSIDE it, though the forge did answer 404 — the caller classifies
//	  forge.ErrProjectNotFound             0; a bind answers the board ref itself
//	  forge.ErrFileNotFound                0; config-share answers every read failure alike
//
//	OUTSIDE it, and not a 404 in any reading
//	  forge.ErrForbidden                   403  (arm above)
//	  forge.ErrUnauthorized                422  (arm above)
//	  forge.ErrPermissionsNotGranted       422  (arm above)
//	  forge.ErrOAuthAppExists              0
//	  forge.ErrRepoExists                  0
//	  forge.ErrFileConflict                0
//	  forge.ErrBoardSyncLeaseLost          0
//	  forge.ErrAvatarUnsupported           0
//	  forge.ErrSecurityReadMalformed       0
//	  forge.ErrSecurityReadNoOrgKey        0
//	  forge/github.ErrInstallationNotOwned  0
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
// gives it, echoing the forge's Retry-After. An error MARKED as iterion's
// own is answered 500 here, since the caller's default arm would name the
// forge for it. Reports false — writing nothing — only when the failure is
// neither: an unclassified error on a route whose failures are forge round
// trips, which its own 502 default then covers.
func writeForgeUpstreamError(w http.ResponseWriter, err error, format string, args ...any) bool {
	// The marker is what makes "not an answer from the forge" actionable:
	// forgeUpstreamStatus answers 0 for a request that never left AND for an
	// upstream failure it does not recognise, and those two want opposite
	// statuses. Deciding it here keeps the classifier a classifier.
	//
	// Asking FIRST is safe only while the two are disjoint, and today they
	// are: every marked return in MintInstallationToken is above
	// httpClient.Do, and both newIterionFault wraps are on failures no forge
	// answered. Mark an error that ALSO carries a forge status and this
	// order silently outranks it — a 429 with its Retry-After, a 404, would
	// become 500. So mark the STEP that failed, never a call that completed
	// a round trip.
	if isIterionFault(err) {
		httpError(w, http.StatusInternalServerError, format, args...)
		return true
	}
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

// iterionFault marks an error as iterion's own state failing — a store
// write, a marshal, a seal that will not open — on a route whose default
// arm answers 502 to serve the forge's own failures. Without the marker,
// the shared taxonomy cannot tell the two apart: forgeUpstreamStatus
// returning 0 means "not an answer from the forge", which is the truth
// AND the definition of iterion's own, but a route that ALSO makes a
// forge call after which its own state may fail (the avatar route: upload
// then persist) cannot safely default to 500 — an unclassified upstream
// error would then be blamed on iterion. The marker resolves it at the
// wrap site, where which step failed is known: mark the store-write
// failure; leave the raw forge error alone.
//
// A route whose ONLY failure path is a forge round-trip does NOT need
// this: an unclassified error there IS a forge error the classifier does
// not yet know about, and defaulting to 502 is the safe assumption.
//
// That premise is far easier to assert than to establish, and asserting
// it wrongly is how the inversion spreads. A forge CLIENT METHOD is not
// the same thing as a forge round-trip: pkg/forge/github's App client
// mints an installation token and signs the App JWT — parsing a stored
// private key — before its first socket, so a key that is not parseable
// PEM fails inside what reads like a pure `admin.ListRepos(ctx)`. Trace
// the call to its first byte on the wire before concluding a site is
// clean. That is what forge.ErrLocalPreflight settles for the mint
// chain: the marking happens where the wire boundary is visible, so a
// handler no longer has to derive it.
//
// It settles the MINT, and only the mint. InstallationInfo and Slug
// sign the same App JWT from the same stored key and hand the failure
// back unmarked, so the refresh route's "probe installation: %v" 502 is
// this same inversion on a second source. Marking those two would be
// half a fix and no answer changed: their callers write 502 themselves
// rather than through this package's junction (see newIterionFault).
//
// Finally: the 502 default is a per-route CHOICE, not the package's
// rule, and this marker exists for the routes that make it. A handler
// that can enumerate the forge's refusals and treat everything left as
// its own defaults to 500 instead and needs no marker —
// writeForgeOAuthAppError is that shape. A handler that cannot must
// keep 502 (an unclassified upstream error must not be blamed on
// iterion), and the marker is then the ONLY way it can ever answer 500.
// The two are not one doctrine: pick the default the route's error
// surface actually supports.
type iterionFault struct{ err error }

// newIterionFault wraps err so a handler whose default arm is 502 can
// tell iterion's own faults apart (answer 500) from a forge that
// answered or fell silent (keep 502 / the taxonomy code).
//
// The mark ACTS, at one junction: writeForgeUpstreamError asks
// isIterionFault before the classifier and answers 500. Marking a site
// is therefore one edit, not two — for a route that hands its failure
// to that junction.
//
// That junction is the whole reach, and it is NOT every 502-defaulting
// route. A handler that writes http.StatusBadGateway itself never
// consults the marker, and several do so while holding a client that
// can produce one: the App-token mint at the end of the install
// callback (forge_connect_routes.go), the board's issue sync, issue
// push and hook listing (board_forge.go), the review publish
// (forge_publish.go). Each still answers 502 for a key only iterion can
// read — the #969 inversion, on arms that are not this marker's to
// close: routing them here would also hand them the forge taxonomy
// (404, 429 + Retry-After, the mirrored 4xx), which is a change of its
// own argument, and copying the isIterionFault check into each is the
// duplicated guard this junction exists to avoid. So: when you mark a
// new error, check the HANDLER as well as the wrap site — the mark only
// changes an answer where writeForgeUpstreamError is the one writing
// it.
//
// forgeUpstreamStatus still has no case for it, deliberately — it says
// what the FORGE answered, and a marked error is one the forge never
// saw.
func newIterionFault(err error) error {
	if err == nil {
		return nil
	}
	return &iterionFault{err: err}
}

func (e *iterionFault) Error() string { return e.err.Error() }
func (e *iterionFault) Unwrap() error { return e.err }

// isIterionFault reports whether err (or anything it wraps) was marked as
// iterion's own, so a 502-default handler answers 500 instead. Two markers
// say the same thing from the two sides of the package boundary:
// iterionFault, wrapped in pkg/server where the failing step is known, and
// forge.ErrLocalPreflight, carried out of pkg/forge by work that ran before
// any byte reached the network.
func isIterionFault(err error) bool {
	var f *iterionFault
	return errors.As(err, &f) || errors.Is(err, forge.ErrLocalPreflight)
}

// retryAfterHeader renders a delay as delta-seconds, the form every client
// reads. Zero (the forge said nothing) renders empty — never a guess.
func retryAfterHeader(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.Itoa(int(math.Ceil(d.Seconds())))
}
