package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

const denyMaintenanceFence = "maintenance_fence"

// Read through on every admission: a TTL/stale-on-error resolver is unsuitable
// for a stop-new-launches switch. No store means the unchanged local default.
func (s *Server) maintenance(ctx context.Context) (platformcfg.Maintenance, error) {
	if s.maintenanceStore == nil {
		return platformcfg.Maintenance{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, platformcfg.FetchTimeout)
	defer cancel()
	rec, err := s.maintenanceStore.Get(ctx)
	if err != nil {
		return platformcfg.Maintenance{}, err
	}
	if rec == nil {
		return platformcfg.Maintenance{}, nil
	}
	return *rec, nil
}

func (s *Server) gateMaintenance(ctx context.Context, subj launchSubject) *launchDenial {
	id, _ := auth.FromContext(ctx)
	if subj.ExistingRun || id.IsSuperAdmin {
		return nil
	}
	rec, err := s.maintenance(ctx)
	if err != nil {
		return &launchDenial{status: http.StatusServiceUnavailable, reason: denyMaintenanceFence,
			detail: "maintenance.fence_launches could not be read; retry after the maintenance store recovers"}
	}
	if !rec.FenceLaunches {
		return nil
	}
	return &launchDenial{status: http.StatusServiceUnavailable, reason: denyMaintenanceFence,
		detail: fmt.Sprintf("maintenance.fence_launches is armed since %s: %s", rec.UpdatedAt.UTC().Format(time.RFC3339Nano), rec.Reason)}
}

func (s *Server) handleGetMaintenance(w http.ResponseWriter, r *http.Request) {
	rec, err := s.maintenance(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "read maintenance.fence_launches: %v", err)
		return
	}
	s.writeJSONFor(w, r, rec)
}

type maintenanceRequest struct {
	FenceLaunches *bool  `json:"fence_launches"`
	Reason        string `json:"reason"`
}

// Full replacement, with a mandatory explicit boolean and dated audit reason.
// The store owns the timestamp; a client cannot backdate an armed fence.
func (s *Server) handlePutMaintenance(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req maintenanceRequest
	if err := readJSONStrict(r, &req); err != nil || req.FenceLaunches == nil || strings.TrimSpace(req.Reason) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "maintenance requires fence_launches (boolean) and a nonempty reason")
		return
	}
	id, _ := auth.FromContext(r.Context())
	rec := platformcfg.Maintenance{FenceLaunches: *req.FenceLaunches, Reason: strings.TrimSpace(req.Reason), UpdatedBy: id.UserID}
	ctx, cancel := context.WithTimeout(r.Context(), platformcfg.FetchTimeout)
	defer cancel()
	if err := s.maintenanceStore.Put(ctx, rec); err != nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "write maintenance.fence_launches: %v", err)
		return
	}
	s.auditPlatform(r, "", "platform.settings.maintenance.updated", "platform_settings", platformcfg.FamilyMaintenance,
		map[string]any{"fence_launches": rec.FenceLaunches, "reason": rec.Reason})
	s.handleGetMaintenance(w, r)
}
