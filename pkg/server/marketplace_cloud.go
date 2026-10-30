package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

// marketplaceViewer builds the scope/status visibility context for a
// browse request. In local single-tenant mode it returns the zero value
// (Enforce false → every entry visible to the sole operator); in cloud
// mode it reflects the authenticated principal (or an anonymous enforced
// viewer when no identity is present).
func (s *Server) marketplaceViewer(r *http.Request) marketplace.ViewerContext {
	if s.cfg.Mode != "cloud" {
		return marketplace.ViewerContext{}
	}
	v := marketplace.ViewerContext{Enforce: true}
	if id, ok := auth.FromContext(r.Context()); ok && id.UserID != "" {
		v.Authenticated = true
		v.UserID = id.UserID
		v.IsSuperAdmin = id.IsSuperAdmin
		if id.TeamID != "" {
			v.OrgIDs = []string{id.TeamID}
		}
	}
	return v
}

// marketplaceConfigResponse is the wire body for
// GET /api/v1/marketplace/config — what the studio submit form needs to
// render the scope picker and decide whether submission is available.
type marketplaceConfigResponse struct {
	Mode          string   `json:"mode"`           // "cloud" | "local"
	SubmitEnabled bool     `json:"submit_enabled"` // false when the registry is read-only
	Scopes        []string `json:"scopes"`         // allowed visibility scopes
	DefaultScope  string   `json:"default_scope"`  // pre-selected scope
	Moderated     bool     `json:"moderated"`      // submissions land pending (cloud)
}

// marketplaceAllowedScopes resolves the visibility scopes an operator may
// pick at submit time. Configurable via ITERION_MARKETPLACE_SCOPES
// (comma-separated subset of public,instance,org); defaults to all three
// in cloud mode and just "public" in local mode (scope is moot there).
func (s *Server) marketplaceAllowedScopes() []string {
	if s.cfg.Mode != "cloud" {
		return []string{string(marketplace.ScopePublic)}
	}
	raw := strings.TrimSpace(envOperator("ITERION_MARKETPLACE_SCOPES"))
	if raw == "" {
		return []string{
			string(marketplace.ScopeOrg),
			string(marketplace.ScopeInstance),
			string(marketplace.ScopePublic),
		}
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		switch marketplace.Scope(strings.TrimSpace(p)) {
		case marketplace.ScopePublic, marketplace.ScopeInstance, marketplace.ScopeOrg:
			out = append(out, strings.TrimSpace(p))
		}
	}
	if len(out) == 0 {
		out = []string{string(marketplace.ScopeOrg)}
	}
	return out
}

// marketplaceDefaultScope is the pre-selected submit scope: the first
// allowed scope, overridable via ITERION_MARKETPLACE_DEFAULT_SCOPE.
func (s *Server) marketplaceDefaultScope() string {
	allowed := s.marketplaceAllowedScopes()
	want := strings.TrimSpace(envOperator("ITERION_MARKETPLACE_DEFAULT_SCOPE"))
	for _, a := range allowed {
		if a == want {
			return want
		}
	}
	return allowed[0]
}

// resolveSubmitScope validates a requested scope against the allowed
// set, falling back to the configured default when empty or invalid.
func (s *Server) resolveSubmitScope(want string) string {
	want = strings.TrimSpace(want)
	for _, a := range s.marketplaceAllowedScopes() {
		if a == want {
			return want
		}
	}
	return s.marketplaceDefaultScope()
}

// handleMarketplaceConfig answers GET /api/v1/marketplace/config.
func (s *Server) handleMarketplaceConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	mode := "local"
	if s.cfg.Mode == "cloud" {
		mode = "cloud"
	}
	s.writeJSONFor(w, r, marketplaceConfigResponse{
		Mode:          mode,
		SubmitEnabled: true,
		Scopes:        s.marketplaceAllowedScopes(),
		DefaultScope:  s.marketplaceDefaultScope(),
		Moderated:     s.cfg.Mode == "cloud",
	})
}

