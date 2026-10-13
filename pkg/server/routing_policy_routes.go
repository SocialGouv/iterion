package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

// The TENANT routing-policy control surface (ADR-121 delivery 2): an
// org's and a team's own adaptive-routing policy, one document per
// tenant, org-admin / team-admin managed. The launch-time fold
// (resolveRunLLMRoutePolicy) reads these records between the bot and
// the platform level. This file is their only validated write path —
// and the fold still re-validates on read, because a hand-edited or
// rolling-deploy record must refuse the launch rather than fold
// garbage.
//
// CAS: every write carries the record's updated_at stamp. A client
// that read stale state loses the race as a loud 409 (the bot-vars
// precedent) instead of silently dropping the concurrent editor's
// policy — the one primitive cost governance has is a lock, and a
// blind last-write-wins could erase one.

func (s *Server) registerRoutingPolicyRoutes() {
	s.mux.Handle("GET /api/orgs/{id}/routing-policy", s.requireAuth(http.HandlerFunc(s.handleGetOrgRoutingPolicy)))
	s.mux.Handle("PUT /api/orgs/{id}/routing-policy", s.requireAuth(http.HandlerFunc(s.handlePutOrgRoutingPolicy)))
	s.mux.Handle("GET /api/teams/{id}/routing-policy", s.requireAuth(http.HandlerFunc(s.handleGetTeamRoutingPolicy)))
	s.mux.Handle("PUT /api/teams/{id}/routing-policy", s.requireAuth(http.HandlerFunc(s.handlePutTeamRoutingPolicy)))
}

// routingPolicyView is GET /api/{orgs,teams}/{id}/routing-policy: the
// stored policy (null when the level is unset — origin "default"),
// plus the CAS stamp writes carry back.
type routingPolicyView struct {
	Scope     string           `json:"scope"`  // "org" | "team"
	Policy    *llmroute.Policy `json:"policy"` // null = level unset
	Origin    string           `json:"origin"` // "default" | "db"
	UpdatedAt *time.Time       `json:"updated_at,omitempty"`
	UpdatedBy string           `json:"updated_by,omitempty"`
}

// routingPolicyPutReq is the PUT body of both tenant routing-policy
// routes. Routing is REQUIRED — absent is a 400, not a keep: a body
// naming nothing is a misfire, and a blind rewrite would bump
// updated_at under a concurrent editor's CAS.
type routingPolicyPutReq struct {
	// An object REPLACES the tenant's policy wholesale (the fold's
	// levels are whole records, like the platform block); null CLEARS
	// the record outright so a later read answers "never set".
	Routing json.RawMessage `json:"routing"`
	// The updated_at the caller based the write on. Present and stale
	// → 409, whether or not the store itself CASses.
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at,omitempty"`
}

func (s *Server) handleGetOrgRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this org")
		return
	}
	s.writeRoutingPolicyView(w, r, "org", platformcfg.OrgRoutingPolicyID(orgID))
}

func (s *Server) handlePutOrgRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	s.putRoutingPolicy(w, r, "org", orgID, "org.routing_policy.updated")
}

func (s *Server) handleGetTeamRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this team")
		return
	}
	s.writeRoutingPolicyView(w, r, "team", platformcfg.TeamRoutingPolicyID(teamID))
}

func (s *Server) handlePutTeamRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	s.putRoutingPolicy(w, r, "team", teamID, "team.routing_policy.updated")
}

// writeRoutingPolicyView answers the GET view for one tenant document.
// A deployment without the tenant store (local mode) answers the empty
// view: the level is genuinely unset there, and the UI stays
// renderable.
func (s *Server) writeRoutingPolicyView(w http.ResponseWriter, r *http.Request, scope, docID string) {
	view := routingPolicyView{Scope: scope, Origin: "default"}
	if get := s.routingPolicyStoreFor; get != nil {
		rec, err := get(docID).Get(r.Context())
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if rec != nil {
			stamp := rec.UpdatedAt
			view.Policy = rec.Policy
			view.Origin = "db"
			view.UpdatedAt = &stamp
			view.UpdatedBy = rec.UpdatedBy
		}
	}
	s.writeJSONFor(w, r, view)
}

// putRoutingPolicy validates and stores (or clears) one tenant's
// routing policy. The scope is "org" or "team"; tenantID is the path
// id; auditAction names the audit row.
func (s *Server) putRoutingPolicy(w http.ResponseWriter, r *http.Request, scope, tenantID, auditAction string) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.routingPolicyStoreFor == nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "tenant routing-policy store unavailable on this deployment")
		return
	}
	var req routingPolicyPutReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if len(req.Routing) == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "routing is required (an object to replace, null to clear)")
		return
	}
	docID := s.tenantRoutingDocID(scope, tenantID)
	st := s.routingPolicyStoreFor(docID)

	prevUpdatedAt := time.Time{}
	var old *platformcfg.RoutingPolicyRecord
	if rec, err := st.Get(r.Context()); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	} else if rec != nil {
		old = rec
		prevUpdatedAt = rec.UpdatedAt
	}
	if req.ExpectedUpdatedAt != nil && !req.ExpectedUpdatedAt.Equal(prevUpdatedAt) {
		s.httpErrorFor(w, r, http.StatusConflict, "routing policy changed concurrently — re-read and retry")
		return
	}

	cleared := string(bytes.TrimSpace(req.Routing)) == "null"
	auditMeta := map[string]any{"cleared": cleared, "had_policy": old != nil}
	if cleared {
		// The clear CASses like the replace: a null-write racing a
		// concurrent editor must lose as a 409, never destroy the
		// editor's just-landed policy behind a 200.
		del, ok := st.(platformcfg.CASDeleter)
		if !ok {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "tenant routing-policy store cannot clear")
			return
		}
		wrote, err := del.DeleteIfUnchanged(r.Context(), prevUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if !wrote {
			s.httpErrorFor(w, r, http.StatusConflict, "routing policy changed concurrently — re-read and retry")
			return
		}
	} else {
		var policy llmroute.Policy
		dec := json.NewDecoder(bytes.NewReader(req.Routing))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&policy); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "routing: %v", err)
			return
		}
		if err := llmroute.Validate(policy); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "routing: %v", err)
			return
		}
		rec := platformcfg.RoutingPolicyRecord{Policy: &policy, UpdatedBy: s.requestUserID(r)}
		if cas, ok := st.(platformcfg.CASStore[platformcfg.RoutingPolicyRecord]); ok {
			wrote, err := cas.PutIfUnchanged(r.Context(), rec, prevUpdatedAt)
			if err != nil {
				s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
				return
			}
			if !wrote {
				s.httpErrorFor(w, r, http.StatusConflict, "routing policy changed concurrently — re-read and retry")
				return
			}
		} else if err := st.Put(r.Context(), rec); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	if scope == "org" {
		s.auditOrg(r, tenantID, auditAction, "routing_policy", docID, auditMeta)
	} else {
		s.auditTenant(r, tenantID, auditAction, "routing_policy", docID, auditMeta)
	}
	s.writeRoutingPolicyView(w, r, scope, docID)
}

// tenantRoutingDocID maps the route's scope + path id to the document
// id the store and the audit row share.
func (s *Server) tenantRoutingDocID(scope, tenantID string) string {
	if scope == "org" {
		return platformcfg.OrgRoutingPolicyID(tenantID)
	}
	return platformcfg.TeamRoutingPolicyID(tenantID)
}
