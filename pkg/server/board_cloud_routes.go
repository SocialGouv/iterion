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
	api.RegisterRoutesWithMiddleware(s.mux.ServeMux, "/api/v1/native", s.boardWriteAuth)
}

// mutatingRungAuth is the ONE definition of the member rung as HTTP
// middleware, and the two wrappers below are its only instances: a mount
// wraps reads as well as writes, and on a read it is inert — requireAuth,
// then, on a state-changing method only, the member rung the mutation takes.
// Its two instances guard the surfaces that FILL a queue a machine
// principal drains: a board's Ready column (the dispatcher, boarddispatch.go)
// and a trigger subscription (the spine's serviceLauncher, which launches
// under machineLaunchCtx). Both drains are exempt from the rung by kind, so
// the authorization is owed where the queue is filled, not where the daemon
// launches it. The detail is the refused caller's one sentence, so each
// family says what the caller was changing.
//
// The refusal renders through writeRungDenial, not writeLaunchDenial: same
// token, status and envelope, no metric — iterion_launch_denied_total counts
// the admission gate's refusals, and a refused board write is not a refused
// launch (launch_gate.go holds both renderers side by side).
func (s *Server) mutatingRungAuth(detail string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d := s.mutatingRungDenial(r, detail); d != nil {
				s.writeRungDenial(w, r, d)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// mutatingRungDenial is the rung's decision for one request: nil means serve
// it. The predicate is gateLaunch's own (launchRungCleared), so the
// queue-filling surfaces and the launch surfaces cannot disagree about who
// may spend the org's money, and the refusal carries the same token and
// envelope a denied launch carries: member_role_required.
//
// Two conditions keep it to the cloud: no identity store means local or
// desktop mode — a single operator, no roles to read, the same arm
// gateLaunch opens with — and a method that changes nothing is a read. A
// super-admin passes, as everywhere else.
//
// No arm for a principal with no team: none reaches this decision over
// HTTP. requireAuth's choke point (stampAuthedContext, middleware.go) refuses
// a team-less credential on every route outside tenantFreePrefixes — these
// mounts included — before any wrapper runs, with its own sentence. The
// decision therefore reads the role alone.
func (s *Server) mutatingRungDenial(r *http.Request, detail string) *launchDenial {
	if s.authStore() == nil || !isStateChangingMethod(r.Method) {
		return nil
	}
	id, _ := auth.FromContext(r.Context())
	if id.IsSuperAdmin || launchRungCleared(id) {
		return nil
	}
	return &launchDenial{
		status: http.StatusForbidden,
		reason: denyMemberRoleRequired,
		detail: detail,
	}
}

// boardWriteRungDetail is what a refused board write is told.
const boardWriteRungDetail = "changing this board takes at least the member role in this workspace — its Ready column queues runs the platform launches; ask a workspace admin to raise your role"

// boardWriteAuth is the wrapper every route of the THREE board registrars
// mounts through: registerCloudBoardRoutes above, registerPipelineBoardRoutes
// (pipeline_boards.go) and registerBoardForgeRoutes (board_forge.go, the
// per-card push/pull-request/merge endpoints under the same prefix).
//
// Mounted at those three registrars rather than inside the store resolvers: a
// resolver sees a METHOD, not a route, and POST
// /api/v1/assistant/context/resolve resolves the same board store only to
// READ it — refusing a viewer there would break a surface that launches
// nothing. The class witness (board_write_rung_test.go) walks all three
// registrars' routes, so a route mounted past this wrapper turns red on the
// route that lacks it.
//
// The self-hosted single-store mount of the same prefix (server_routes.go,
// NativeTrackerStore) keeps requireAuth alone, and nothing is missing there:
// that store is only ever set by the local studio (pkg/cli), which wires no
// AuthService, so there is no identity store and no role to read — the arm
// mutatingRungDenial opens with.
func (s *Server) boardWriteAuth(next http.Handler) http.Handler {
	return s.mutatingRungAuth(boardWriteRungDetail)(next)
}

// boardWriteDenial is the board family's instance of the rung decision.
func (s *Server) boardWriteDenial(r *http.Request) *launchDenial {
	return s.mutatingRungDenial(r, boardWriteRungDetail)
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
