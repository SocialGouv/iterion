package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run origin gate: a request that addresses a run by ID may name the
// birth of the run it means (store.Run.BirthID, exposed as `birth_id` on the
// snapshot header and the run rows). A run deleted and recreated under the
// same ID has another birth, so the server refuses a request whose
// expectation no longer matches — 409 run_origin_lost — before any handler
// runs: the successor's data never answers a request meant for its
// predecessor, and no action meant for the predecessor reaches it.
//
// The expectation is optional on every route: the CLI, MCP, bots and the
// remote API address runs by ID by contract, and a studio served by another
// version may send none. Without one the gate passes the request through
// and reads nothing. The studio sends one for every run whose birth it knows.
//
// One choke point: runOriginGate sits inside authMiddleware, so it runs on
// the authenticated chain AFTER scopeRunByID's 404 and ladder (no existence
// oracle) and on the DisableAuth chain the local studio serves. It classifies
// with runIDClass — the route the mux matches — and never by path prefix.

const (
	// expectedBirthHeader carries the expectation on a fetch.
	expectedBirthHeader = "X-Iterion-Expected-Birth"
	// expectedBirthParam carries it where no header can travel: an <img> or
	// <iframe> src, a link, a WebSocket URL.
	expectedBirthParam = "expected_birth"
	// expectedBirthHistorical expects a historical run, one with no birth
	// identity. Births are minted uppercase base32 (store.NewRunBirthID), so
	// the lowercase sentinel never collides with one.
	expectedBirthHistorical = "historical"
	// runBirthCheckEnv=0 turns the comparison off (rollback, or a fleet whose
	// writers predate birth identities): every expectation is admitted
	// unread. Operator-grade: a project .env cannot plant it.
	runBirthCheckEnv = "ITERION_RUN_BIRTH_CHECK"

	errCodeInvalidExpectedBirth = "invalid_expected_birth"
	errCodeScopeUnavailable     = "scope_unavailable"
	// errCodeStoreUnsupported refuses an expectation for a run of a foreign
	// store (`?store=`): births are compared in the server's own store only.
	errCodeStoreUnsupported = "store_unsupported"
	errStoreUnsupported     = "an expected birth is checked in this server's own store only: a run of a foreign ?store= is read without one"
)

var expectedBirthShape = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

// runBirthCheckOff reports whether the operator turned the comparison off.
func runBirthCheckOff() bool {
	return strings.TrimSpace(envOperator(runBirthCheckEnv)) == "0"
}

// parseExpectedBirth maps a wire value to the birth it expects: the sentinel
// to the empty birth of a historical run, any other value to itself.
func parseExpectedBirth(raw string) (string, error) {
	if raw == expectedBirthHistorical {
		return "", nil
	}
	if !expectedBirthShape.MatchString(raw) {
		return "", fmt.Errorf("an expected birth is %q or 1-128 ASCII letters and digits, got %q", expectedBirthHistorical, raw)
	}
	return raw, nil
}

// expectedBirthOf reads the expectation a request carries in its header or
// its query. Both may be present when they agree; a repeated or conflicting
// value is an error, never a choice.
func expectedBirthOf(r *http.Request) (birth string, present bool, err error) {
	header := r.Header.Values(expectedBirthHeader)
	query := r.URL.Query()[expectedBirthParam]
	if len(header) == 0 && len(query) == 0 {
		return "", false, nil
	}
	if len(header) > 1 || len(query) > 1 {
		return "", true, fmt.Errorf("the expected birth is given more than once")
	}
	raw := ""
	switch {
	case len(header) == 1 && len(query) == 1:
		if header[0] != query[0] {
			return "", true, fmt.Errorf("the %s header (%q) and the %s parameter (%q) disagree", expectedBirthHeader, header[0], expectedBirthParam, query[0])
		}
		raw = header[0]
	case len(header) == 1:
		raw = header[0]
	default:
		raw = query[0]
	}
	birth, err = parseExpectedBirth(raw)
	return birth, true, err
}

// withoutExpectedBirth returns r without its expectation, so no handler
// forwards it: the preview proxy relays its query to the run's target. The
// rest of the query is kept byte for byte.
func withoutExpectedBirth(r *http.Request) *http.Request {
	out := withoutQueryParam(r, expectedBirthParam)
	out.Header.Del(expectedBirthHeader)
	return out
}

// withoutQueryParam returns a copy of r without any `key` query parameter,
// the rest of the query kept byte for byte.
func withoutQueryParam(r *http.Request, key string) *http.Request {
	out := r.Clone(r.Context())
	if out.URL.RawQuery != "" {
		parts := strings.Split(out.URL.RawQuery, "&")
		kept := parts[:0]
		for _, p := range parts {
			name, _, _ := strings.Cut(p, "=")
			if k, err := url.QueryUnescape(name); err == nil && k == key {
				continue
			}
			kept = append(kept, p)
		}
		out.URL.RawQuery = strings.Join(kept, "&")
		out.RequestURI = out.URL.RequestURI()
	}
	return out
}

