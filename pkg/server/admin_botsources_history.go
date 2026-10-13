package server

import (
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/botsource"

	"github.com/SocialGouv/iterion/pkg/store"
)

// registerAdminBotSourceHistoryRoutes wires the super-admin purge of one
// stored bot's version history (#1517): the sensitive-deletion act the
// retention clock cannot answer on demand. Super-admin gated — a team's
// history outlives its members' reach — and audit-trailed, the count of
// snapshots removed in the audit's meta through the response.
func (s *Server) registerAdminBotSourceHistoryRoutes() {
	s.mux.Handle("DELETE /api/admin/tenants/{tenant}/bot-sources/{slug}/history", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPurgeBotSourceHistory)))
}

// handleAdminPurgeBotSourceHistory removes every snapshot of the named
// stored bot, the live row untouched: a pinned version the TTL would have
// removed anyway answers the same after this, and the deletion's REASON is
// the operator's, carried by the audit entry — not by the store.
func (s *Server) handleAdminPurgeBotSourceHistory(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant")
	slug := r.PathValue("slug")
	if tenantID == "" || slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing tenant or slug")
		return
	}
	ctx := store.WithTenant(r.Context(), tenantID)
	bs, err := s.botSources.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "stored bot %s/%s: %v", tenantID, slug, err)
		return
	}
	purged, err := s.botSources.PurgeHistory(ctx, tenantID, bs.ID)
	if errors.Is(err, botsource.ErrNotFound) {
		// An empty history is a normal answer — a second purge, a
		// pre-history row, or snapshots the TTL already swept (#1517).
		s.writeJSONFor(w, r, map[string]any{
			"bot":              bs.Slug,
			"tenant":           tenantID,
			"snapshots_purged": 0,
		})
		return
	}
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "purge history of %s/%s: %v", tenantID, slug, err)
		return
	}
	s.auditBotSource(r, tenantID, "history.purged", bs)
	s.writeJSONFor(w, r, map[string]any{
		"bot":              bs.Slug,
		"tenant":           tenantID,
		"snapshots_purged": purged,
	})
}
