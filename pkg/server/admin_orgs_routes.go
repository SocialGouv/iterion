package server

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerAdminOrgRoutes wires the super-admin Organization console.
// Every route is super-admin only and operates on the top-level
// identity.Org (members/SSO/billing/quotas); team-scoped resources live
// under /api/teams/{id}. The org-admin self-serve mirror of these views
// lives in orgs_routes.go.
func (s *Server) registerAdminOrgRoutes() {
	s.mux.Handle("GET /api/admin/orgs", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListOrgs)))
	s.mux.Handle("POST /api/admin/orgs", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminCreateOrg)))
	s.mux.Handle("GET /api/admin/orgs/{id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetOrg)))
	s.mux.Handle("PATCH /api/admin/orgs/{id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminUpdateOrg)))
	s.mux.Handle("DELETE /api/admin/orgs/{id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeleteOrg)))
	s.mux.Handle("POST /api/admin/orgs/{id}/status", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminSetOrgStatus)))
	s.mux.Handle("GET /api/admin/orgs/{id}/usage", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminOrgUsage)))
	// Super-admin drill-down: the teams inside one org.
	s.mux.Handle("GET /api/admin/orgs/{id}/teams", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminOrgTeams)))
}

// ---- views / requests ----

type orgView struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	Status            string  `json:"status"`
	Personal          bool    `json:"personal,omitempty"`
	MonthlyRunQuota   int     `json:"monthly_run_quota,omitempty"`
	MemoryQuotaBytes  int64   `json:"memory_quota_bytes,omitempty"`
	MonthlyCostCapUSD float64 `json:"monthly_cost_cap_usd,omitempty"`
	SuspendReason     string  `json:"suspend_reason,omitempty"`
	CreatedAt         string  `json:"created_at,omitempty"`
}

func toOrgView(o identity.Org) orgView {
	return orgView{
		ID:                o.ID,
		Name:              o.Name,
		Slug:              o.Slug,
		Status:            string(o.EffectiveStatus()),
		Personal:          o.Personal,
		MonthlyRunQuota:   o.MonthlyRunQuota,
		MemoryQuotaBytes:  o.MemoryQuotaBytes,
		MonthlyCostCapUSD: o.MonthlyCostCapUSD,
		SuspendReason:     o.SuspendReason,
		CreatedAt:         o.CreatedAt.Format(time.RFC3339),
	}
}

