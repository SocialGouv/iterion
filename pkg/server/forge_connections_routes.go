package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---- handlers ----

func (s *Server) handleListForgeConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.forgeConnections.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.Connection{}
	}
	writeJSON(w, struct {
		Connections []forge.Connection `json:"connections"`
	}{Connections: list})
}

// forgeTeamRepo is one row of the team-wide connected-repo aggregator: a
// repo the team holds a RepoIntegration for, joined with its connection
// so the client renders provider + URLs without a per-connection fan-out.
type forgeTeamRepo struct {
	ConnectionID      string   `json:"connection_id"`
	ConnectionStatus  string   `json:"connection_status,omitempty"`
	Provider          string   `json:"provider"`
	RepoFullName      string   `json:"repo_full_name"`
	CloneURL          string   `json:"clone_url,omitempty"`
	WebURL            string   `json:"web_url,omitempty"`
	IntegrationID     string   `json:"integration_id"`
	BotIDs            []string `json:"bot_ids"`
	SyncIssuesEnabled bool     `json:"sync_issues_enabled"`
}

// handleListTeamForgeRepos is the RepoSwitcher's data source: the team's
// CONNECTED repos (one row per RepoIntegration) in a single call.
// Discovering not-yet-connected repos stays on the per-connection
// /connections/{conn_id}/repos picker. Absent forge stores (local mode)
// yield an empty list, not an error — the switcher just shows its CTA.
func (s *Server) handleListTeamForgeRepos(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	repos := []forgeTeamRepo{}
	if s.forgeConnections != nil && s.forgeIntegrations != nil {
		conns, err := s.forgeConnections.ListByTenant(r.Context(), teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		ints, err := s.forgeIntegrations.ListByTenant(r.Context(), teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		byConn := make(map[string]forge.Connection, len(conns))
		for _, c := range conns {
			byConn[c.ID] = c
		}
		for _, it := range ints {
			row := forgeTeamRepo{
				ConnectionID:      it.ConnectionID,
				Provider:          string(it.Provider),
				RepoFullName:      it.RepoFullName,
				IntegrationID:     it.ID,
				BotIDs:            it.BotIDs,
				SyncIssuesEnabled: it.SyncIssuesEnabled,
			}
			if row.BotIDs == nil {
				row.BotIDs = []string{}
			}
			if c, ok := byConn[it.ConnectionID]; ok {
				row.ConnectionStatus = string(c.Status)
				row.CloneURL = forge.CloneURLFor(c.BaseURL(), it.RepoFullName)
				row.WebURL = forge.WebURLFor(c.BaseURL(), it.RepoFullName)
			}
			repos = append(repos, row)
		}
		sort.Slice(repos, func(i, j int) bool {
			if repos[i].Provider != repos[j].Provider {
				return repos[i].Provider < repos[j].Provider
			}
			return repos[i].RepoFullName < repos[j].RepoFullName
		})
	}
	writeJSON(w, struct {
		Repos []forgeTeamRepo `json:"repos"`
	}{Repos: repos})
}

// forgeConnectionHealth is the connection card's actionable state: the
// stored status/reason plus, for a GitHub App, the installation's LIVE
// repo scope and its settings URL — so the UI can explain "the repo you
// want isn't covered by the installation" and deep-link where to widen
// it, instead of dead-ending on an empty repo search.
type forgeConnectionHealth struct {
	Status               string   `json:"status"`
	StatusReason         string   `json:"status_reason,omitempty"`
	Provider             string   `json:"provider"`
	Kind                 string   `json:"kind"`
	AccountLogin         string   `json:"account_login,omitempty"`
	AppSlug              string   `json:"app_slug,omitempty"`
	InstallationID       int64    `json:"installation_id,omitempty"`
	InstallationAccount  string   `json:"installation_account,omitempty"`
	ProvisionedRepoCount int      `json:"provisioned_repo_count"`
	InstallationRepos    []string `json:"installation_repos,omitempty"`
	// ManageInstallURL is the forge-side page where the operator widens
	// the installation (repo scope + permission grants). GitHub has no
	// API for this — the link is the fix.
	ManageInstallURL string `json:"manage_install_url,omitempty"`
	// LiveError reports a failed live probe (token mint / API call)
	// without failing the endpoint — the stored status is still useful.
	LiveError string `json:"live_error,omitempty"`
	// GrantedPermissions is what the installation's owner actually approved.
	// MissingPermissions names the delivery grants it lacks — publishing a CI
	// workflow or an image is refused outright without them, and that refusal
	// otherwise surfaces only at push time, hours into a run.
	GrantedPermissions map[string]string `json:"granted_permissions,omitempty"`
	MissingPermissions []string          `json:"missing_permissions,omitempty"`
	// TokenPermissions is what the most recently minted token actually
	// CARRIED. It can be NARROWER than GrantedPermissions — a token pinned to
	// a permission subset is exactly that — and it is the token that acts, so
	// this is the field a pre-flight should read. Absent until this process
	// has minted one for the installation.
	TokenPermissions        map[string]string `json:"token_permissions,omitempty"`
	TokenMissingPermissions []string          `json:"token_missing_permissions,omitempty"`
	// SecurityReadEnabled mirrors the connection's opt-in into the org-wide
	// Dependabot-alerts token flow; MissingSecurityPermissions names the
	// grant it would need (vulnerability_alerts) when the installation
	// hasn't approved it — the fix is the ManageInstallURL page.
	SecurityReadEnabled        bool     `json:"security_read_enabled"`
	MissingSecurityPermissions []string `json:"missing_security_permissions,omitempty"`
}

// syncGrantedPermissions persists the installation's live grant onto the
// connection when it moved. Best-effort: the health view is already correct
// from the live probe, and the stored copy only optimises the mint — a failed
// write must not fail the endpoint.
func (s *Server) syncGrantedPermissions(ctx context.Context, conn forge.Connection, live map[string]string, installAccount string) {
	sameAccount := installAccount == "" || conn.InstallationAccount == installAccount
	if s.forgeConnections == nil || (sameAccount && (len(live) == 0 || samePermissions(conn.GrantedPermissions, live))) {
		return
	}
	if len(live) > 0 {
		conn.GrantedPermissions = live
	}
	// Also the repair path for connections created before the field existed:
	// without it their security-read token could never be keyed by org.
	if installAccount != "" {
		conn.InstallationAccount = installAccount
	}
	conn.UpdatedAt = time.Now().UTC()
	if err := s.forgeConnections.Update(store.WithTenant(ctx, conn.TenantID), conn); err != nil && s.logger != nil {
		s.logger.Warn("forge: persist granted permissions for connection %s: %v", conn.ID, err)
	}
}

func samePermissions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (s *Server) handleForgeConnectionHealth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	h := forgeConnectionHealth{
		Status:              string(conn.Status),
		StatusReason:        conn.StatusReason,
		Provider:            string(conn.Provider),
		Kind:                string(conn.Kind),
		AccountLogin:        conn.AccountLogin,
		AppSlug:             conn.AppSlug,
		InstallationID:      conn.InstallationID,
		SecurityReadEnabled: conn.SecurityReadEnabled,
	}
	if names, err := s.forgeConnRepoNames(r.Context(), conn); err == nil {
		h.ProvisionedRepoCount = len(names)
	}
	if conn.Kind == forge.KindGitHubApp && conn.InstallationID != 0 {
		if cfg, _, ok := s.githubAppConfigForConnection(r.Context(), conn); ok {
			inst, err := forgegithub.InstallationInfo(r.Context(), s.forgeHTTPClient(),
				forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC())
			if err != nil {
				h.LiveError = err.Error()
			} else {
				h.InstallationAccount = inst.Login
				h.ManageInstallURL = inst.HTMLURL
				h.GrantedPermissions = inst.Permissions
				// A watch-only connection has NO delivery grants on purpose —
				// reporting them as missing would tell an operator to "fix" the
				// one property that makes this App safe to install org-wide.
				if !conn.IsSecurityReadOnly() {
					h.MissingPermissions = forgegithub.MissingDeliveryPermissions(inst.Permissions)
				}
				h.MissingSecurityPermissions = forgegithub.MissingSecurityPermissions(inst.Permissions)
				// Keep the stored grant in step with the live one: the mint
				// reads it, and an owner may approve (or revoke) a permission
				// long after the install.
				s.syncGrantedPermissions(r.Context(), conn, inst.Permissions, inst.Login)
			}
			// What the installation GRANTED and what a token CARRIES are two
			// different things, and reading the first as the second cost a full
			// run: the installation had `workflows`, the minted token did not
			// (it was pinned to the baseline), and this view reported all-clear
			// while the push was refused an hour later. Report the token's own
			// permissions so the pre-flight looks at the thing that acts.
			if tokenPerms, ok := forgegithub.LastMintedPermissions(conn.InstallationID); ok {
				h.TokenPermissions = tokenPerms
				h.TokenMissingPermissions = forgegithub.MissingDeliveryPermissions(tokenPerms)
			}
		}
		if admin, err := s.forgeAdminFor(r.Context(), conn); err == nil {
			if repos, err := admin.ListRepos(r.Context(), forge.RepoQuery{}); err == nil {
				names := make([]string, 0, len(repos))
				for i, rp := range repos {
					if i >= 100 {
						break
					}
					names = append(names, rp.FullName)
				}
				h.InstallationRepos = names
			} else if h.LiveError == "" {
				h.LiveError = err.Error()
			}
		}
	}
	writeJSON(w, h)
}

type forgeCreateRepoReq struct {
	ConnectionID  string `json:"connection_id"`
	Owner         string `json:"owner,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Private       *bool  `json:"private,omitempty"` // nil = private
	DefaultBranch string `json:"default_branch,omitempty"`
	InitReadme    bool   `json:"init_readme,omitempty"`
}

// handleCreateForgeRepo creates a NEW repository on a connected forge —
// the "new app → new repo" launch journey. Creation only: iterion never
// updates or deletes forge repositories. GitHub App connections mint a
// per-call administration:write token (see AppClient.CreateRepo); an
// installation whose grant lacks Administration surfaces the actionable
// 422 instead of silently failing.
func (s *Server) handleCreateForgeRepo(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	var req forgeCreateRepoReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || req.ConnectionID == "" {
		httpError(w, http.StatusBadRequest, "connection_id and name are required")
		return
	}
	// Tenant pin: a connection_id from another team 404s (never 403 —
	// same non-enumeration discipline as forgeConnForTenant everywhere).
	conn, ok := s.forgeConnForTenant(w, r, teamID, req.ConnectionID)
	if !ok {
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	creator, ok := admin.(forge.RepoCreator)
	if !ok {
		httpError(w, http.StatusNotImplemented, "this connection's provider/credential cannot create repositories")
		return
	}
	private := true
	if req.Private != nil {
		private = *req.Private
	}
	sum, err := creator.CreateRepo(r.Context(), forge.RepoCreateSpec{
		Owner:         req.Owner,
		Name:          req.Name,
		Description:   req.Description,
		Private:       private,
		DefaultBranch: req.DefaultBranch,
		InitReadme:    req.InitReadme,
	})
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrRepoExists):
			httpError(w, http.StatusConflict, "%v", err)
		case errors.Is(err, forge.ErrPermissionsNotGranted):
			httpError(w, http.StatusUnprocessableEntity, "the GitHub App installation lacks the Administration permission — approve the App's pending permission update on GitHub, then retry: %v", err)
		default:
			httpError(w, http.StatusBadGateway, "create repository: %v", err)
		}
		return
	}
	s.auditTenant(r, teamID, "forge.repo.created", "forge_repo", sum.FullName, map[string]any{
		"provider": string(conn.Provider), "connection_id": conn.ID, "private": private,
	})
	writeJSON(w, struct {
		Repo     forge.RepoSummary `json:"repo"`
		CloneURL string            `json:"clone_url"`
	}{Repo: sum, CloneURL: forge.CloneURLFor(conn.BaseURL(), sum.FullName)})
}

func (s *Server) handleDeleteForgeConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	connID := r.PathValue("conn_id")
	ctx := store.WithTenant(r.Context(), teamID)
	if err := s.forgeOrchestrator.DeprovisionConnection(ctx, teamID, connID); err != nil {
		if errors.Is(err, forge.ErrConnectionNotFound) {
			httpError(w, http.StatusNotFound, "connection not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "disconnect failed: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.deleted", "forge_connection", connID, nil)
	w.WriteHeader(http.StatusNoContent)
}

type forgeConnectionPatchReq struct {
	// SecurityReadEnabled toggles the org-wide Dependabot-alerts token flow
	// for this github_app connection (see forge.SecurityReadSecretName).
	SecurityReadEnabled *bool `json:"security_read_enabled,omitempty"`
}

// handlePatchForgeConnection updates a connection's operator-tunable flags.
// Enabling security-read mints the token IMMEDIATELY (not at the next
// refresh tick) so the operator learns on the spot whether the installation
// granted 'Dependabot alerts: read' — a missing grant answers 422 with the
// remediation named, and nothing is persisted. Disabling removes the
// connection's org entry from the dependabot_tokens secret.
func (s *Server) handlePatchForgeConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	var req forgeConnectionPatchReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SecurityReadEnabled == nil {
		httpError(w, http.StatusBadRequest, "nothing to update: security_read_enabled is the only patchable field")
		return
	}
	enable := *req.SecurityReadEnabled
	ctx := store.WithTenant(r.Context(), teamID)
	if enable {
		if conn.Kind != forge.KindGitHubApp {
			httpError(w, http.StatusUnprocessableEntity, "security-read requires a github_app connection (this one is %s); a non-App deployment can set the %q team secret by hand instead", conn.Kind, forge.SecurityReadSecretName)
			return
		}
		// A personal secret of the same name OUTRANKS the team map at
		// resolution (user > team), so enabling here would look fine while
		// every run of that member reads a different token map. Name the
		// conflict instead of minting into a shadowed secret.
		if owner, clash, err := s.securityReadNameClash(ctx, teamID); err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		} else if clash {
			httpError(w, http.StatusConflict, "a personal secret named %q already exists in this team (owner %s) and would shadow the managed map for that user's runs — remove it first", forge.SecurityReadSecretName, owner)
			return
		}
		// The map is keyed by ORG alone (the bot's config names orgs, not
		// hosts). Two connections claiming the same org on different forge
		// hosts would overwrite each other every cycle, and a private
		// instance's token could end up filed where the public one is read.
		if other, err := s.securityReadOrgCollision(ctx, conn); err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		} else if other != "" {
			httpError(w, http.StatusConflict, "connection %s (host %s) already holds the security-read token for org %q; the token map is keyed by org, so both cannot be enabled — disable that one first", other, conn.Host(), securityReadOrgOf(conn))
			return
		}
		mint := s.forgeSecurityMint
		if mint == nil {
			mint = s.forgeSecurityTokenMinter
		}
		tok, _, err := mint(ctx, conn)
		if err != nil {
			if errors.Is(err, forge.ErrPermissionsNotGranted) {
				httpError(w, http.StatusUnprocessableEntity, "%v", err)
				return
			}
			httpError(w, http.StatusBadGateway, "security-read token mint: %v", err)
			return
		}
		if err := forge.UpsertSecurityReadToken(ctx, s.genericSecrets, s.sealer, &conn, tok, time.Now().UTC()); err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
	} else if conn.Kind == forge.KindGitHubApp {
		// Unconditional for an App connection (the only kind that can hold a
		// minted entry): the stored flag and the secret map can disagree —
		// a failed persist after a successful mint — and gating the
		// withdrawal on the flag would answer 200 while leaving the org
		// token in place. RemoveSecurityReadToken is idempotent on absence.
		//
		// Scoped to that kind, though: a pat/oauth connection can never have
		// minted anything, and its AccountLogin IS an org name — withdrawing
		// on it would delete that org's entry from the operator's own
		// hand-set map, which is the documented non-App path.
		if err := forge.RemoveSecurityReadToken(ctx, s.genericSecrets, s.sealer, &conn); err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	conn.SecurityReadEnabled = enable
	conn.UpdatedAt = time.Now().UTC()
	if err := s.forgeConnections.Update(ctx, conn); err != nil {
		// The token is already in the map but the flag says otherwise, so no
		// lifecycle owns it: the worker skips it, the withdrawal skips it,
		// deprovision skips it — it dies in ~1h and the hourly bot then
		// fails on a 401 with no trail. Roll it back.
		if enable {
			if rerr := forge.RemoveSecurityReadToken(ctx, s.genericSecrets, s.sealer, &conn); rerr != nil && s.logger != nil {
				s.logger.Warn("forge: could not roll back the security-read token for %s after a failed persist: %v", conn.ID, rerr)
			}
		}
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.security_read", "forge_connection", conn.ID, map[string]any{
		"enabled": enable, "account": securityReadOrgOf(conn),
	})
	writeJSON(w, conn)
}

func (s *Server) handleListForgeRepos(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	repos, err := admin.ListRepos(r.Context(), forge.RepoQuery{
		Search: r.URL.Query().Get("search"),
		Page:   page,
	})
	if err != nil {
		if errors.Is(err, forge.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "connection credential rejected — reconnect")
			return
		}
		httpError(w, http.StatusBadGateway, "list repos: %v", err)
		return
	}
	if repos == nil {
		repos = []forge.RepoSummary{}
	}
	writeJSON(w, struct {
		Repos []forge.RepoSummary `json:"repos"`
	}{Repos: repos})
}

// securityReadOrgOf is the ORG a connection operates on, lowercased. For a
// github_app connection that is the installation account — NOT AccountLogin,
// which holds the App's own bot handle and is identical across every
// connection minted from one App. Reading the wrong one both invents
// collisions between different orgs and misses the real cross-host one.
func securityReadOrgOf(c forge.Connection) string {
	if c.Kind == forge.KindGitHubApp {
		return strings.ToLower(strings.TrimSpace(c.InstallationAccount))
	}
	return strings.ToLower(strings.TrimSpace(c.AccountLogin))
}

// securityReadNameClash reports whether a USER-scoped secret named
// dependabot_tokens exists in the team. Resolution ranks user > team, so such
// a secret silently outranks the managed map for that member's runs — the
// enable endpoint refuses rather than mint into something shadowed.
func (s *Server) securityReadNameClash(ctx context.Context, teamID string) (string, bool, error) {
	// Optional capability: ListByTeam is a per-USER resolution view and
	// cannot see other members' personal secrets. Stores without the
	// whole-team view (the single-user local file store) skip the check.
	lister, ok := s.genericSecrets.(interface {
		ListAllInTeam(context.Context, string) ([]secrets.GenericSecret, error)
	})
	if !ok {
		return "", false, nil
	}
	list, err := lister.ListAllInTeam(ctx, teamID)
	if err != nil {
		return "", false, fmt.Errorf("list team secrets: %w", err)
	}
	for _, sec := range list {
		if sec.Name == forge.SecurityReadSecretName && sec.ScopeUserID != "" {
			return sec.ScopeUserID, true, nil
		}
	}
	return "", false, nil
}

// securityReadOrgCollision returns the id of another security-read-enabled
// connection of the same tenant that claims the SAME org login on a
// DIFFERENT forge host, or "".
//
// Same-host duplicates are deliberately NOT refused: two Apps installed on
// one org mint interchangeable tokens for the same alert data, so they
// overwrite each other harmlessly (the cost is a withdrawal when either is
// disabled, and a re-mint on the next tick). The cross-host case is the
// dangerous one — the map is keyed by org alone, so a private instance's
// token could end up filed where the public one is read.
//
// This is a read-then-write check, so two admins enabling concurrently on
// two replicas can both pass it; it narrows a misconfiguration, it is not
// an invariant.
func (s *Server) securityReadOrgCollision(ctx context.Context, conn forge.Connection) (string, error) {
	org := securityReadOrgOf(conn)
	if org == "" || s.forgeConnections == nil {
		return "", nil
	}
	list, err := s.forgeConnections.ListByTenant(ctx, conn.TenantID)
	if err != nil {
		return "", fmt.Errorf("list connections: %w", err)
	}
	for _, other := range list {
		if other.ID == conn.ID || !other.SecurityReadEnabled {
			continue
		}
		if securityReadOrgOf(other) == org && other.Host() != conn.Host() {
			return other.ID, nil
		}
	}
	return "", nil
}

// forgeConnForTenant fetches a connection and asserts tenant ownership.
func (s *Server) forgeConnForTenant(w http.ResponseWriter, r *http.Request, teamID, connID string) (forge.Connection, bool) {
	conn, err := s.forgeConnections.Get(r.Context(), connID)
	if err != nil || conn.TenantID != teamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return forge.Connection{}, false
	}
	return conn, true
}
