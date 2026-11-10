// Admin team console: the super-admin surface for team-scoped settings an
// org admin must not touch. The sovereign runner-pool mapping lives here —
// a boundary field, not a preference, so it is written by exactly one route
// and audited on every write.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue"
)

func (s *Server) registerAdminTeamRoutes() {
	s.mux.Handle("PUT /api/admin/teams/{id}/runner-pool", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminSetTeamRunnerPool)))
	s.mux.Handle("PUT /api/admin/teams/{id}/llm-fallback", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminSetTeamLLMFallback)))
	s.registerRunnerPoolRoutes()
}

// handleAdminSetTeamRunnerPool maps (or unmaps) a team onto a sovereign
// runner pool. The ONLY writer of Team.RunnerPool: any other writer is a
// whole-document replace whose stale read would silently unmap the team.
// Body: {"runner_pool": ""} to unmap, {"runner_pool": "<name>"} to map.
// Empty means the shared default pool. Audited on every write.
func (s *Server) handleAdminSetTeamRunnerPool(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("id")
	var req struct {
		RunnerPool *string `json:"runner_pool"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RunnerPool == nil {
		httpError(w, http.StatusBadRequest, "runner_pool is required (a string; empty clears the mapping)")
		return
	}
	pool := *req.RunnerPool
	if pool != "" && !queue.ValidPoolName(pool) {
		httpError(w, http.StatusUnprocessableEntity, "runner_pool %q invalid (want 1–31 chars [a-z0-9-], starting alphanumeric)", pool)
		return
	}
	// The registry must KNOW the pool (any state — provisioning is fine to
	// map, the launch refuses until active). A registry not wired skips the
	// check here; the launch still refuses, so the boundary holds.
	if s.runnerPoolsStore != nil && pool != "" {
		reg, err := s.runnerPoolsStore.Get(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		if reg == nil || !reg.Exists(pool) {
			httpError(w, http.StatusUnprocessableEntity, "runner pool %q is not in the platform registry — create it first (PUT /api/admin/runner-pools)", pool)
			return
		}
	}
	cur, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// D13's one-team-per-pool invariant: two teams on one pool would let
	// each team's runs carry the other's credentials to the same pods.
	// The widening is a product decision, never a mapping accident —
	// refuse and name the holder; re-mapping a team onto its own pool is
	// fine. The check-then-act window is two super-admin writes in the
	// same instant: a partial unique index would close it at the store;
	// the sequential case — the realistic one — is refused here.
	if pool != "" {
		holder, err := s.authStore().GetTeamByRunnerPool(r.Context(), pool)
		if err != nil && !errors.Is(err, identity.ErrNotFound) {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		if err == nil && holder.ID != teamID {
			httpError(w, http.StatusConflict, "runner pool %q is already held by team %q (%s) — one team per pool; unmap that team first", pool, holder.Slug, holder.ID)
			return
		}
		// The same invariant from the fork lane's side (ADR-162 Q3): an
		// unmapped team's fork-lane config runs its untrusted runs on the
		// pool it names, and the runner admits a stamped run on its pool
		// alone. Mapping another team here would put that lane's queued,
		// running and redelivered fork runs — their code and their team's
		// sealed keys — on this team's sovereign pods, and refuse the lane's
		// every next gesture. The config is immutable, so the remedy is to
		// recreate it naming another pool before this mapping.
		lanes, err := s.forkLanesNamingPool(r.Context(), teamID, pool)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		if len(lanes) > 0 {
			httpError(w, http.StatusConflict, "runner pool %q is the fork-lane pool of %s — that lane's untrusted runs execute there, and mapping team %s onto it would put them on its sovereign pods; recreate those fork-lane webhooks naming another pool first", pool, strings.Join(lanes, ", "), teamID)
			return
		}
	}
	updated, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{RunnerPool: &pool})
	if err != nil {
		// The store-level backstop (the partial unique index) spoke: name
		// the holder the same way the pre-check would.
		if errors.Is(err, identity.ErrRunnerPoolHeld) {
			holder, herr := s.authStore().GetTeamByRunnerPool(r.Context(), pool)
			if herr == nil {
				httpError(w, http.StatusConflict, "runner pool %q is already held by team %q (%s) — one team per pool; unmap that team first", pool, holder.Slug, holder.ID)
				return
			}
			httpError(w, http.StatusConflict, "%s", err.Error())
			return
		}
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// A boundary field's trail must answer what the team WAS, not only what
	// it became. Scoped like every sibling team event: tenant trail for the
	// org's own audit view, org mirror so the org admin sees the change that
	// will refuse their resumes and launches.
	s.auditTenant(r, teamID, "team.runner_pool_set", "team", teamID, map[string]any{
		"runner_pool": pool, "previous": cur.RunnerPool,
	})
	if cur.OrgID != "" {
		s.auditOrg(r, cur.OrgID, "team.runner_pool_set", "team", teamID, map[string]any{
			"runner_pool": pool, "previous": cur.RunnerPool,
		})
	}
	writeJSON(w, toTeamSummaryView(updated))
}

// forkLanesNamingPool lists, sorted, the fork-lane webhook configs of every
// team but teamID whose runs execute on pool (webhooks.Config.ForkLanePool),
// each rendered "webhook config <id> (team <id>)". Every team is walked, page
// by page, and a disabled config counts: it can be re-enabled, and its pool
// cannot be edited. No webhook store wired means no fork lane exists.
func (s *Server) forkLanesNamingPool(ctx context.Context, teamID, pool string) ([]string, error) {
	if s.webhookConfigs == nil || pool == "" {
		return nil, nil
	}
	const pageSize = 1000
	var lanes []string
	for offset := 0; ; offset += pageSize {
		teams, err := s.authStore().ListTeams(ctx, identity.Page{Offset: offset, Limit: pageSize})
		if err != nil {
			return nil, fmt.Errorf("list teams: %w", err)
		}
		for _, tm := range teams {
			if tm.ID == teamID {
				continue
			}
			cfgs, err := s.webhookConfigs.ListByTenant(ctx, tm.ID)
			if err != nil {
				return nil, fmt.Errorf("list team %s's webhook configs: %w", tm.ID, err)
			}
			for _, c := range cfgs {
				if c.ForkLane && c.ForkLanePool == pool {
					lanes = append(lanes, fmt.Sprintf("webhook config %s (team %s)", c.ID, tm.ID))
				}
			}
		}
		if len(teams) < pageSize {
			break
		}
	}
	sort.Strings(lanes)
	return lanes, nil
}

// handleAdminSetTeamLLMFallback sets (or resets) a team's credential-fallback
// policy. The ONLY writer of Team.LLMFallback, the same rule as the pool
// mapping above: any other writer is a whole-document replace whose stale
// read would silently reset the policy. Body: {"llm_fallback": "none"} for
// the sovereign posture (the team's own credentials only, an unfunded route
// refuses the launch), "" or "platform" for the shared tiers (the default).
// Audited on every write.
func (s *Server) handleAdminSetTeamLLMFallback(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("id")
	var req struct {
		LLMFallback *string `json:"llm_fallback"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LLMFallback == nil {
		httpError(w, http.StatusBadRequest, `llm_fallback is required (empty or "platform" for the shared tiers, "none" for the sovereign posture)`)
		return
	}
	policy := *req.LLMFallback
	if !identity.ValidLLMFallback(policy) {
		httpError(w, http.StatusUnprocessableEntity, `llm_fallback %q invalid (want empty, "platform", or "none")`, policy)
		return
	}
	cur, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	updated, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{LLMFallback: &policy})
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// Same dual-scope trail as the pool mapping: the tenant row keyed by the
	// team, the org mirror keyed by the org, each carrying the previous
	// value.
	s.auditTenant(r, teamID, "team.llm_fallback_set", "team", teamID, map[string]any{
		"llm_fallback": policy, "previous": cur.LLMFallback,
	})
	if cur.OrgID != "" {
		s.auditOrg(r, cur.OrgID, "team.llm_fallback_set", "team", teamID, map[string]any{
			"llm_fallback": policy, "previous": cur.LLMFallback,
		})
	}
	writeJSON(w, toTeamSummaryView(updated))
}

