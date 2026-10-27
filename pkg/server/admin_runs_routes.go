package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform runs view (super-admin, #2148): one listing of every org's
// runs, filterable by org and state, server-side paginated across
// orgs, each row naming its team and org. Read-only by design — no
// cross-org mutation exists on this surface. Opening ONE run rides the
// existing ADR-103 choke point (scopeRunByID), whose super-admin
// cross-team grant is audited by the same predicate this route uses.

// adminRunsLimitDefault / adminRunsLimitMax mirror the audit feed's
// page shape — one page convention across the super-admin console.
const (
	adminRunsLimitDefault = 50
	adminRunsLimitMax     = 500
)

// adminRunRow is one row of the cross-org runs view: the summary
// fields the console renders plus the identity the view exists to
// name — the run's team and its org, resolved server-side so a row
// never depends on the caller's active org.
type adminRunRow struct {
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	WorkflowName string          `json:"workflow_name"`
	BundleName   string          `json:"bundle_name,omitempty"`
	Status       store.RunStatus `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Error        string          `json:"error,omitempty"`
	SourceKind   string          `json:"source_kind,omitempty"`
	ProjectPath  string          `json:"project_path,omitempty"`
	TeamID       string          `json:"team_id,omitempty"`
	TeamName     string          `json:"team_name,omitempty"`
	OrgID        string          `json:"org_id,omitempty"`
	OrgName      string          `json:"org_name,omitempty"`
	// RunnerPool is the sovereign runner pool the run is bound to
	// (#2029). Non-empty rows stay visually marked in the console
	// (A19): instance-level reach crosses every boundary by default,
	// so the boundary must be visible where it is crossed.
	RunnerPool string `json:"runner_pool,omitempty"`
}

// adminRunsResponse is one page: the rows plus the keyset token the
// next page reads from. An absent next_cursor is the last page.
type adminRunsResponse struct {
	Runs       []adminRunRow `json:"runs"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// registerAdminRunRoutes mounts the platform runs view. The view reads
// the run store (s.runs), so a server without a run service has
// nothing to mount — the same guard as the /api/runs family.
func (s *Server) registerAdminRunRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.Handle("GET /api/admin/runs", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListRuns)))
}

// superAdminRunRead is THE named predicate every super-admin run
// content read crosses (A19, #2148): the cross-org list asks it once
// per request, and the run-by-id choke point (scopeRunByID) asks it
// for every cross-team grant. It takes the TEAM the read reaches —
// not a bare boolean — so the roadmap per-team shield
// (shield_from_instance_admin) slots in HERE, one code path, no second
// gate to forget. Today: true exactly for a super-admin, and every
// true answer is audited at platform scope (who reached which team,
// how). Run sockets re-ask standing per tick (runSocketStanding) and
// do not re-audit — the upgrade already crossed this predicate when
// scopeRunByID admitted it.
func (s *Server) superAdminRunRead(r *http.Request, id auth.Identity, teamID, action, targetID string, meta map[string]any) bool {
	if !id.IsSuperAdmin {
		return false
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["team_id"] = teamID
	meta["via"] = "super-admin"
	s.auditPlatform(r, teamID, action, "run", targetID, meta)
	return true
}

func (s *Server) handleAdminListRuns(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	filter := runview.AdminListFilter{}
	if status := q.Get("status"); status != "" {
		filter.Status = store.RunStatus(status)
	}
	if since := q.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid since (want RFC3339): %v", err)
			return
		}
		filter.Since = t
	}
	limit := adminRunsLimitDefault
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > adminRunsLimitMax {
		limit = adminRunsLimitMax
	}
	filter.Limit = limit
	cursor, err := runview.DecodeRunCursor(q.Get("cursor"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	filter.After = cursor

	// Org and team narrow the answer. They COMBINE: an org plus a team
	// outside it answers an empty page, never the org alone — a filter
	// set, not a fallback chain (#1419 was exactly the silent drop).
	orgID := q.Get("org_id")
	teamID := q.Get("team_id")
	orgNarrowed := false
	if orgID != "" {
		orgNarrowed = true
		if _, err := s.authStore().GetOrg(r.Context(), orgID); err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				s.httpErrorFor(w, r, http.StatusNotFound, "org not found: %s", orgID)
				return
			}
			s.httpErrorFor(w, r, http.StatusInternalServerError, "read org %s: %v", orgID, err)
			return
		}
		teams, err := s.authStore().ListTeamsByOrg(r.Context(), orgID)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list org %s teams: %v", orgID, err)
			return
		}
		if teamID != "" {
			inOrg := false
			for _, t := range teams {
				if t.ID == teamID {
					inOrg = true
					break
				}
			}
			if !inOrg {
				// The combination is honoured: this page is empty,
				// and the empty answer is still the audited read it
				// asked for.
				s.writeJSONFor(w, r, adminRunsResponse{Runs: []adminRunRow{}})
				return
			}
		}
		for _, t := range teams {
			filter.TeamIDs = append(filter.TeamIDs, t.ID)
		}
	}
	if teamID != "" && len(filter.TeamIDs) == 0 {
		filter.TeamIDs = []string{teamID}
	}
	if orgNarrowed && len(filter.TeamIDs) == 0 {
		// An org with zero teams: the filter set is empty, so the
		// honest page is empty — never the unfiltered everything an
		// empty TeamIDs set would otherwise mean.
		s.writeJSONFor(w, r, adminRunsResponse{Runs: []adminRunRow{}})
		return
	}

	// The predicate, once per request: WHO may read across orgs, and
	// the audit line that says this request did.
	scope := orgID
	if teamID != "" {
		scope = teamID
	}
	if !s.superAdminRunRead(r, id, scope, "run.cross_org_list", "list", map[string]any{
		"org_id": orgID, "team_id": teamID, "status": string(filter.Status),
	}) {
		httpError(w, http.StatusForbidden, "super-admin only")
		return
	}

	summaries, next, err := s.runs.ListAcrossTenants(r.Context(), filter)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list runs across tenants: %v", err)
		return
	}
	rows, err := s.nameAdminRunRows(r, summaries)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "resolve run teams: %v", err)
		return
	}
	s.writeJSONFor(w, r, adminRunsResponse{Runs: rows, NextCursor: next})
}