// admittedRunOrigin is the run of the server's own store the gate compared
// for a request: what a handler binds its reads to, and what the run socket
// keeps.
type admittedRunOrigin struct {
	runID  string
	origin store.RunObservation
}

type admittedRunOriginKey struct{}

// admittedOrigin returns the origin the gate admitted for runID, if any.
func admittedOrigin(ctx context.Context, runID string) (store.RunObservation, bool) {
	a, ok := ctx.Value(admittedRunOriginKey{}).(admittedRunOrigin)
	if !ok || a.runID != runID {
		return store.RunObservation{}, false
	}
	return a.origin, true
}

// runOriginGate is the choke point (see the file comment).
func (s *Server) runOriginGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.runs == nil {
			next.ServeHTTP(w, r)
			return
		}
		runID, inClass, fixedSegments := s.runIDClass(r)
		if !inClass || runID == "" {
			next.ServeHTTP(w, r)
			return
		}
		birth, present, err := expectedBirthOf(r)
		if !present {
			next.ServeHTTP(w, r)
			return
		}
		// The guarantee covers the server's own store: an expectation for a
		// run of a foreign store (a cross-store view) is refused, never
		// compared with the run of the same ID here — a scope refusal the
		// birth-check escape hatch does not lift. A socket is accepted then
		// closed: a refused handshake reaches a browser as 1006, which its
		// client would redial forever.
		if foreignStore(r) {
			if websocket.IsWebSocketUpgrade(r) {
				s.closeRefusedSocket(w, r, runID, "", errCodeStoreUnsupported, errStoreUnsupported)
				return
			}
			s.httpErrorCode(w, r, http.StatusBadRequest, errCodeStoreUnsupported, "%s", errStoreUnsupported)
			return
		}
		if runBirthCheckOff() {
			next.ServeHTTP(w, withoutExpectedBirth(r))
			return
		}
		if err != nil {
			if websocket.IsWebSocketUpgrade(r) {
				s.closeRefusedSocket(w, r, runID, "", errCodeInvalidExpectedBirth, err.Error())
				return
			}
			s.httpErrorCode(w, r, http.StatusBadRequest, errCodeInvalidExpectedBirth, "%v", err)
			return
		}
		origin, err := s.runs.CaptureRunOrigin(r.Context(), runID)
		switch {
		case err == nil && origin.Header().BirthID == birth:
			ctx := context.WithValue(r.Context(), admittedRunOriginKey{}, admittedRunOrigin{runID: runID, origin: origin})
			next.ServeHTTP(w, withoutExpectedBirth(r.WithContext(ctx)))
		case err == nil || runOriginLost(err):
			s.refuseRunOriginLost(w, r, runID, fixedSegments)
		default:
			s.httpErrorCode(w, r, http.StatusServiceUnavailable, errCodeScopeUnavailable,
				"run %s could not be checked against its expected birth: %v", runID, err)
		}
	})
}

// writeRunOriginLost answers 409 run_origin_lost naming the run, so a client
// whose request carried the run in its body (not its path) knows which run
// it lost.
func (s *Server) writeRunOriginLost(w http.ResponseWriter, r *http.Request, runID string) {
	s.reflectAllowedOrigin(w, r)
	httpx.WriteErrorJSON(errtrack.HTTPErrorWriter(w, r, s.logger), http.StatusConflict, map[string]string{
		"error":      "run " + runID + " was replaced or deleted: it is not the run this request expected",
		"error_code": wsErrRunOriginLost,
		"run_id":     runID,
	})
}

// gateCloseDrain bounds the wait for the client's close reply after a
// refused socket's close frame.
const gateCloseDrain = 2 * time.Second

// refuseRunOriginLost answers a request whose expected birth no longer
// matches. A plain request gets 409. A WebSocket upgrade is accepted, then
// closed scope_lost — a browser never sees a refused handshake's status, it
// sees 1006 and redials. The run socket first sends its run_origin_lost
// error envelope; the shell (PTY binary plus its own control frames) and the
// CDP pump speak other protocols and get the close alone, the token their
// clients map to a lost run.
func (s *Server) refuseRunOriginLost(w http.ResponseWriter, r *http.Request, runID string, fixedSegments int) {
	if !websocket.IsWebSocketUpgrade(r) {
		s.writeRunOriginLost(w, r, runID)
		return
	}
	envelope := ""
	if fixedSegments == runScopePrefixes[1].fixedSegments && len(routeSubPath(r.URL.EscapedPath(), fixedSegments)) == 0 {
		envelope = wsErrRunOriginLost
	}
	s.closeRefusedSocket(w, r, runID, envelope, wsCloseScopeLost, wsCauseOriginLost)
}

