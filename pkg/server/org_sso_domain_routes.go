package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerOrgSSODomainRoutes wires the per-tenant verified-domain CRUD that
// gates per-org SSO auto-link (a tenant proves it controls an email domain via
// a DNS TXT challenge before its IdP may auto-link addresses at that domain).
func (s *Server) registerOrgSSODomainRoutes() {
	s.mux.Handle("GET /api/orgs/{id}/sso/domains", s.requireAuth(http.HandlerFunc(s.handleListOrgSSODomains)))
	s.mux.Handle("POST /api/orgs/{id}/sso/domains", s.requireAuth(http.HandlerFunc(s.handleCreateOrgSSODomain)))
	s.mux.Handle("POST /api/orgs/{id}/sso/domains/{domain_id}/verify", s.requireAuth(http.HandlerFunc(s.handleVerifyOrgSSODomain)))
	s.mux.Handle("DELETE /api/orgs/{id}/sso/domains/{domain_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgSSODomain)))
}

// orgDomainView is the response shape: the row plus the DNS TXT record the
// admin must publish (the token is the challenge value — public by design).
type orgDomainView struct {
	orgsso.VerifiedDomain
	ChallengeHost  string `json:"challenge_host"`
	ChallengeValue string `json:"challenge_value"`
}

func domainView(d orgsso.VerifiedDomain) orgDomainView {
	return orgDomainView{VerifiedDomain: d, ChallengeHost: d.ChallengeHost(), ChallengeValue: d.ChallengeValue()}
}

func (s *Server) handleListOrgSSODomains(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	rows, err := s.orgDomains.ListByTenant(store.WithTenant(r.Context(), teamID), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list domains: %v", err)
		return
	}
	views := make([]orgDomainView, 0, len(rows))
	for _, d := range rows {
		views = append(views, domainView(d))
	}
	writeJSON(w, map[string]any{"domains": views})
}

func (s *Server) handleCreateOrgSSODomain(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	domain := orgsso.NormalizeDomain(req.Domain)
	if domain == "" || !strings.Contains(domain, ".") {
		httpError(w, http.StatusBadRequest, "a valid domain is required")
		return
	}
	token, err := orgsso.NewDomainToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "token")
		return
	}
	d := orgsso.VerifiedDomain{
		ID: uuid.NewString(), TenantID: teamID, Domain: domain, Token: token,
		CreatedBy: id.UserID, CreatedAt: time.Now().UTC(),
	}
	if err := s.orgDomains.Create(store.WithTenant(r.Context(), teamID), d); err != nil {
		if err == orgsso.ErrDomainExists {
			httpError(w, http.StatusConflict, "domain already claimed for this org")
			return
		}
		httpError(w, http.StatusInternalServerError, "create domain: %v", err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.domain_added", "org_verified_domain", d.ID, map[string]any{"domain": domain})
	writeJSON(w, domainView(d))
}

func (s *Server) handleVerifyOrgSSODomain(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	d, err := s.orgDomains.Get(ctx, r.PathValue("domain_id"))
	if err != nil || d.TenantID != teamID {
		httpError(w, http.StatusNotFound, "domain not found")
		return
	}
	ok, verr := orgsso.VerifyDomainTXT(ctx, s.orgDomainTXT, d)
	if verr != nil {
		writeJSON(w, map[string]any{"verified": false, "error": "DNS lookup failed"})
		return
	}
	if !ok {
		writeJSON(w, map[string]any{"verified": false})
		return
	}
	now := time.Now().UTC()
	d.VerifiedAt = &now
	if err := s.orgDomains.Update(ctx, d); err != nil {
		httpError(w, http.StatusInternalServerError, "persist verification: %v", err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.domain_verified", "org_verified_domain", d.ID, map[string]any{"domain": d.Domain})
	writeJSON(w, map[string]any{"verified": true, "domain": domainView(d)})
}

func (s *Server) handleDeleteOrgSSODomain(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	teamID := s.firstTeamInOrg(r.Context(), orgID)
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	domainID := r.PathValue("domain_id")
	d, err := s.orgDomains.Get(ctx, domainID)
	if err != nil || d.TenantID != teamID {
		httpError(w, http.StatusNotFound, "domain not found")
		return
	}
	if err := s.orgDomains.Delete(ctx, domainID); err != nil {
		httpError(w, http.StatusInternalServerError, "delete domain: %v", err)
		return
	}
	s.auditOrg(r, orgID, "org_sso.domain_removed", "org_verified_domain", domainID, map[string]any{"domain": d.Domain})
	w.WriteHeader(http.StatusNoContent)
}
