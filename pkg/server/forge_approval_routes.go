package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Org-side ex-ante validation of repo-bot provisioning
// (Org.RequireProvisionApproval): a team admin's enable/update request is
// parked as a forge.ProvisionApproval — with ZERO forge-side surface
// created — until an org admin approves (replays the exact request through
// the orchestrator) or rejects (deletes it). Org admins and super-admins
// are never gated: they already hold the approval right.
func (s *Server) registerForgeApprovalRoutes() {
	s.mux.Handle("GET /api/orgs/{id}/provision-approvals", s.requireAuth(http.HandlerFunc(s.handleListOrgProvisionApprovals)))
	s.mux.Handle("POST /api/orgs/{id}/provision-approvals/{approval_id}/approve", s.requireAuth(http.HandlerFunc(s.handleApproveProvision)))
	s.mux.Handle("POST /api/orgs/{id}/provision-approvals/{approval_id}/reject", s.requireAuth(http.HandlerFunc(s.handleRejectProvision)))
	s.mux.Handle("GET /api/teams/{id}/provision-approvals", s.requireAuth(http.HandlerFunc(s.handleListTeamProvisionApprovals)))
}

// provisionOrgRequiringApproval returns the parent org's id when THIS
// caller's provisioning on THIS team must go through the approval queue:
// the approval store is wired, the org opted in, and the caller does not
// already hold the org-admin approval right. Empty string = provision
// directly.
//
// FAIL CLOSED: a store error reading the team or the org is returned as
// an error, never as "no approval needed" — otherwise a transient Mongo
// blip would silently bypass Org.RequireProvisionApproval. A team with no
// parent org (local mode, legacy rows) legitimately has no approval
// authority and provisions directly.
func (s *Server) provisionOrgRequiringApproval(ctx context.Context, id auth.Identity, teamID string) (string, error) {
	if s.provisionApprovals == nil || s.authSvc == nil {
		return "", nil
	}
	t, err := s.authStore().GetTeam(ctx, teamID)
	if err != nil {
		return "", fmt.Errorf("resolve team %s for the provision-approval gate: %w", teamID, err)
	}
	if t.OrgID == "" {
		return "", nil
	}
	o, err := s.authStore().GetOrg(ctx, t.OrgID)
	if err != nil {
		return "", fmt.Errorf("resolve org %s for the provision-approval gate: %w", t.OrgID, err)
	}
	if !o.RequireProvisionApproval {
		return "", nil
	}
	if s.canManageOrg(ctx, id, t.OrgID) {
		return "", nil // org admin / super-admin: auto-approved by right
	}
	return t.OrgID, nil
}

// parkProvisionRequest records the pending request and answers the team
// admin with 202 + the approval id. The caller has already validated the
// request exactly as the direct path would. baseBotIDs snapshots the
// integration's live bot set for update requests (nil for new repos).
func (s *Server) parkProvisionRequest(w http.ResponseWriter, r *http.Request, id auth.Identity, orgID, teamID string, req forgeEnableReq, integrationID string, replace bool, baseBotIDs []string) {
	a := forge.ProvisionApproval{
		ID:             uuid.NewString(),
		OrgID:          orgID,
		TenantID:       teamID,
		ConnectionID:   req.ConnectionID,
		RepoFullName:   req.Repo,
		BotIDs:         req.BotIDs,
		IntegrationID:  integrationID,
		Replace:        replace,
		BaseBotIDs:     baseBotIDs,
		ScheduleCrons:  req.ScheduleCrons,
		LaunchVars:     req.LaunchVars,
		Overlap:        req.Overlap,
		AutoFix:        req.AutoFixOnGateFailure,
		HoldLabels:     req.HoldLabels,
		LabelAllowlist: req.LabelAllowlist,
		RequestedBy:    id.UserID,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.provisionApprovals.Create(r.Context(), a); err != nil {
		httpError(w, http.StatusConflict, "%s", err.Error())
		return
	}
	// Dual-write, like approve/reject: the team trail carries the request
	// for its members, and the ORG trail is what the approver reads — a
	// request only visible team-side never surfaces in Org → Audit.
	s.auditTenant(r, teamID, "forge.provision.approval_requested", "provision_approval", a.ID, map[string]any{
		"repo": a.RepoFullName, "bots": a.BotIDs, "org_id": orgID,
	})
	s.auditOrg(r, orgID, "forge.provision.approval_requested", "provision_approval", a.ID, map[string]any{
		"repo": a.RepoFullName, "bots": a.BotIDs, "team_id": teamID,
	})
	writeJSONStatus(w, http.StatusAccepted, map[string]any{
		"pending_approval": true,
		"approval_id":      a.ID,
		"detail":           "this org requires an org admin to approve repo provisioning; the request is queued and nothing was created on the forge yet",
	})
}

func (s *Server) handleListOrgProvisionApprovals(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	if s.provisionApprovals == nil {
		writeJSON(w, struct {
			Approvals []forge.ProvisionApproval `json:"approvals"`
		}{Approvals: []forge.ProvisionApproval{}})
		return
	}
	list, err := s.provisionApprovals.ListByOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.ProvisionApproval{}
	}
	writeJSON(w, struct {
		Approvals []forge.ProvisionApproval `json:"approvals"`
	}{Approvals: list})
}

func (s *Server) handleListTeamProvisionApprovals(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	if s.provisionApprovals == nil {
		writeJSON(w, struct {
			Approvals []forge.ProvisionApproval `json:"approvals"`
		}{Approvals: []forge.ProvisionApproval{}})
		return
	}
	list, err := s.provisionApprovals.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.ProvisionApproval{}
	}
	writeJSON(w, struct {
		Approvals []forge.ProvisionApproval `json:"approvals"`
	}{Approvals: list})
}