// closeRefusedSocket accepts a socket the gate refuses and closes it naming
// token — the reason a client maps, where a refused handshake would reach a
// browser as 1006 and be redialed. A non-empty envelope first sends the run
// socket's error envelope with that code.
func (s *Server) closeRefusedSocket(w http.ResponseWriter, r *http.Request, runID, envelope, token, cause string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already answered the HTTP error.
		if s.logger != nil {
			s.logger.Error("server: refuse the socket of run %s (%s): upgrade: %v", runID, token, err)
		}
		return
	}
	defer func() { _ = conn.Close() }()
	if envelope != "" {
		payload, _ := json.Marshal(wsErrorPayload{Code: envelope, Message: cause})
		frame, _ := json.Marshal(runWSEnvelope{Type: wsTypeError, Payload: payload})
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			return
		}
	}
	if err := writeWSClose(conn, websocket.CloseNormalClosure, token, cause); err != nil {
		return
	}
	// Wait for the client's close reply: closing first would reset a
	// connection whose client already queued a frame, and a reset can drop
	// the close frame before the client reads it.
	_ = conn.SetReadDeadline(time.Now().Add(gateCloseDrain))
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

// writeBoundReadFailure answers a bound read that failed: a lost origin is
// 409 run_origin_lost; anything else falls to the caller's own mapping.
func (s *Server) writeBoundReadFailure(w http.ResponseWriter, r *http.Request, runID string, bound bool, err error) bool {
	if !bound || !runOriginLost(err) {
		return false
	}
	s.writeRunOriginLost(w, r, runID)
	return true
}

// errCarriedRunOriginLost is a body-carried run whose loaded birth is not
// the one the request expected.
type errCarriedRunOriginLost struct{ runID string }

func (e errCarriedRunOriginLost) Error() string {
	return "run " + e.runID + " was replaced or deleted: it is not the run this request expected"
}

// checkCarriedBirth compares the birth a request body expects for a run it
// carries by ID with the run the handler just loaded. An empty expectation
// is unchecked; the sentinel expects a historical run; a malformed value is
// an invalid request. ITERION_RUN_BIRTH_CHECK=0 admits every expectation.
func checkCarriedBirth(field, expected string, loaded *store.Run) error {
	if expected == "" || loaded == nil || runBirthCheckOff() {
		return nil
	}
	birth, err := parseExpectedBirth(expected)
	if err != nil {
		return errInvalidCarriedBirth{fmt.Errorf("%s: %w", field, err)}
	}
	if loaded.BirthID != birth {
		return errCarriedRunOriginLost{runID: loaded.ID}
	}
	return nil
}

// validateCarriedBirth refuses a body-carried expectation that is not a
// birth before anything is read for it: a request answered before its run's
// birth is compared (an assistant run deleted or unavailable, a watch that
// does not resolve) never accepts, nor answers as a loss, what
// checkCarriedBirth would refuse.
func validateCarriedBirth(field, expected string) error {
	if expected == "" || runBirthCheckOff() {
		return nil
	}
	if _, err := parseExpectedBirth(expected); err != nil {
		return errInvalidCarriedBirth{fmt.Errorf("%s: %w", field, err)}
	}
	return nil
}

// errInvalidCarriedBirth is a body-carried expectation that is not a birth.
type errInvalidCarriedBirth struct{ err error }

func (e errInvalidCarriedBirth) Error() string { return e.err.Error() }
func (e errInvalidCarriedBirth) Unwrap() error { return e.err }

// carriedBirthFailure reports whether err is a checkCarriedBirth refusal,
// for a caller whose other errors map to another status.
func carriedBirthFailure(err error) bool {
	var lost errCarriedRunOriginLost
	var invalid errInvalidCarriedBirth
	return errors.As(err, &lost) || errors.As(err, &invalid)
}

// writeCarriedBirthFailure answers a checkCarriedBirth error: 409
// run_origin_lost for a mismatch, 400 invalid_expected_birth otherwise.
func (s *Server) writeCarriedBirthFailure(w http.ResponseWriter, r *http.Request, err error) {
	var lost errCarriedRunOriginLost
	if errors.As(err, &lost) {
		s.writeRunOriginLost(w, r, lost.runID)
		return
	}
	s.httpErrorCode(w, r, http.StatusBadRequest, errCodeInvalidExpectedBirth, "%v", err)
}

// writeCarriedBirthFailureJSON is writeCarriedBirthFailure for a writer with
// no Server behind it (WorkspaceHost).
func writeCarriedBirthFailureJSON(w http.ResponseWriter, err error) {
	var lost errCarriedRunOriginLost
	if errors.As(err, &lost) {
		httpx.WriteErrorJSON(w, http.StatusConflict, map[string]string{
			"error": lost.Error(), "error_code": wsErrRunOriginLost, "run_id": lost.runID,
		})
		return
	}
	httpx.WriteError(w, http.StatusBadRequest, errCodeInvalidExpectedBirth, err.Error())
}
