package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Live-run steering routes: POST /api/runs/{id}/bump-loop,
// /raise-budget and /answer-human. The HTTP layer maps the service's
// truthful contract onto status codes:
//
//	400 — unknown loop / structurally invalid command
//	404 — run not found (or cross-tenant)
//	409 — terminal run, run not held anywhere reachable, no budget block
//	202 — command delivered but the run is busy in a long node (it WILL
//	      apply at the next boundary)
//	504 — cross-process runner did not answer in time
//	200 — applied or an honest noop (body says which)

type bumpLoopHTTPRequest struct {
	LoopName string `json:"loop_name"`
	Delta    int    `json:"delta"`
}

type raiseBudgetHTTPRequest struct {
	Budget struct {
		MaxCostUSD    float64 `json:"max_cost_usd,omitempty"`
		MaxTokens     int     `json:"max_tokens,omitempty"`
		MaxIterations int     `json:"max_iterations,omitempty"`
		MaxDuration   string  `json:"max_duration,omitempty"`
	} `json:"budget"`
}

type answerHumanHTTPRequest struct {
	Answers map[string]any `json:"answers"`
}

func (s *Server) handleBumpLoop(w http.ResponseWriter, r *http.Request) {
	id, ok := s.steerGate(w, r)
	if !ok {
		return
	}
	var req bumpLoopHTTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.runs.BumpLoopCtx(r.Context(), id, runview.BumpLoopRequest{
		LoopName: req.LoopName,
		Delta:    req.Delta,
		IssuedBy: steerIssuer(r.Context()),
	})
	if err != nil {
		s.writeSteerError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, res)
}

func (s *Server) handleRaiseBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.steerGate(w, r)
	if !ok {
		return
	}
	var req raiseBudgetHTTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.runs.RaiseBudgetCtx(r.Context(), id, runview.RaiseBudgetRequest{
		Budget: ir.BudgetOverrides{
			MaxCostUSD:    req.Budget.MaxCostUSD,
			MaxTokens:     req.Budget.MaxTokens,
			MaxIterations: req.Budget.MaxIterations,
			MaxDuration:   req.Budget.MaxDuration,
		},
		IssuedBy: steerIssuer(r.Context()),
	})
	if err != nil {
		s.writeSteerError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, res)
}

type answerInteractionHTTPRequest struct {
	Answer string `json:"answer"`
}

// handleAnswerInteraction answers a pending ASYNC question (ADR-081) —
// valid while the run is running OR paused. 404 unknown interaction,
// 409 non-async or already answered, 200 with {queued, resumed}.
func (s *Server) handleAnswerInteraction(w http.ResponseWriter, r *http.Request) {
	id, ok := s.steerGate(w, r)
	if !ok {
		return
	}
	iid := r.PathValue("iid")
	if iid == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing interaction id")
		return
	}
	var req answerInteractionHTTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Answer == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "answer is required")
		return
	}
	res, err := s.runs.AnswerInteractionCtx(r.Context(), id, iid, req.Answer)
	if err != nil {
		switch {
		case errors.Is(err, runview.ErrInteractionNotFound):
			s.httpErrorFor(w, r, http.StatusNotFound, "%s", err.Error())
		case errors.Is(err, runview.ErrInteractionNotAsync), errors.Is(err, store.ErrInteractionAlreadyAnswered):
			s.httpErrorFor(w, r, http.StatusConflict, "%s", err.Error())
		default:
			s.writeSteerError(w, r, err)
		}
		return
	}
	s.writeJSONFor(w, r, res)
}

// handleListPendingInteractions lists the run's pending async questions.
func (s *Server) handleListPendingInteractions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	pending, err := s.runs.PendingAsyncInteractions(r.Context(), id)
	if err != nil {
		s.writeSteerError(w, r, err)
		return
	}
	if pending == nil {
		pending = []*store.Interaction{}
	}
	s.writeJSONFor(w, r, map[string]any{"interactions": pending})
}

func (s *Server) handleAnswerHuman(w http.ResponseWriter, r *http.Request) {
	id, ok := s.steerGate(w, r)
	if !ok {
		return
	}
	var req answerHumanHTTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Answers) == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "answers are required")
		return
	}
	res, err := s.runs.AnswerHumanCtx(r.Context(), id, req.Answers)
	if err != nil {
		s.writeSteerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, res)
}

// steerGate factors the shared entry checks of the three steering
// handlers: safe origin, no cross-store writes, run id present.
func (s *Server) steerGate(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.requireSafeOrigin(w, r) {
		return "", false
	}
	if s.rejectCrossStoreWrite(w, r) {
		return "", false
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return "", false
	}
	return id, true
}

// steerIssuer names the authenticated principal for the persisted
// run_steered event ("" in local no-auth mode).
func steerIssuer(ctx context.Context) string {
	id, _ := auth.FromContext(ctx)
	return id.UserID
}