// marketplaceModerateGate authorizes a moderation action on entry by the
// request's principal: org-scoped entries are moderated by that org's
// admins, instance/public entries by a platform super-admin (platformAuthority:
// the claim re-derived at the identity). Returns the identity on success, or
// writes the error response and returns ok=false.
func (s *Server) marketplaceModerateGate(w http.ResponseWriter, r *http.Request, entry *marketplace.Entry) (auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok || id.UserID == "" {
		s.httpErrorFor(w, r, http.StatusUnauthorized, "authentication required")
		return auth.Identity{}, false
	}
	switch marketplace.EffectiveScope(*entry) {
	case marketplace.ScopeOrg:
		if !s.canManageTeam(r.Context(), id, entry.OrgID) {
			s.httpErrorFor(w, r, http.StatusForbidden, "org admin required")
			return auth.Identity{}, false
		}
	default: // public / instance
		if !s.platformAuthority(w, r) {
			return auth.Identity{}, false
		}
	}
	return id, true
}

// handleMarketplaceModerationList answers
// GET /api/v1/marketplace/moderation — the pending queue scoped to the
// principal (super-admin sees all; an org admin sees only their org's).
func (s *Server) handleMarketplaceModerationList(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	if s.cfg.Mode != "cloud" {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: moderation is cloud-only")
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok || id.UserID == "" {
		s.httpErrorFor(w, r, http.StatusUnauthorized, "authentication required")
		return
	}
	if !id.HasRole(identity.RoleAdmin) {
		s.httpErrorFor(w, r, http.StatusForbidden, "admin required")
		return
	}
	q := marketplace.ModerationQuery{Statuses: []marketplace.Status{marketplace.StatusPending}}
	if id.IsSuperAdmin {
		q.All = true
	} else {
		q.OrgIDs = []string{id.TeamID}
	}
	entries, err := s.marketplace.ListForModeration(r.Context(), q)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: moderation list: %v", err)
		return
	}
	if entries == nil {
		entries = []marketplace.Entry{}
	}
	s.writeJSONFor(w, r, map[string]any{"bots": entries})
}

// marketplaceRejectRequest is the body for the reject endpoint.
type marketplaceRejectRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleMarketplaceApprove answers
// POST /api/v1/marketplace/moderation/{slug}/approve.
func (s *Server) handleMarketplaceApprove(w http.ResponseWriter, r *http.Request) {
	s.marketplaceModerate(w, r, marketplace.StatusApproved, "")
}

// handleMarketplaceReject answers
// POST /api/v1/marketplace/moderation/{slug}/reject (with an optional
// reason surfaced to the submitter).
func (s *Server) handleMarketplaceReject(w http.ResponseWriter, r *http.Request) {
	var req marketplaceRejectRequest
	_ = readJSON(r, &req) // reason is optional; ignore decode errors
	s.marketplaceModerate(w, r, marketplace.StatusRejected, strings.TrimSpace(req.Reason))
}

// marketplaceModerate is the shared approve/reject tail: resolve the
// entry, authorize by scope, transition the status, and audit.
func (s *Server) marketplaceModerate(w http.ResponseWriter, r *http.Request, next marketplace.Status, reason string) {
	if !s.requireMarketplace(w, r) {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode != "cloud" {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: moderation is cloud-only")
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: slug required")
		return
	}
	entry, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	id, ok := s.marketplaceModerateGate(w, r, entry)
	if !ok {
		return
	}
	review := marketplace.Review{
		By:     id.UserID,
		At:     time.Now().UTC().Format(time.RFC3339),
		Reason: reason,
	}
	if err := s.marketplace.SetStatus(r.Context(), slug, "", next, review); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: set status: %v", err)
		return
	}
	action := "marketplace.approved"
	if next == marketplace.StatusRejected {
		action = "marketplace.rejected"
	}
	meta := map[string]any{"slug": slug, "scope": string(marketplace.EffectiveScope(*entry))}
	if marketplace.EffectiveScope(*entry) == marketplace.ScopeOrg {
		s.auditTenant(r, entry.OrgID, action, "marketplace", slug, meta)
	} else {
		s.auditPlatform(r, entry.OrgID, action, "marketplace", slug, meta)
	}
	refreshed, _, _ := s.marketplace.Get(r.Context(), slug)
	if refreshed == nil {
		refreshed = entry
	}
	s.writeJSONFor(w, r, refreshed)
}
