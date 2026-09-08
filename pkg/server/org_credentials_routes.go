package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The ORG credential tier's control surface: an org's own provider keys and
// forfait blobs, plus the audience deciding which of its teams may spend
// them. Org-admin managed, and deliberately a mirror of the team endpoints
// — the records ARE ordinary ApiKey / OAuthRecord rows, under a reserved
// scope (secrets.OrgTierTenantID / OrgTierOwnerKey), so every shared write
// path (shape gate, sealing, rotation, the refresh worker) is reused.
//
// It exists because sharing a key across an org's product teams previously
// meant copying it into each of them: N writes per rotation, N places to
// forget one, and no way to tell whose spend was whose. See
// pkg/server/cloudpublisher/org_tier.go for the resolution side.
func (s *Server) registerOrgCredentialRoutes() {
	s.mux.Handle("GET /api/orgs/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleListOrgApiKeys)))
	s.mux.Handle("POST /api/orgs/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleCreateOrgApiKey)))
	s.mux.Handle("PATCH /api/orgs/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgApiKey)))
	s.mux.Handle("DELETE /api/orgs/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgApiKey)))

	s.mux.Handle("GET /api/orgs/{id}/oauth/connections", s.requireAuth(http.HandlerFunc(s.handleOrgListOAuth)))
	s.mux.Handle("POST /api/orgs/{id}/oauth/{kind}/authorize/start", s.requireAuth(http.HandlerFunc(s.handleOrgStartOAuth)))
	s.mux.Handle("POST /api/orgs/{id}/oauth/{kind}/authorize/complete", s.requireAuth(http.HandlerFunc(s.handleOrgCompleteOAuth)))
	s.mux.Handle("POST /api/orgs/{id}/oauth/{kind}/credentials", s.requireAuth(http.HandlerFunc(s.handleOrgUploadOAuth)))
	s.mux.Handle("POST /api/orgs/{id}/oauth/{kind}/refresh", s.requireAuth(http.HandlerFunc(s.handleOrgRefreshOAuth)))
	s.mux.Handle("PATCH /api/orgs/{id}/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleOrgRenameOAuth)))
	s.mux.Handle("DELETE /api/orgs/{id}/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleOrgDeleteOAuth)))

	s.mux.Handle("GET /api/orgs/{id}/credential-audience", s.requireAuth(http.HandlerFunc(s.handleGetOrgCredentialAudience)))
	s.mux.Handle("PATCH /api/orgs/{id}/credential-audience", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgCredentialAudience)))
}

// orgCredentialScope validates the caller may administer the org's shared
// credentials and returns the reserved scope to store them under. It
// answers false having already written the error.
func (s *Server) orgCredentialScope(w http.ResponseWriter, r *http.Request, manage bool) (orgID, scope string, ok bool) {
	id, _ := auth.FromContext(r.Context())
	orgID = r.PathValue("id")
	allowed := s.canViewOrg(r.Context(), id, orgID)
	if manage {
		allowed = s.canManageOrg(r.Context(), id, orgID)
	}
	if !allowed {
		if manage {
			httpError(w, http.StatusForbidden, "org admin or owner required")
		} else {
			httpError(w, http.StatusForbidden, "not a member of this org")
		}
		return "", "", false
	}
	return orgID, secrets.OrgTierTenantID(orgID), true
}

// ---- API keys ----

func (s *Server) handleListOrgApiKeys(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := s.orgCredentialScope(w, r, false)
	if !ok {
		return
	}
	// Owner "" — an org key is never user-scoped: it is the org's, and a
	// personal key hidden inside the org tier would be spendable by teams
	// whose members cannot see it.
	keys, err := s.apiKeys.ListByTeam(store.WithTenant(r.Context(), scope), scope, "")
	s.writeApiKeyList(w, r, keys, err)
}

func (s *Server) handleCreateOrgApiKey(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.handleCreateApiKey(w, r, scope, "")
}

func (s *Server) handleUpdateOrgApiKey(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.handleUpdateApiKeyIn(w, r, scope)
}

func (s *Server) handleDeleteOrgApiKey(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.handleDeleteApiKeyIn(w, r, scope)
}

// ---- OAuth forfaits ----

func (s *Server) handleOrgListOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, false)
	if !ok {
		return
	}
	s.listOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID))
}

func (s *Server) handleOrgStartOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.startOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleOrgCompleteOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.completeOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleOrgUploadOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.uploadOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleOrgRefreshOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.refreshOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleOrgRenameOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.renameOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleOrgDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	s.deleteOAuthForOwner(w, r, secrets.OrgTierOwnerKey(orgID), secrets.OAuthKind(r.PathValue("kind")))
}

// ---- Audience ----

type orgCredentialAudienceView struct {
	Teams    []string `json:"teams"`
	AllTeams bool     `json:"all_teams"`
}

func (s *Server) handleGetOrgCredentialAudience(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, false)
	if !ok {
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	teams := o.CredentialAudience.Teams
	if teams == nil {
		teams = []string{}
	}
	writeJSON(w, orgCredentialAudienceView{Teams: teams, AllTeams: o.CredentialAudience.AllTeams})
}

func (s *Server) handleUpdateOrgCredentialAudience(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.orgCredentialScope(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Teams    *[]string `json:"teams,omitempty"`
		AllTeams *bool     `json:"all_teams,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// The read above is only to MERGE onto the current audience (a request
	// that names teams but not all_teams keeps the stored flag); the write
	// below patches the audience field alone, so a concurrent settings or
	// plan edit is not reverted by it.
	if req.Teams != nil {
		// Every named team must belong to THIS org. Without the check an
		// admin could lend their org's subscription to a team of another
		// org by pasting its id — the audience is an authorization list,
		// so an unvalidated entry is a cross-tenant grant.
		teams, err := s.authStore().ListTeamsByOrg(r.Context(), orgID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "list org teams: %v", err)
			return
		}
		inOrg := make(map[string]bool, len(teams))
		for _, t := range teams {
			inOrg[t.ID] = true
		}
		clean := make([]string, 0, len(*req.Teams))
		seen := map[string]bool{}
		for _, id := range *req.Teams {
			if !inOrg[id] {
				httpError(w, http.StatusUnprocessableEntity,
					"team %s is not in this org — an org lends its credentials only to its own teams", id)
				return
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			clean = append(clean, id)
		}
		o.CredentialAudience.Teams = clean
	}
	if req.AllTeams != nil {
		o.CredentialAudience.AllTeams = *req.AllTeams
	}
	if _, err := s.authStore().PatchOrg(r.Context(), orgID, identity.OrgPatch{CredentialAudience: &o.CredentialAudience}); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org.credential_audience.updated", "org", orgID, map[string]any{
		"teams": o.CredentialAudience.Teams, "all_teams": o.CredentialAudience.AllTeams,
	})
	teamsOut := o.CredentialAudience.Teams
	if teamsOut == nil {
		teamsOut = []string{}
	}
	writeJSON(w, orgCredentialAudienceView{Teams: teamsOut, AllTeams: o.CredentialAudience.AllTeams})
}
