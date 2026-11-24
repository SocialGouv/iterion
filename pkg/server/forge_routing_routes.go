package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The forge routing surfaces of ADR-172: the per-integration, per-lane switch
// that puts a lane on the subscription spine (or back on legacy), and the
// on-demand projection of an integration's subscriptions.

func (s *Server) registerForgeRoutingRoutes() {
	s.mux.Handle("PUT /api/teams/{id}/forge/repo-bots/{integration_id}/routing", s.requireAuth(http.HandlerFunc(s.handleSetForgeRouting)))
	s.mux.Handle("POST /api/teams/{id}/forge/repo-bots/{integration_id}/subscriptions/sync", s.requireAuth(http.HandlerFunc(s.handleSyncForgeSubscriptions)))
}

type forgeRoutingReq struct {
	Lane string `json:"lane"`
	Mode string `json:"mode"`
	// Force is the escape hatch of the shadow-parity gate (P2). It overrides
	// no refusal the switch makes today, and never the org's approval gate.
	Force bool `json:"force,omitempty"`
}

// handleSetForgeRouting flips one lane of an integration between legacy,
// shadow and spine. A flip TO the spine widens what automation does on the
// repository — every bot whose subscription claims an event starts, where
// legacy starts one — so it answers to the org's approval gate like a
// provisioning that adds a bot. The approval record has no seat for a routing
// switch yet, so a caller the gate would park is refused 409
// approval_required, naming the org: an org admin makes the flip. A flip back
// to legacy never asks the gate.
func (s *Server) handleSetForgeRouting(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeOrchestrator == nil || s.forgeIntegrations == nil {
		httpError(w, http.StatusServiceUnavailable, "forge provisioning unavailable")
		return
	}
	integrationID := r.PathValue("integration_id")
	ri, err := s.forgeIntegrations.Get(r.Context(), integrationID)
	if err != nil || ri.TenantID != teamID {
		httpError(w, http.StatusNotFound, "integration not found")
		return
	}
	var req forgeRoutingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if forge.RoutingSwitchExpandsSurface(ri.ForgeRouting, req.Lane, req.Mode) {
		orgID, gerr := s.provisionOrgRequiringApproval(r.Context(), id, teamID, ri.Pool != nil && ri.Pool.Enabled)
		if gerr != nil {
			httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", gerr)
			return
		}
		if orgID != "" {
			writeJSONStatus(w, http.StatusConflict, map[string]string{
				"error":  "approval_required",
				"org_id": orgID,
				"reason": "putting a lane on the spine widens what automation starts on this repository, and the org requires approval for that: an org admin makes this flip",
			})
			return
		}
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.SetForgeRouting(ctx, teamID, integrationID, req.Lane, req.Mode, req.Force)
	if err != nil {
		s.writeForgeRoutingError(w, r, err)
		return
	}
	s.auditTenant(r, teamID, "forge.integration.routing_set", "forge_integration", integrationID,
		map[string]any{"lane": res.Lane, "mode": res.Mode, "force": req.Force})
	writeJSON(w, res)
}

// handleSyncForgeSubscriptions projects an integration's subscriptions now,
// under its provision fence, and answers the report: the enabled and disabled
// rows per bot with their labels and reasons, the warnings, the digest, any
// hand-pinned launch_vars keys.
func (s *Server) handleSyncForgeSubscriptions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeOrchestrator == nil {
		httpError(w, http.StatusServiceUnavailable, "forge provisioning unavailable")
		return
	}
	integrationID := r.PathValue("integration_id")
	ctx := store.WithTenant(r.Context(), teamID)
	report, err := s.forgeOrchestrator.SyncSubscriptions(ctx, teamID, integrationID)
	if err != nil {
		s.writeForgeRoutingError(w, r, err)
		return
	}
	s.auditTenant(r, teamID, "forge.integration.subscriptions_synced", "forge_integration", integrationID, nil)
	writeJSON(w, report)
}

// writeForgeRoutingError answers the routing switch's and the sync's typed
// errors with their own codes; anything else is the provisioning layer's
// answer.
func (s *Server) writeForgeRoutingError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *forge.RoutingRefusedError
	var invalid *forge.RoutingInvalidError
	switch {
	case errors.As(err, &refused):
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":  refused.Code,
			"reason": refused.Reason,
			"report": refused.Report,
		})
	case errors.As(err, &invalid):
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_" + invalid.Field,
			"value":   invalid.Value,
			"allowed": invalid.Allowed,
		})
	case errors.Is(err, forge.ErrIntegrationNotFound):
		httpError(w, http.StatusNotFound, "integration not found")
	case errors.Is(err, forge.ErrSubscriptionProjection):
		// A refused projection on the sync route (overlapping rows, a row
		// with nothing to bind to, no projector wired): the operator's to
		// fix, never a server fault.
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "projection_refused", "reason": err.Error()})
	default:
		s.writeForgeProvisionError(w, r, err)
	}
}

// forgeSubscriptionProjector is the projector the orchestrator calls under an
// integration's provision fence, or nil when the server holds no trigger
// store. Its roles resolve through forgeRoleBots, the one adapter the router's
// staleness check reads too.
func (s *Server) forgeSubscriptionProjector(st trigger.SubscriptionStore) forge.SubscriptionProjector {
	if st == nil {
		return nil
	}
	return forgetrigger.Projector{
		Store: st,
		Roles: func(context.Context) forgetrigger.RoleBots { return s.forgeRoleBots() },
		Clock: time.Now,
	}
}

// forgeRoleBots adapts the deployment's role bots to the planner's shape: the
// projector and the router's digest check both read them here.
func (s *Server) forgeRoleBots() forgetrigger.RoleBots {
	roles := s.roleBots()
	return forgetrigger.RoleBots{Implementer: roles.Implementer, Reviewer: roles.Reviewer}
}
