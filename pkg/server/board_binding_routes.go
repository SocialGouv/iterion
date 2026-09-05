package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The team ⇄ project-board binding endpoints (ADR-097 §4).
//
// GET is a member read; PUT and DELETE need team-manage rights: binding a
// board redirects where a team's work is projected, and rebinding it silently
// would be a way to point somebody else's roadmap at your own board.

// registerBoardBindingRoutes mounts the endpoints. Like every other forge
// surface they self-disable when their stores are absent (local mode), so a
// self-hosted studio simply does not advertise them.
func (s *Server) registerBoardBindingRoutes() {
	if s.boardBindings == nil {
		return
	}
	s.mux.Handle("GET /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handleGetBoardBinding)))
	s.mux.Handle("PUT /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handlePutBoardBinding)))
	s.mux.Handle("DELETE /api/teams/{id}/board-binding", s.requireAuth(http.HandlerFunc(s.handleDeleteBoardBinding)))
}

// boardBindingReq is the PUT payload: the board's ADDRESS plus the policy.
// Never the resolved ids — a caller that could name a project id could point a
// team at a board it has no rights on, so every id in the stored binding comes
// from reading the board with the team's own credential.
type boardBindingReq struct {
	Owner        string `json:"owner"`
	OwnerKind    string `json:"owner_kind,omitempty"` // org (default) | user
	Number       int    `json:"number"`
	ConnectionID string `json:"connection_id"`
	// StatusMap overrides the shipped five-column vocabulary
	// (`{"Todo": "ready"}`). Must be injective.
	StatusMap map[string]string `json:"status_map,omitempty"`
	// SyncEverySeconds is the reconciliation interval. Absent = the default;
	// an explicit 0 = off. A pointer, because those are different answers.
	SyncEverySeconds *int64 `json:"sync_every_seconds,omitempty"`
}

func (s *Server) handleGetBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	b, err := s.boardBindings.GetByTenant(r.Context(), teamID)
	if err != nil {
		if errors.Is(err, forge.ErrBoardBindingNotFound) {
			httpError(w, http.StatusNotFound, "no project board bound to this team")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, b)
}

func (s *Server) handlePutBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req boardBindingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	bind, err := s.bindTeamBoard(r.Context(), teamID, req)
	if err != nil {
		// Every failure here is the caller's request or their board: a bad
		// ref, a non-injective map, an interval under the floor, a project the
		// credential cannot see. 400 with the reason beats a 500 that hides
		// which of those it was.
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.boardBindings.Upsert(r.Context(), bind); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	stored, err := s.boardBindings.GetByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, stored)
}

func (s *Server) handleDeleteBoardBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if err := s.boardBindings.Delete(r.Context(), teamID); err != nil {
		if errors.Is(err, forge.ErrBoardBindingNotFound) {
			httpError(w, http.StatusNotFound, "no project board bound to this team")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bindTeamBoard resolves a request into a binding by READING the board with
// the connection's credential. It is the shared body behind the HTTP handler
// and any other caller (the remote CLI goes through the HTTP one).
func (s *Server) bindTeamBoard(ctx context.Context, teamID string, req boardBindingReq) (forge.BoardBinding, error) {
	bc, provider, err := s.boardClientFor(ctx, req.ConnectionID)
	if err != nil {
		return forge.BoardBinding{}, err
	}
	bind := forge.BindRequest{
		TenantID: teamID,
		Provider: provider,
		Ref: forge.ProjectRef{
			Owner:     req.Owner,
			OwnerKind: forge.ProjectOwnerKind(req.OwnerKind),
			Number:    req.Number,
		},
		ConnectionID: req.ConnectionID,
		StatusMap:    req.StatusMap,
	}
	if req.SyncEverySeconds != nil {
		d := time.Duration(*req.SyncEverySeconds) * time.Second
		bind.SyncEvery = &d
	}
	return forge.BindBoard(ctx, bc, bind)
}

// boardClientFor resolves a forge connection into a project-board client. The
// indirection through a field is what lets a test inject a fake without a
// connection store, and what keeps the cloud/local difference in ONE place.
func (s *Server) boardClientFor(ctx context.Context, connID string) (forge.BoardClient, forge.Provider, error) {
	if s.boardClientForConnection != nil {
		return s.boardClientForConnection(ctx, connID)
	}
	if s.forgeConnections == nil {
		return nil, "", errors.New("forge connections are not configured on this instance")
	}
	conn, err := s.forgeConnections.Get(ctx, connID)
	if err != nil {
		return nil, "", err
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, "", err
	}
	bc, ok := forge.AsBoardClient(admin)
	if !ok {
		return nil, "", errors.New("this connection's provider exposes no project board")
	}
	return bc, conn.Provider, nil
}

// boardClientForBoundBinding resolves the board client a bound team's sync
// pass writes through — the worker's BoardClientFor.
func (s *Server) boardClientForBoundBinding(ctx context.Context, b forge.BoardBinding) (forge.BoardClient, error) {
	if s.boardClientForBinding != nil {
		return s.boardClientForBinding(ctx, b)
	}
	bc, _, err := s.boardClientFor(ctx, b.ConnectionID)
	return bc, err
}

// startBoardSync launches the periodic reconciliation worker (ADR-097 §10).
// It needs a binding store AND a per-tenant card store; without either there
// is nothing to reconcile, and saying so once at boot beats a worker that
// ticks forever over an empty set.
func (s *Server) startBoardSync() {
	if s == nil || s.boardBindings == nil || s.cfg.CloudBoardFor == nil {
		return
	}
	w := &BoardSyncWorker{
		Bindings:       s.boardBindings,
		BoardClientFor: s.boardClientForBoundBinding,
		CardsFor: func(_ context.Context, tenantID string) (native.BoardStore, error) {
			b := s.cfg.CloudBoardFor(tenantID)
			if b == nil {
				return nil, errors.New("no card store for this team")
			}
			return b, nil
		},
		Logger: s.logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.boardSyncCancel = cancel
	errtrack.Go("server.boardSync", func() { w.Run(ctx) })
	if s.logger != nil {
		s.logger.Info("server: project-board reconciliation started (per-team interval, elected per tenant)")
	}
}
