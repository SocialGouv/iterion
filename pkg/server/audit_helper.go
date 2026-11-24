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
	s.auditInsert(audit.Event{
		Scope:     audit.ScopeTenant,
		TenantID:  tenantID,
		ActorID:   actorID,
		ActorKind: "system",
		Action:    action,
		Target:    target,
		TargetID:  targetID,
		Meta:      meta,
	})
}

// auditWriteActor records a row attributed to an EXPLICIT actor rather than
// the identity of the request it serves: a sign-in has none in its context
// (the route is anonymous), and the account it resolved — or failed to — is
// what the row is about. IP and user-agent still come from the request.
// Scope and tenant are the caller's: a refused sign-in names no tenant.
func (s *Server) auditWriteActor(r *http.Request, scope audit.Scope, tenantID, actorID, actorKind, action, target, targetID string, meta map[string]any) {
	s.auditInsert(s.auditActorEvent(r, scope, tenantID, actorID, actorKind, action, target, targetID, meta))
}

// auditActorEvent is the row auditWriteActor records, built and not yet
// inserted.
func (s *Server) auditActorEvent(r *http.Request, scope audit.Scope, tenantID, actorID, actorKind, action, target, targetID string, meta map[string]any) audit.Event {
	return audit.Event{
		Scope:     scope,
		TenantID:  tenantID,
		ActorID:   actorID,
		ActorKind: actorKind,
		Action:    action,
		Target:    target,
		TargetID:  targetID,
		Meta:      meta,
		IP:        s.clientIP(r),
		UserAgent: r.UserAgent(),
	}
}

// auditInsert is the one write every audit row goes through: it stamps the
// id and the time, then inserts detached and best-effort — an audit-store
// blip must not fail or slow what the row records. No-op without a store
// (local mode). The insert's deadline is set when it is queued, not when
// its goroutine starts: a line reaches the store within s.auditInsertBound()
// of being queued, or never — the bound an erasure's second scrub waits out.
func (s *Server) auditInsert(e audit.Event) {
	if s.auditStore == nil {
		return
	}
	e.ID = uuid.NewString()
	e.CreatedAt = time.Now().UTC()
	deadline := e.CreatedAt.Add(s.auditInsertBound())
	s.goSafe("audit-insert", func() {
		bg, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if err := s.auditStore.Insert(bg, e); err != nil && s.logger != nil {
			s.logger.Warn("audit: insert %s: %v", e.Action, err)
		}
	})
}

// auditActor names who an audit line is attributed to: the inbound request a
// write serves, or a subsystem writing with none. A body shared by an HTTP
// write and a service hook carries one of these instead of a *http.Request,
// so both record the SAME line, each under its own actor.
type auditActor struct {
	// r is the request the write serves; nil for a subsystem.
	r *http.Request
	// system names the subsystem when r is nil, as auditSystem's actorID.
	system string
}

// requestActor attributes the lines of a write to the request it serves.
func requestActor(r *http.Request) auditActor { return auditActor{r: r} }

// systemActor attributes the lines of a write a subsystem makes with no
// request of its own (id names the subsystem, e.g. "auth").
func systemActor(id string) auditActor { return auditActor{system: id} }

// auditTenantAs records a tenant-scoped mutation for either actor.
func (s *Server) auditTenantAs(a auditActor, tenantID, action, target, targetID string, meta map[string]any) {
	if a.r != nil {
		s.auditTenant(a.r, tenantID, action, target, targetID, meta)
		return
	}
	s.auditSystem(tenantID, a.system, action, target, targetID, meta)
}

// auditWrite records a row attributed to the identity of the request it
// serves (auth.FromContext): the actor of every control-plane mutation.
func (s *Server) auditWrite(r *http.Request, scope audit.Scope, tenantID, action, target, targetID string, meta map[string]any) {
	s.auditInsert(s.auditRequestEvent(r, scope, tenantID, action, target, targetID, meta))
}

// auditRequestEvent is the row auditWrite records, built and not yet
// inserted: attributed to the identity of the request it serves.
func (s *Server) auditRequestEvent(r *http.Request, scope audit.Scope, tenantID, action, target, targetID string, meta map[string]any) audit.Event {
	id, _ := auth.FromContext(r.Context())
	return s.auditActorEvent(r, scope, tenantID, id.UserID, auditActorKind(id, id.UserID), action, target, targetID, meta)
}

// auditActorKind classifies the actor of an audit line from the principal
// the write runs under and the actor id it records: a super-admin, the
// synthetic webhook principal (by kind, or by the "webhook:" owner prefix a
// lane with no auth identity still carries), a daemon of this process
// (KindInternal, or no principal at all), else a user.
func auditActorKind(id auth.Identity, actorID string) string {
	switch {
	case id.IsSuperAdmin:
		return "super_admin"
	case id.Kind == auth.KindWebhook || strings.HasPrefix(actorID, "webhook:"):
		return "webhook"
	case id.Kind == auth.KindInternal || id.UserID == "":
		return "system"
	}
	return "user"
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
