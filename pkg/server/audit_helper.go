package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
)

// auditTenant records a tenant-scoped control-plane mutation (visible
// to the org's admins). One line at each mutation call site; actor /
// IP / user-agent are derived from the request. Best-effort and
// detached — an audit-store blip must not fail or slow the mutation
// (same pattern as the webhook MarkUsed write).
func (s *Server) auditTenant(r *http.Request, tenantID, action, target, targetID string, meta map[string]any) {
	s.auditWrite(r, audit.ScopeTenant, tenantID, action, target, targetID, meta)
}

// auditOrg records an org-scoped control-plane mutation (members, SSO,
// settings — visible to the org's admins via /api/orgs/{id}/audit). It
// is a tenant-scoped row keyed by the org id; team-scoped rows key by
// team id, so the two are naturally partitioned by the id used.
func (s *Server) auditOrg(r *http.Request, orgID, action, target, targetID string, meta map[string]any) {
	s.auditWrite(r, audit.ScopeTenant, orgID, action, target, targetID, meta)
}

// auditPlatform records a platform-scoped (super-admin) action,
// readable only via /api/admin/audit. TenantID still records which
// org was acted on, for filtering.
func (s *Server) auditPlatform(r *http.Request, tenantID, action, target, targetID string, meta map[string]any) {
	s.auditWrite(r, audit.ScopePlatform, tenantID, action, target, targetID, meta)
}

// auditSystem records a tenant-scoped event originated by a background
// goroutine (no *http.Request — e.g. the cloudsched ticker's per-tick
// decisions). ActorKind is "system"; actorID names the subsystem
// ("scheduler:<bot>"). Same best-effort/detached discipline as
// auditWrite.
func (s *Server) auditSystem(tenantID, actorID, action, target, targetID string, meta map[string]any) {
	if s.auditStore == nil {
		return
	}
	e := audit.Event{
		ID:        uuid.NewString(),
		Scope:     audit.ScopeTenant,
		TenantID:  tenantID,
		ActorID:   actorID,
		ActorKind: "system",
		Action:    action,
		Target:    target,
		TargetID:  targetID,
		Meta:      meta,
		CreatedAt: time.Now().UTC(),
	}
	s.goSafe("audit-insert", func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.auditStore.Insert(bg, e); err != nil && s.logger != nil {
			s.logger.Warn("audit: insert %s: %v", action, err)
		}
	})
}

func (s *Server) auditWrite(r *http.Request, scope audit.Scope, tenantID, action, target, targetID string, meta map[string]any) {
	if s.auditStore == nil {
		return
	}
	id, _ := auth.FromContext(r.Context())
	kind := "user"
	switch {
	case id.IsSuperAdmin:
		kind = "super_admin"
	case strings.HasPrefix(id.UserID, "webhook:"):
		kind = "webhook"
	case id.UserID == "":
		kind = "system"
	}
	e := audit.Event{
		ID:        uuid.NewString(),
		Scope:     scope,
		TenantID:  tenantID,
		ActorID:   id.UserID,
		ActorKind: kind,
		Action:    action,
		Target:    target,
		TargetID:  targetID,
		Meta:      meta,
		IP:        s.clientIP(r),
		UserAgent: r.UserAgent(),
		CreatedAt: time.Now().UTC(),
	}
	s.goSafe("audit-insert", func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.auditStore.Insert(bg, e); err != nil && s.logger != nil {
			s.logger.Warn("audit: insert %s: %v", action, err)
		}
	})
}

// ---- REST ----

func (s *Server) registerAuditRoutes() {
	if s.auditStore == nil {
		return
	}
	s.mux.Handle("GET /api/teams/{id}/audit", s.requireAuth(http.HandlerFunc(s.handleTeamAudit)))
	s.mux.Handle("GET /api/orgs/{id}/audit", s.requireAuth(http.HandlerFunc(s.handleOrgAudit)))
	s.mux.Handle("GET /api/admin/audit", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminAudit)))
}

// auditPageFromQuery parses ?action&actor&from&to&offset&limit.
func auditPageFromQuery(r *http.Request) audit.Page {
	q := r.URL.Query()
	p := audit.Page{
		Action:  q.Get("action"),
		ActorID: q.Get("actor"),
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v > 0 {
		p.Offset = v
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		p.Limit = v
	}
	if t, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		p.From = t
	}
	if t, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		p.To = t
	}
	return p
}

type auditListResponse struct {
	Events     []audit.Event `json:"events"`
	NextOffset int           `json:"next_offset"`
}

func (s *Server) handleTeamAudit(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	// Admin-gated (not just membership): audit rows expose actor
	// emails/IPs across the whole org.
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "requires team admin")
		return
	}
	p := auditPageFromQuery(r)
	events, err := s.auditStore.ListByTenant(r.Context(), teamID, p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list audit: %v", err)
		return
	}
	writeJSON(w, auditListResponse{Events: events, NextOffset: p.Offset + len(events)})
}

func (s *Server) handleOrgAudit(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	// Admin-gated: audit rows expose actor emails/IPs across the org.
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "requires org admin")
		return
	}
	p := auditPageFromQuery(r)
	events, err := s.auditStore.ListByTenant(r.Context(), orgID, p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list audit: %v", err)
		return
	}
	writeJSON(w, auditListResponse{Events: events, NextOffset: p.Offset + len(events)})
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	p := auditPageFromQuery(r)
	events, err := s.auditStore.ListPlatform(r.Context(), p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list audit: %v", err)
		return
	}
	writeJSON(w, auditListResponse{Events: events, NextOffset: p.Offset + len(events)})
}