// ---- WS command handlers (same service calls, envelope replies) ----

type wsBumpLoopRequest struct {
	LoopName string `json:"loop_name"`
	Delta    int    `json:"delta"`
}

type wsRaiseBudgetRequest struct {
	MaxCostUSD    float64 `json:"max_cost_usd,omitempty"`
	MaxTokens     int     `json:"max_tokens,omitempty"`
	MaxIterations int     `json:"max_iterations,omitempty"`
	MaxDuration   string  `json:"max_duration,omitempty"`
}

func (c *runConn) handleBumpLoop(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "steering is not available for cross-store runs — open the owning daemon", env.AckID)
		return
	}
	var req wsBumpLoopRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	res, err := c.server.runs.BumpLoopCtx(c.authCtx(), c.runID, runview.BumpLoopRequest{
		LoopName: req.LoopName,
		Delta:    req.Delta,
		IssuedBy: c.identity.UserID,
	})
	if err != nil {
		c.sendError(steerWSCode(err), err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeAck, res, env.AckID)
}

func (c *runConn) handleRaiseBudget(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "steering is not available for cross-store runs — open the owning daemon", env.AckID)
		return
	}
	var req wsRaiseBudgetRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	res, err := c.server.runs.RaiseBudgetCtx(c.authCtx(), c.runID, runview.RaiseBudgetRequest{
		Budget: ir.BudgetOverrides{
			MaxCostUSD:    req.MaxCostUSD,
			MaxTokens:     req.MaxTokens,
			MaxIterations: req.MaxIterations,
			MaxDuration:   req.MaxDuration,
		},
		IssuedBy: c.identity.UserID,
	})
	if err != nil {
		c.sendError(steerWSCode(err), err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeAck, res, env.AckID)
}

// steerWSCode maps the steering contract onto WS error codes (the WS
// twin of writeSteerError).
func steerWSCode(err error) string {
	var (
		unknownLoop *runtime.UnknownLoopError
		terminal    *runview.RunTerminalError
		steerErr    *runview.SteerError
	)
	switch {
	case errors.As(err, &unknownLoop), errors.Is(err, runtime.ErrInvalidOverride):
		return "bad_request"
	case errors.Is(err, store.ErrRunNotFound):
		return "run_not_found"
	case errors.As(err, &terminal), errors.Is(err, runtime.ErrNoBudgetDeclared), errors.Is(err, runview.ErrRunNotHeld):
		return "not_active"
	case errors.Is(err, runview.ErrSteerPending):
		return "queued"
	case errors.As(err, &steerErr):
		return steerErr.Code
	default:
		return "steer_failed"
	}
}

// writeSteerError maps the service-layer contract onto HTTP.
func (s *Server) writeSteerError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		unknownLoop *runtime.UnknownLoopError
		terminal    *runview.RunTerminalError
		steerErr    *runview.SteerError
	)
	switch {
	case errors.As(err, &unknownLoop):
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", unknownLoop.Error())
	case errors.Is(err, runtime.ErrInvalidOverride):
		s.httpErrorFor(w, r, http.StatusBadRequest, "%s", err.Error())
	case errors.Is(err, store.ErrRunDeleted):
		s.httpErrorFor(w, r, http.StatusGone, "run was deleted")
	case errors.Is(err, store.ErrRunNotFound):
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
	case errors.As(err, &terminal):
		s.httpErrorFor(w, r, http.StatusConflict, "%s", terminal.Error())
	case errors.Is(err, runview.ErrNotAwaitingHuman):
		s.httpErrorFor(w, r, http.StatusConflict, "%s", err.Error())
	case errors.Is(err, runtime.ErrNoBudgetDeclared):
		s.httpErrorFor(w, r, http.StatusConflict, "%s", err.Error())
	case errors.Is(err, runview.ErrRunNotHeld):
		s.httpErrorFor(w, r, http.StatusConflict, "%s", err.Error())
	case errors.Is(err, runview.ErrSteerPending):
		// Delivered but unacked: the command applies at the run's next
		// safe boundary. 202 keeps the caller honest about the timing.
		w.WriteHeader(http.StatusAccepted)
		s.writeJSONFor(w, r, map[string]any{"status": "queued", "detail": err.Error()})
	case errors.As(err, &steerErr):
		// Cross-process reply carrying its own code.
		switch steerErr.Code {
		case "unknown_loop", "invalid":
			s.httpErrorFor(w, r, http.StatusBadRequest, "%s", steerErr.Message)
		case "no_budget", "terminal", "not_active":
			s.httpErrorFor(w, r, http.StatusConflict, "%s", steerErr.Message)
		case "engine_stalled":
			s.httpErrorFor(w, r, http.StatusGatewayTimeout, "%s", steerErr.Message)
		default:
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%s", steerErr.Message)
		}
	default:
		s.httpErrorFor(w, r, http.StatusInternalServerError, "steer: %v", err)
	}
}
