package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// registerConnectorDeadLetterRoutes wires the connector intake's dead-letter
// view and its replays (ADR-168 §8.4), on the trigger family's scope and
// auth: the request's team (the local scope locally), requireAuth on every
// route and the member rung on a replay.
func (s *Server) registerConnectorDeadLetterRoutes() {
	s.mux.Handle("GET /api/v1/intakes", s.triggerWriteAuth(http.HandlerFunc(s.handleListIntakes)))
	s.mux.Handle("GET /api/v1/intakes/{id}", s.triggerWriteAuth(http.HandlerFunc(s.handleGetIntake)))
	s.mux.Handle("POST /api/v1/intakes/{id}/replay", s.triggerWriteAuth(http.HandlerFunc(s.handleReplayIntake)))
	s.mux.Handle("GET /api/v1/pairs", s.triggerWriteAuth(http.HandlerFunc(s.handleListPairs)))
	s.mux.Handle("POST /api/v1/pairs/{id}/replay", s.triggerWriteAuth(http.HandlerFunc(s.handleReplayPair)))
}

// intakeTenant is the tenant a request's intakes are stored under: the
// trigger scope's connection tenant (binding.ConnectionTenant).
func (s *Server) intakeTenant(r *http.Request) string {
	return binding.ConnectionTenant(s.triggerTenant(r))
}

// deadLetterLimit reads ?limit= over a default, bounded.
func deadLetterLimit(r *http.Request) (int, error) {
	const def, max = 100, 1000
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > max {
		return 0, fmt.Errorf("limit %q: want a count from 1 to %d", v, max)
	}
	return n, nil
}

// writeIntakeErr answers an intake store error: an absent or foreign row
// is a 404, a row in no state to replay a 409.
func writeIntakeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, intake.ErrNotFound):
		dispatcher.WriteErr(w, http.StatusNotFound, err)
	case errors.Is(err, intake.ErrNotReplayable):
		dispatcher.WriteErr(w, http.StatusConflict, err)
	default:
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) handleListIntakes(w http.ResponseWriter, r *http.Request) {
	limit, err := deadLetterLimit(r)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	rows, err := s.connectorIntakes.ListIntakes(r.Context(), s.intakeTenant(r), r.URL.Query().Get("state"), limit)
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	if rows == nil {
		rows = []intake.Intake{}
	}
	dispatcher.WriteJSON(w, http.StatusOK, map[string]any{"intakes": rows})
}

func (s *Server) handleGetIntake(w http.ResponseWriter, r *http.Request) {
	in, err := s.connectorIntakes.GetIntake(r.Context(), s.intakeTenant(r), r.PathValue("id"))
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, in)
}

func (s *Server) handleReplayIntake(w http.ResponseWriter, r *http.Request) {
	in, err := s.connectorIntakes.ReplayIntake(r.Context(), s.intakeTenant(r), r.PathValue("id"), time.Now().UTC())
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, in)
}

func (s *Server) handleListPairs(w http.ResponseWriter, r *http.Request) {
	limit, err := deadLetterLimit(r)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	q := intake.PairQuery{IntakeID: r.URL.Query().Get("intake_id"), State: r.URL.Query().Get("state")}
	rows, err := s.connectorIntakes.ListPairs(r.Context(), s.intakeTenant(r), q, limit)
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	if rows == nil {
		rows = []intake.Pair{}
	}
	dispatcher.WriteJSON(w, http.StatusOK, map[string]any{"pairs": rows})
}

// handleReplayPair replays a dead or launch-refused pair into its launch
// (intake.Store.ReplayPair). A pair matching refused is not replayed into a
// launch — no filter ever matched it — but re-judged (#3296): the matcher
// reads it again against the subscription as it stands, and the pair takes
// that verdict.
func (s *Server) handleReplayPair(w http.ResponseWriter, r *http.Request) {
	tenant := s.intakeTenant(r)
	p, err := s.connectorIntakes.GetPair(r.Context(), tenant, r.PathValue("id"))
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	now := time.Now().UTC()
	if p.State != intake.PairRefused || !p.MatchRefusal {
		out, err := s.connectorIntakes.ReplayPair(r.Context(), tenant, p.ID, now)
		if err != nil {
			writeIntakeErr(w, err)
			return
		}
		dispatcher.WriteJSON(w, http.StatusOK, out)
		return
	}
	in, err := s.connectorIntakes.GetIntake(r.Context(), tenant, p.IntakeID)
	if errors.Is(err, intake.ErrNotFound) {
		dispatcher.WriteErr(w, http.StatusConflict, fmt.Errorf("pair %s cannot be re-judged: its intake is gone (expired or removed)", p.ID))
		return
	}
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	var v intake.Verdict
	sub, err := s.cfg.TriggerStore.Get(r.Context(), p.SubscriptionID)
	switch {
	case errors.Is(err, trigger.ErrSubscriptionNotFound) || (err == nil && sub.TenantID != binding.SubscriptionTenant(tenant)):
		v = intake.Verdict{SubscriptionID: p.SubscriptionID, Refusal: "its subscription was deleted"}
	case err != nil:
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	default:
		judged, _, err := s.connectorMatcher().Rejudge(r.Context(), in, sub)
		if err != nil {
			dispatcher.WriteErr(w, http.StatusServiceUnavailable, fmt.Errorf("re-judge pair %s: %w", p.ID, err))
			return
		}
		v = intake.Verdict{SubscriptionID: p.SubscriptionID, Matched: judged.Matched, Reason: judged.Reason, Refusal: judged.Refusal}
	}
	out, err := s.connectorIntakes.RejudgePair(r.Context(), tenant, p.ID, v, now)
	if err != nil {
		writeIntakeErr(w, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, out)
}
