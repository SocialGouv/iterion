package server

import (
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
)

// handleForgeConnectionRefresh re-probes a GitHub-App connection's live state
// and re-syncs the stored GrantedPermissions immediately, so an owner who just
// changed the App's permissions on the forge (e.g. granted Commit statuses:
// write for the merge gate) doesn't have to wait for the periodic refresh
// worker — or restart the server — for iterion to pick them up. It also forces
// a fresh installation-token mint so the last-minted-token observability
// reflects the new grants. The connection store is shared, so the re-synced
// grant is visible to every replica at once — no cross-pod cache to flush.
// Team-scoped, same auth as the connection health view.
func (s *Server) handleForgeConnectionRefresh(w http.ResponseWriter, r *http.Request) {
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
	if conn.Kind != forge.KindGitHubApp || conn.InstallationID == 0 {
		httpError(w, http.StatusBadRequest, "refresh applies to GitHub-App connections only")
		return
	}
	cfg, _, hasApp := s.githubAppConfigForConnection(r.Context(), conn)
	if !hasApp {
		httpError(w, http.StatusBadGateway, "no github app available for this connection")
		return
	}
	inst, err := forgegithub.InstallationInfo(r.Context(), s.forgeHTTPClient(),
		forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC())
	if err != nil {
		httpError(w, http.StatusBadGateway, "probe installation: %v", err)
		return
	}
	// Re-sync the stored grant AND the installation account with the live
	// ones (the mint reads the first, the security-read key the second).
	s.syncGrantedPermissions(r.Context(), conn, inst.Permissions, inst.Login)

	out := forgeConnectionHealth{
		Status:               string(conn.Status),
		StatusReason:         conn.StatusReason,
		Provider:             string(conn.Provider),
		Kind:                 string(conn.Kind),
		AccountLogin:         conn.AccountLogin,
		AppSlug:              conn.AppSlug,
		InstallationID:       conn.InstallationID,
		InstallationAccount:  inst.Login,
		ManageInstallURL:     inst.HTMLURL,
		GrantedPermissions:   inst.Permissions,
		MissingPermissions:   missingDeliveryFor(conn, inst.Permissions),
		MissingCIPermissions: missingCIFor(conn, inst.Permissions),
	}
	// Force a fresh token mint so the observability reflects the new grants
	// (forgeAdminFor builds a fresh client → rest() mints on first use).
	if admin, err := s.forgeAdminFor(r.Context(), conn); err == nil {
		if _, err := admin.ListRepos(r.Context(), forge.RepoQuery{}); err != nil {
			out.LiveError = err.Error()
		}
	}
	if tokenPerms, ok := forgegithub.LastMintedPermissions(conn.InstallationID); ok {
		out.TokenPermissions = tokenPerms
		out.TokenMissingPermissions = missingDeliveryFor(conn, tokenPerms)
	}
	writeJSON(w, out)
}