// registerRunnerPoolRoutes wires the super-admin registry console. No
// registry store wired = no pools exist (the routes 503 rather than pretend).
func (s *Server) registerRunnerPoolRoutes() {
	if s.runnerPoolsStore == nil {
		return
	}
	s.mux.Handle("GET /api/admin/runner-pools", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetRunnerPools)))
	s.mux.Handle("PUT /api/admin/runner-pools", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutRunnerPools)))
}

// handleAdminGetRunnerPools answers the stored registry (null = none yet).
func (s *Server) handleAdminGetRunnerPools(w http.ResponseWriter, r *http.Request) {
	rec, err := s.runnerPoolsStore.Get(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, rec)
}

// handleAdminPutRunnerPools replaces the registry WHOLESALE — entries are
// small and the lifecycle is explicit per entry (state), so merge semantics
// would only blur who set which state when. Validated (grammar, duplicates,
// states), audited, and the mapping route refuses pools the registry does
// not know.
func (s *Server) handleAdminPutRunnerPools(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pools []platformcfg.RunnerPool `json:"pools"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	rec := platformcfg.RunnerPools{Pools: req.Pools}
	if err := rec.Validate(); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	rec.UpdatedBy = s.requestUserID(r)
	if err := s.runnerPoolsStore.Put(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.auditPlatform(r, "", "platform.settings.runner_pools.updated", "platform_settings", platformcfg.FamilyRunnerPools, map[string]any{
		"pools": len(rec.Pools),
	})
	writeJSON(w, rec)
}