// approvalForOrg loads the record and confirms it belongs to the route org.
func (s *Server) approvalForOrg(w http.ResponseWriter, r *http.Request, orgID, approvalID string) (forge.ProvisionApproval, bool) {
	if s.provisionApprovals == nil {
		httpError(w, http.StatusNotFound, "approval not found")
		return forge.ProvisionApproval{}, false
	}
	a, err := s.provisionApprovals.Get(r.Context(), approvalID)
	if err != nil || a.OrgID != orgID {
		httpError(w, http.StatusNotFound, "approval not found")
		return forge.ProvisionApproval{}, false
	}
	return a, true
}

func (s *Server) handleApproveProvision(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	a, ok := s.approvalForOrg(w, r, orgID, r.PathValue("approval_id"))
	if !ok {
		return
	}
	if s.forgeOrchestrator == nil {
		httpError(w, http.StatusServiceUnavailable, "forge orchestrator not configured")
		return
	}
	// Staleness chokepoint: the record replays a request captured earlier,
	// and the team may have moved since. Approving must never resurrect
	// state the team tore down — verify the referenced integration (for an
	// update request) and the connection still exist, and answer 409 with
	// the reason so the admin rejects the stale record instead of
	// re-provisioning it by surprise.
	ctx := store.WithTenant(r.Context(), a.TenantID)
	if a.IntegrationID != "" && s.forgeIntegrations != nil {
		ri, err := s.forgeIntegrations.Get(ctx, a.IntegrationID)
		if err != nil || ri.TenantID != a.TenantID {
			httpError(w, http.StatusConflict,
				"the integration this request updates no longer exists (the team removed it) — reject the request instead of approving it")
			return
		}
		// A PARTIAL teardown counts too: replaying the recorded bot set over
		// an integration whose bots changed since park time would silently
		// re-add whatever the team removed meanwhile.
		if !equalStringSets(ri.BotIDs, a.BaseBotIDs) {
			httpError(w, http.StatusConflict,
				"the integration's bot set changed since this request was parked — reject it and have the team re-submit against the current state")
			return
		}
	}
	// A NEW-repo request whose repo got provisioned meanwhile (e.g. by an
	// org admin directly) must not silently REPLACE that integration.
	if a.IntegrationID == "" && s.forgeIntegrations != nil {
		if _, err := s.forgeIntegrations.GetByConnRepo(ctx, a.TenantID, a.ConnectionID, a.RepoFullName); err == nil {
			httpError(w, http.StatusConflict,
				"this repo was provisioned after the request was parked — reject the request; the team can submit an update against the existing integration")
			return
		}
	}
	if s.forgeConnections != nil {
		if conn, err := s.forgeConnections.Get(ctx, a.ConnectionID); err != nil || conn.TenantID != a.TenantID {
			httpError(w, http.StatusConflict,
				"the forge connection this request uses no longer exists — reject the request instead of approving it")
			return
		}
	}
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:       a.TenantID,
		ConnectionID:   a.ConnectionID,
		RepoFullName:   a.RepoFullName,
		BotIDs:         a.BotIDs,
		ScheduleCrons:  a.ScheduleCrons,
		LaunchVars:     a.LaunchVars,
		Overlap:        a.Overlap,
		AutoFix:        a.AutoFix,
		HoldLabels:     a.HoldLabels,
		LabelAllowlist: a.LabelAllowlist,
		ActorID:        id.UserID,
		Replace:        a.Replace,
	})
	if err != nil {
		// The request stays pending: a transient forge failure must not
		// silently consume the approval — the admin retries or rejects.
		s.writeForgeProvisionError(w, err)
		return
	}
	if derr := s.provisionApprovals.Delete(r.Context(), a.ID); derr != nil && !errors.Is(derr, forge.ErrProvisionApprovalNotFound) && s.logger != nil {
		s.logger.Warn("forge: provision approval %s executed but not deleted: %v", a.ID, derr)
	}
	s.auditOrg(r, orgID, "forge.provision.approved", "provision_approval", a.ID, map[string]any{
		"repo": a.RepoFullName, "bots": res.BotIDs, "team_id": a.TenantID, "requested_by": a.RequestedBy,
	})
	s.auditTenant(r, a.TenantID, "forge.integration.provisioned", "forge_integration", res.IntegrationID, map[string]any{
		"repo": a.RepoFullName, "bots": res.BotIDs, "connection_id": a.ConnectionID, "approved_by": id.UserID,
	})
	writeJSON(w, res)
}

// equalStringSets compares two string slices as SETS (order-insensitive,
// duplicates collapsed).
func equalStringSets(a, b []string) bool {
	as := make(map[string]bool, len(a))
	for _, s := range a {
		as[s] = true
	}
	bs := make(map[string]bool, len(b))
	for _, s := range b {
		bs[s] = true
	}
	if len(as) != len(bs) {
		return false
	}
	for s := range as {
		if !bs[s] {
			return false
		}
	}
	return true
}

func (s *Server) handleRejectProvision(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	a, ok := s.approvalForOrg(w, r, orgID, r.PathValue("approval_id"))
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason,omitempty"`
	}
	// Body is optional on reject.
	_ = decodeJSONOptional(r, &req)
	if err := s.provisionApprovals.Delete(r.Context(), a.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "forge.provision.rejected", "provision_approval", a.ID, map[string]any{
		"repo": a.RepoFullName, "bots": a.BotIDs, "team_id": a.TenantID,
		"requested_by": a.RequestedBy, "reason": req.Reason,
	})
	w.WriteHeader(http.StatusNoContent)
}