// nameAdminRunRows resolves the team and org names a row names, in
// bulk: one GetTeamsByIDs for the page's distinct tenants, one
// GetOrgsByIDs for their distinct orgs — never a lookup per row. A
// team or org deleted since the run launched leaves its name empty
// while the row stays: the run still exists and its id is still the
// truth.
func (s *Server) nameAdminRunRows(r *http.Request, summaries []runview.RunSummary) ([]adminRunRow, error) {
	st := s.authStore()
	teamIDs := make([]string, 0, len(summaries))
	seenTeams := make(map[string]bool, len(summaries))
	for _, su := range summaries {
		if su.TenantID == "" || seenTeams[su.TenantID] {
			continue
		}
		seenTeams[su.TenantID] = true
		teamIDs = append(teamIDs, su.TenantID)
	}
	teams := map[string]identity.Team{}
	if len(teamIDs) > 0 {
		byID, err := st.GetTeamsByIDs(r.Context(), teamIDs)
		if err != nil {
			return nil, err
		}
		teams = byID
	}
	orgIDs := make([]string, 0, len(teams))
	seenOrgs := map[string]bool{}
	for _, t := range teams {
		if t.OrgID == "" || seenOrgs[t.OrgID] {
			continue
		}
		seenOrgs[t.OrgID] = true
		orgIDs = append(orgIDs, t.OrgID)
	}
	orgs := map[string]identity.Org{}
	if len(orgIDs) > 0 {
		byID, err := st.GetOrgsByIDs(r.Context(), orgIDs)
		if err != nil {
			return nil, err
		}
		orgs = byID
	}
	rows := make([]adminRunRow, 0, len(summaries))
	for _, su := range summaries {
		row := adminRunRow{
			ID:           su.ID,
			Name:         su.Name,
			WorkflowName: su.WorkflowName,
			BundleName:   su.BundleName,
			Status:       su.Status,
			CreatedAt:    su.CreatedAt,
			UpdatedAt:    su.UpdatedAt,
			FinishedAt:   su.FinishedAt,
			Error:        su.Error,
			SourceKind:   su.SourceKind,
			ProjectPath:  su.ProjectPath,
			TeamID:       su.TenantID,
			RunnerPool:   su.RunnerPool,
		}
		if t, ok := teams[su.TenantID]; ok {
			row.TeamName = t.Name
			row.OrgID = t.OrgID
			if o, ok := orgs[t.OrgID]; ok {
				row.OrgName = o.Name
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