// teamSummaryView is the lightweight team row used by the org teams
// drill-down (super-admin) and the org self-serve teams list.
type teamSummaryView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	Status            string `json:"status"`
	Personal          bool   `json:"personal,omitempty"`
	MaxConcurrentRuns int    `json:"max_concurrent_runs,omitempty"`
	LaunchRatePerMin  int    `json:"launch_rate_per_min,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
}

func toTeamSummaryView(t identity.Team) teamSummaryView {
	return teamSummaryView{
		ID:                t.ID,
		Name:              t.Name,
		Slug:              t.Slug,
		Status:            string(t.EffectiveStatus()),
		Personal:          t.Personal,
		MaxConcurrentRuns: t.MaxConcurrentRuns,
		LaunchRatePerMin:  t.LaunchRatePerMin,
		CreatedAt:         t.CreatedAt.Format(time.RFC3339),
	}
}

// orgUsageView is the consumption snapshot for one org — served to
// super-admins (/api/admin/orgs/{id}/usage) and to the org's own
// members (/api/orgs/{id}/usage). The monthly run/cost counters are
// org-keyed; the resource counts (api keys, secrets, bindings,
// webhooks, memory) are summed across every team in the org. Fields
// read zero when the corresponding store isn't wired (local mode).
type orgUsageView struct {
	Org     orgView `json:"org"`
	Members int     `json:"members"`
	Teams   int     `json:"teams"`
	// EffectiveMemoryQuotaBytes resolves the org override (or the
	// platform default) so the console shows the real ceiling.
	EffectiveMemoryQuotaBytes int64 `json:"effective_memory_quota_bytes"`
	MonthlyRunQuota           int   `json:"monthly_run_quota"`

	// Current-month metering (orgusage counter, keyed by org).
	RunsThisMonth    int     `json:"runs_this_month"`
	CostUSDThisMonth float64 `json:"cost_usd_this_month"`
	InputTokens      int64   `json:"input_tokens_this_month"`
	OutputTokens     int64   `json:"output_tokens_this_month"`
	// Caps as enforced by the launch gate (org override or platform
	// default; 0 = unlimited).
	MonthlyCostCapUSD float64 `json:"monthly_cost_cap_usd,omitempty"`

	// Live + auxiliary counters (summed across the org's teams).
	ActiveRuns            int   `json:"active_runs"`
	WebhookCallsThisMonth int   `json:"webhook_calls_this_month"`
	MemoryUsedBytes       int64 `json:"memory_used_bytes"`
	APIKeyCount           int   `json:"api_key_count"`
	GenericSecretCount    int   `json:"generic_secret_count"`
	BotBindingCount       int   `json:"bot_binding_count"`
	WebhookCount          int   `json:"webhook_count"`
}

type createOrgReq struct {
	Name       string `json:"name"`
	Slug       string `json:"slug,omitempty"`
	OwnerEmail string `json:"owner_email,omitempty"`
}

type updateOrgReq struct {
	Name              *string  `json:"name,omitempty"`
	Slug              *string  `json:"slug,omitempty"`
	MonthlyRunQuota   *int     `json:"monthly_run_quota,omitempty"`
	MemoryQuotaBytes  *int64   `json:"memory_quota_bytes,omitempty"`
	MonthlyCostCapUSD *float64 `json:"monthly_cost_cap_usd,omitempty"`
}

type setOrgStatusReq struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// effectiveOrgMemoryQuota resolves the org's memory ceiling: the
// explicit per-org override when set, else the platform default.
func effectiveOrgMemoryQuota(o identity.Org) int64 {
	if o.MemoryQuotaBytes > 0 {
		return o.MemoryQuotaBytes
	}
	return knowledge.DefaultOrgAggregateQuota
}

// authStoreOrFail returns the identity store, writing a 500 and
// reporting ok=false when it isn't wired (so super-admin handlers don't
// each repeat the nil check).
func (s *Server) authStoreOrFail(w http.ResponseWriter) (identity.Store, bool) {
	st := s.authStore()
	if st == nil {
		httpError(w, http.StatusInternalServerError, "identity store unavailable")
		return nil, false
	}
	return st, true
}

// applyNonNegative copies *p into *dst when p is non-nil, rejecting
// negative values with a 400. Returns true on success (including the
// p==nil no-op), false if it already wrote an error response.
func applyNonNegative[T int | int64 | float64](w http.ResponseWriter, p *T, dst *T, field string) bool {
	if p == nil {
		return true
	}
	if *p < 0 {
		httpError(w, http.StatusBadRequest, "%s must be >= 0", field)
		return false
	}
	*dst = *p
	return true
}

// ---- handlers ----

func (s *Server) handleAdminListOrgs(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	orgs, err := store.ListOrgs(r.Context(), identity.Page{Limit: 500})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]orgView, 0, len(orgs))
	for _, o := range orgs {
		views = append(views, toOrgView(o))
	}
	writeJSON(w, struct {
		Orgs []orgView `json:"orgs"`
	}{Orgs: views})
}

func (s *Server) handleAdminCreateOrg(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		httpError(w, http.StatusInternalServerError, "auth not configured")
		return
	}
	id, _ := auth.FromContext(r.Context())
	var req createOrgReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	ownerID := id.UserID
	if req.OwnerEmail != "" {
		u, err := s.authStore().GetUserByEmail(r.Context(), req.OwnerEmail)
		if err != nil {
			httpError(w, mapAuthErrorStatus(err), "owner: %s", err.Error())
			return
		}
		ownerID = u.ID
	}
	o, err := s.authSvc.CreateOrgFor(r.Context(), ownerID, req.Name, req.Slug)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditPlatform(r, o.ID, "org.created", "org", o.ID, map[string]any{"name": o.Name, "owner": ownerID})
	writeJSON(w, toOrgView(o))
}

// handleAdminDeleteOrg permanently removes an org and its identity-scoped
// children (teams, team + org memberships, pending invitations) via the
// service cascade. Super-admin only. Refuses the caller's active org so a
// switch is required first (no self-lockout). Team-scoped resources in other
// stores (runs, board, forge connections) are orphaned, not purged.
func (s *Server) handleAdminDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		httpError(w, http.StatusInternalServerError, "auth not configured")
		return
	}
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if orgID == id.OrgID {
		httpError(w, http.StatusConflict, "cannot delete your active organization — switch to another org first")
		return
	}
	// Capture the name for the audit log before the cascade removes it.
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if err := s.authSvc.DeleteOrgCascade(r.Context(), orgID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditPlatform(r, orgID, "org.deleted", "org", orgID, map[string]any{"name": o.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminGetOrg(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	o, err := store.GetOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, toOrgView(o))
}

func (s *Server) handleAdminUpdateOrg(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	o, err := store.GetOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	var req updateOrgReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		o.Name = *req.Name
	}
	if req.Slug != nil {
		o.Slug = *req.Slug
	}
	if !applyNonNegative(w, req.MonthlyRunQuota, &o.MonthlyRunQuota, "monthly_run_quota") {
		return
	}
	if !applyNonNegative(w, req.MemoryQuotaBytes, &o.MemoryQuotaBytes, "memory_quota_bytes") {
		return
	}
	if !applyNonNegative(w, req.MonthlyCostCapUSD, &o.MonthlyCostCapUSD, "monthly_cost_cap_usd") {
		return
	}
	o.UpdatedAt = time.Now().UTC()
	if err := store.UpdateOrg(r.Context(), o); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// Propagate a memory-quota change to the counter the CAS actually
	// enforces. The memory store is keyed per-team-tenant, so the org's
	// ceiling is pushed onto each of its teams. No-op for the FS store.
	if req.MemoryQuotaBytes != nil {
		if setter, ok := s.memoryStore().(tenantMemoryQuotaSetter); ok {
			quota := effectiveOrgMemoryQuota(o)
			teams, _ := store.ListTeamsByOrg(r.Context(), o.ID)
			for _, t := range teams {
				if err := setter.SetTenantQuota(r.Context(), t.ID, quota); err != nil && s.logger != nil {
					s.logger.Warn("admin: propagate memory quota for org %s team %s: %v", o.ID, t.ID, err)
				}
			}
		}
	}
	s.auditPlatform(r, o.ID, "org.updated", "org", o.ID, map[string]any{
		"monthly_run_quota": o.MonthlyRunQuota, "memory_quota_bytes": o.MemoryQuotaBytes,
		"monthly_cost_cap_usd": o.MonthlyCostCapUSD,
	})
	writeJSON(w, toOrgView(o))
}

// tenantMemoryQuotaSetter is the capability the cloud (Mongo) memory
// store implements so the org console can push a quota change onto the
// enforced counter. The FS store does not implement it.
type tenantMemoryQuotaSetter interface {
	SetTenantQuota(ctx context.Context, tenantID string, quotaBytes int64) error
}

func (s *Server) handleAdminSetOrgStatus(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	id, _ := auth.FromContext(r.Context())
	o, err := store.GetOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	var req setOrgStatusReq
	if !decodeJSON(w, r, &req) {
		return
	}
	st := identity.TeamStatus(req.Status)
	if !identity.ValidTeamStatus(st) {
		httpError(w, http.StatusBadRequest, "invalid status (active|suspended|read_only)")
		return
	}
	o.Status = st
	if st == identity.TeamStatusSuspended {
		now := time.Now().UTC()
		o.SuspendedAt = &now
		o.SuspendedBy = id.UserID
		o.SuspendReason = req.Reason
	} else {
		o.SuspendedAt = nil
		o.SuspendedBy = ""
		o.SuspendReason = ""
	}
	o.UpdatedAt = time.Now().UTC()
	if err := store.UpdateOrg(r.Context(), o); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if s.logger != nil {
		s.logger.Info("admin: org %s status -> %s by %s", o.ID, st, id.UserID)
	}
	s.auditPlatform(r, o.ID, "org.status_changed", "org", o.ID, map[string]any{"status": string(st), "reason": req.Reason})
	writeJSON(w, toOrgView(o))
}

func (s *Server) handleAdminOrgUsage(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	o, err := store.GetOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, s.buildOrgUsageView(r.Context(), store, o))
}

func (s *Server) handleAdminOrgTeams(w http.ResponseWriter, r *http.Request) {
	store, ok := s.authStoreOrFail(w)
	if !ok {
		return
	}
	teams, err := store.ListTeamsByOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]teamSummaryView, 0, len(teams))
	for _, t := range teams {
		views = append(views, toTeamSummaryView(t))
	}
	writeJSON(w, struct {
		Teams []teamSummaryView `json:"teams"`
	}{Teams: views})
}

// tenantMemoryUsageReader is the capability the cloud memory store
// implements for the per-tenant consumption readout. The FS store
// doesn't (local mode has no per-tenant aggregate).
type tenantMemoryUsageReader interface {
	TenantUsedBytes(ctx context.Context, tenantID string) (int64, error)
}

// buildOrgUsageView assembles the usage snapshot for one org. The
// monthly run/cost counters are ORG-keyed (the single source of truth);
// the resource counts are summed across every team in the org. Each
// sub-read is best-effort: a missing store or transient error leaves
// its field at zero rather than failing the whole view.
func (s *Server) buildOrgUsageView(ctx context.Context, st identity.Store, o identity.Org) orgUsageView {
	teams, _ := st.ListTeamsByOrg(ctx, o.ID)
	orgMembers, _ := st.ListOrgMembershipsByOrg(ctx, o.ID)
	v := orgUsageView{
		Org:                       toOrgView(o),
		Members:                   len(orgMembers),
		Teams:                     len(teams),
		EffectiveMemoryQuotaBytes: effectiveOrgMemoryQuota(o),
		MonthlyRunQuota:           orValue(o.MonthlyRunQuota, s.orgDefaults.MonthlyRunQuota),
		MonthlyCostCapUSD:         orValue(o.MonthlyCostCapUSD, s.orgDefaults.MonthlyCostCapUSD),
	}
	now := time.Now().UTC()

	// Org-keyed monthly counters: one read, keyed by org.ID.
	if s.orgUsage != nil {
		if u, err := s.orgUsage.Usage(ctx, o.ID, now); err == nil {
			v.RunsThisMonth = u.Runs
			v.CostUSDThisMonth = u.CostUSD
			v.InputTokens = u.InputTokens
			v.OutputTokens = u.OutputTokens
		}
	}
	if s.webhookCounter != nil {
		if n, err := s.webhookCounter.OrgCount(ctx, o.ID, now); err == nil {
			v.WebhookCallsThisMonth = n
		}
	}

	// Team-scoped resource counts: fan out per team and sum. Each
	// goroutine writes a distinct accumulator slot to avoid shared
	// mutation; we reduce after Wait.
	id, _ := auth.FromContext(ctx)
	type teamCounts struct {
		active, apiKeys, genericSecrets, botBindings, webhooks int
		memoryUsed                                             int64
	}
	counts := make([]teamCounts, len(teams))
	g, _ := errgroup.WithContext(ctx)
	for i := range teams {
		i := i
		t := teams[i]
		tctx := store.WithIdentity(ctx, t.ID, id.UserID)
		g.Go(func() error {
			if counter, ok := s.cfg.Store.(activeRunCounter); ok {
				if n, err := counter.CountActiveRunsByTenant(tctx, t.ID); err == nil {
					counts[i].active = n
				}
			}
			if reader, ok := s.memoryStore().(tenantMemoryUsageReader); ok {
				if n, err := reader.TenantUsedBytes(tctx, t.ID); err == nil {
					counts[i].memoryUsed = n
				}
			}
			if s.apiKeys != nil {
				if keys, err := s.apiKeys.ListByTeam(tctx, t.ID, ""); err == nil {
					counts[i].apiKeys = len(keys)
				}
			}
			if s.genericSecrets != nil {
				if secs, err := s.genericSecrets.ListByTeam(tctx, t.ID, ""); err == nil {
					counts[i].genericSecrets = len(secs)
				}
			}
			if s.botBindings != nil {
				if bs, err := s.botBindings.ListByTenant(tctx, t.ID); err == nil {
					counts[i].botBindings = len(bs)
				}
			}
			if s.webhookConfigs != nil {
				if whs, err := s.webhookConfigs.ListByTenant(tctx, t.ID); err == nil {
					counts[i].webhooks = len(whs)
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	for _, c := range counts {
		v.ActiveRuns += c.active
		v.APIKeyCount += c.apiKeys
		v.GenericSecretCount += c.genericSecrets
		v.BotBindingCount += c.botBindings
		v.WebhookCount += c.webhooks
		v.MemoryUsedBytes += c.memoryUsed
	}
	return v
}

// ---- launch suspend gate ----

// orgCanLaunch is the suspend-only gate decision, isolated for
// testability. The full launch admission (quotas, concurrency, rate)
// lives in gateLaunch (launch_gate.go), which folds this check in plus
// the org-level suspend. Returns true (allow) when there is no identity
// store (local mode), the caller is a super-admin, has no active team,
// or the team lookup fails (fail-open).
func orgCanLaunch(ctx context.Context, st identity.Store, id auth.Identity) bool {
	if st == nil || id.IsSuperAdmin || id.TeamID == "" {
		return true
	}
	t, err := st.GetTeam(ctx, id.TeamID)
	if err != nil {
		return true
	}
	return t.CanLaunch()
}
