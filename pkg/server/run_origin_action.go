package server

import (
	"context"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// actionOrigin is what a handler acting on one run by ID knows of the origin
// runOriginGate compared for it. checked is false when the request carried no
// expectation, under ITERION_RUN_BIRTH_CHECK=0, or for another run than the
// path's: the handler then calls its service exactly as an unchecked caller
// (the CLI, MCP, bots, an older studio) does — the request context unbound,
// no read added.
type actionOrigin struct {
	runID   string
	origin  store.RunObservation
	checked bool
	store   store.RunStore
}

// admittedAction returns the gate's admission for runID. It never reads the
// request's expectation itself: the gate strips it, and an admission is the
// only proof that the birth was compared.
func (s *Server) admittedAction(r *http.Request, runID string) actionOrigin {
	o, ok := admittedOrigin(r.Context(), runID)
	if !ok {
		return actionOrigin{runID: runID}
	}
	a := actionOrigin{runID: runID, origin: o, checked: true}
	if s.runs != nil {
		a.store = s.runs.RunStore()
	}
	return a
}

// bind returns ctx bound to the admitted origin (store.BindRunObservation),
// so every store access a service call makes under it compares the run it
// reaches with the run the request expected, and refuses a successor
// recreated after the admission. The binding is for the service call of the
// action only: never the identity context, never anything an execution keeps.
// An unchecked action gets ctx unchanged. A store that may drop the context
// on a read or a write (store.RequireBoundObservationStore) is refused: its
// accesses could reach whatever run holds the ID now.
func (a actionOrigin) bind(ctx context.Context) (context.Context, error) {
	if !a.checked {
		return ctx, nil
	}
	if err := store.RequireBoundObservationStore(a.store); err != nil {
		return nil, err
	}
	return store.BindRunObservation(ctx, a.origin)
}

// actionContext is bind on the request context; a refusal answers 503
// scope_unavailable and the action does not run.
func (s *Server) actionContext(w http.ResponseWriter, r *http.Request, a actionOrigin) (context.Context, bool) {
	return s.bindAction(w, r, a, r.Context())
}

// bindAction is bind on ctx, a context the handler derived from its request
// (detached, tenant-stamped); a refusal answers 503 scope_unavailable and the
// action does not run.
func (s *Server) bindAction(w http.ResponseWriter, r *http.Request, a actionOrigin, ctx context.Context) (context.Context, bool) {
	bound, err := a.bind(ctx)
	if err != nil {
		s.httpErrorCode(w, r, http.StatusServiceUnavailable, errCodeScopeUnavailable,
			"run %s cannot be bound to the birth this request expected: %v", a.runID, err)
		return nil, false
	}
	return bound, true
}

// expect is the admitted origin as a run service carries it when the action
// continues an execution or reads other runs (runview.ActionOrigin): the
// service verifies the run it loads and binds only its named store-only
// writes. An unchecked action carries the zero value.
func (a actionOrigin) expect() runview.ActionOrigin {
	if !a.checked {
		return runview.ActionOrigin{}
	}
	return runview.ExpectRunOrigin(a.origin)
}

// actionExpectation is expect for a handler whose service binds writes of
// the addressed run to it: a store that may drop the context is refused
// before the action runs, 503 scope_unavailable, as actionContext refuses it.
func (s *Server) actionExpectation(w http.ResponseWriter, r *http.Request, a actionOrigin) (runview.ActionOrigin, bool) {
	if _, ok := s.actionContext(w, r, a); !ok {
		return runview.ActionOrigin{}, false
	}
	return a.expect(), true
}

// writeActionFailure answers a checked action that failed, whatever the
// error: it reads the addressed run's current origin, unbound, and answers
// 409 run_origin_lost naming the run only when that run is gone (deleted,
// absent) or holds another birth than the admitted one. A birth never comes
// back once replaced, so the verdict is a fact read from the run itself,
// never inferred from the error: another run's absence (a body-carried run,
// a projection's source) leaves the addressed run intact, and the handler's
// own mapping answers. It reports whether it answered; an unchecked action,
// or a confirm read that fails otherwise, leaves the answer to the caller.
func (s *Server) writeActionFailure(w http.ResponseWriter, r *http.Request, a actionOrigin, err error) bool {
	return s.answerActionFailure(w, r, a, err, true)
}

// writeDeleteFailure is writeActionFailure for a delete. A delete the store
// refused on a lost origin (another birth, a deletion before it, an absence)
// answers by the rule. Any other failure came past the store's bound check:
// the delete may have removed the admitted run part-way, so a re-read that
// finds the run gone reads this request's own deletion — only another birth
// is then a lost origin, and the store's error is the answer otherwise.
func (s *Server) writeDeleteFailure(w http.ResponseWriter, r *http.Request, a actionOrigin, err error) bool {
	return s.answerActionFailure(w, r, a, err, runOriginLost(err))
}

// answerActionFailure is the failure rule; goneIsLost says whether a re-read
// finding the run gone is a lost origin.
func (s *Server) answerActionFailure(w http.ResponseWriter, r *http.Request, a actionOrigin, err error, goneIsLost bool) bool {
	if !a.checked || err == nil || s.runs == nil {
		return false
	}
	admitted := a.origin.Header().BirthID
	current, rerr := s.runs.CaptureRunOrigin(r.Context(), a.runID)
	now := ""
	switch {
	case rerr == nil && current.Header().BirthID == admitted:
		return false
	case rerr == nil:
		now = "birth " + quoteBirth(current.Header().BirthID)
	case runOriginLost(rerr) && goneIsLost:
		now = "gone (" + rerr.Error() + ")"
	case runOriginLost(rerr):
		return false
	default:
		s.logWarn("server: %s %s: run %s failed (%v) and its origin could not be read again: %v", r.Method, r.URL.Path, a.runID, err, rerr)
		return false
	}
	s.logWarn("server: %s %s: run %s admitted at birth %s is now %s — answering %s (action error: %v)",
		r.Method, r.URL.Path, a.runID, quoteBirth(admitted), now, wsErrRunOriginLost, err)
	s.writeRunOriginLost(w, r, a.runID)
	return true
}

// quoteBirth renders a birth for a log line, the historical one by name.
func quoteBirth(birth string) string {
	if birth == "" {
		return expectedBirthHistorical
	}
	return `"` + birth + `"`
}
