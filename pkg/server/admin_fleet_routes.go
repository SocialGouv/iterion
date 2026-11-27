package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/queue"
)

// The fleet registry, read back: every live server and runner record, and
// the engine floor those records produce — the number a bot push requiring
// `requires.iterion` is held against (engine_floor.go). A read model over the
// registry, never a second source of truth.
//
// Super-admin only: the records name every pod of the deployment.
func (s *Server) registerAdminFleetRoutes() {
	if s.authSvc == nil || s.fleet == nil {
		return
	}
	s.mux.Handle("GET /api/admin/fleet", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminFleet)))
}

// adminFleetView is the GET's answer.
type adminFleetView struct {
	Members []queue.FleetMember `json:"members"`
	Floor   adminFleetFloorView `json:"floor"`
}

// adminFleetFloorView is the engine floor computed from the same snapshot
// as Members, by the rule a push meets.
type adminFleetFloorView struct {
	Build     string   `json:"build"`
	Sources   []string `json:"sources"`
	Unchecked []string `json:"unchecked,omitempty"`
}

func (s *Server) handleAdminFleet(w http.ResponseWriter, r *http.Request) {
	members, err := s.fleet.FleetMembers(r.Context())
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "read the fleet registry: %s", err.Error())
		return
	}
	if members == nil {
		members = []queue.FleetMember{}
	}
	floor := floorOverFleet(serverBuild(), s.cfg.RunnerEpoch, members)
	writeJSON(w, adminFleetView{
		Members: members,
		Floor: adminFleetFloorView{
			Build:     floor.build,
			Sources:   floor.sources,
			Unchecked: floor.unchecked,
		},
	})
}
