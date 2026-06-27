package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// registerCloudBoardRoutes mounts the kanban board REST surface in CLOUD
// mode, where there is no single filesystem board — each team has its own
// Mongo-backed board (boardmongo, via cfg.CloudBoardFor). It reuses the very
// same prefix the self-hosted mount uses (/api/v1/native) and the same
// BoardAPI handlers, but resolves the store PER REQUEST from the caller's
// ACTIVE TEAM (the team stamped on the JWT). So the studio board view works
// unchanged in cloud — it just sees the active team's board — and switching
// the active team switches the board. Board membership is implicit: a user
// only ever resolves their own active team's board.
//
// Self-hosted mode (NativeTrackerStore != nil) keeps the single-store mount;
// the two are mutually exclusive, so there is no route conflict.
func (s *Server) registerCloudBoardRoutes() {
	api := &native.BoardAPI{Resolve: s.cloudBoardResolve}
	api.RegisterRoutesWithMiddleware(s.mux, "/api/v1/native", s.requireAuth)
}

// cloudBoardResolve returns the active team's board store for this request.
// A request with no active team (a brand-new account not yet in a team)
// resolves to a nil store, which BoardAPI renders as 404 "board not
// available" rather than erroring.
func (s *Server) cloudBoardResolve(r *http.Request) (native.BoardStore, error) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		return nil, nil
	}
	return s.cfg.CloudBoardFor(id.TeamID), nil
}
