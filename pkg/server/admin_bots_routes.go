package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform bot overrides — the DB-backed form of the catalog baked into the
// server/runner images, so iterating on a native bot (prompt tweak, skill
// fix, gate adjustment) is one API/CLI call instead of an image build +
// rollout. Deleting an override falls back to the baked catalog.
//
// Storage rides the existing bot-source store under the reserved sentinel
// tenant botsource.PlatformTenantID — same collection, same validation and
// compile check, zero schema change (the pattern admin_llm_routes.go
// established for platform LLM credentials). The handlers are thin wrappers:
// they scope the request to the platform sentinel and delegate to the shared
// bot-source cores; because every core reads and writes through that tenant
// scope, a super-admin route can never touch a team's row.
//
// Super-admin only: a platform override executes across ALL tenants — the
// same trust level as publishing a new image. Every mutation lands on the
// platform audit log with the bundle's content digest (auditBotSource).
func (s *Server) registerAdminBotRoutes() {
	if s.authSvc == nil || s.botSources == nil {
		return
	}
	s.mux.Handle("GET /api/admin/bots", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListPlatformBots)))
	s.mux.Handle("GET /api/admin/bots/{slug}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetPlatformBot)))
	s.mux.Handle("PUT /api/admin/bots/{slug}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutPlatformBot)))
	s.mux.Handle("DELETE /api/admin/bots/{slug}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeletePlatformBot)))
	s.mux.Handle("PUT /api/admin/bots/{slug}/files/{path...}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutPlatformBotFile)))
	s.mux.Handle("DELETE /api/admin/bots/{slug}/files/{path...}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeletePlatformBotFile)))
	s.mux.Handle("POST /api/admin/bots/{slug}/fork", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminForkPlatformBot)))
}

// platformBotReq re-scopes the store context to the sentinel platform tenant.
func (s *Server) platformBotReq(r *http.Request) *http.Request {
	return r.WithContext(store.WithTenant(r.Context(), botsource.PlatformTenantID))
}

// requestUserID returns the authenticated caller's user id ("" when absent).
func (s *Server) requestUserID(r *http.Request) string {
	id, _ := auth.FromContext(r.Context())
	return id.UserID
}

func (s *Server) handleAdminListPlatformBots(w http.ResponseWriter, r *http.Request) {
	s.listBotSourcesFor(w, s.platformBotReq(r), botsource.PlatformTenantID)
}

func (s *Server) handleAdminGetPlatformBot(w http.ResponseWriter, r *http.Request) {
	s.getBotSourceFor(w, s.platformBotReq(r), botsource.PlatformTenantID, r.PathValue("slug"))
}

func (s *Server) handleAdminPutPlatformBot(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	s.putBotSourceFor(w, s.platformBotReq(r), botsource.PlatformTenantID, s.requestUserID(r), r.PathValue("slug"))
}

func (s *Server) handleAdminDeletePlatformBot(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	s.deleteBotSourceFor(w, s.platformBotReq(r), botsource.PlatformTenantID, r.PathValue("slug"))
}

func (s *Server) handleAdminPutPlatformBotFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	s.putBotSourceFileFor(w, s.platformBotReq(r), botsource.PlatformTenantID, s.requestUserID(r), r.PathValue("slug"))
}

func (s *Server) handleAdminDeletePlatformBotFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	s.deleteBotSourceFileFor(w, s.platformBotReq(r), botsource.PlatformTenantID, s.requestUserID(r), r.PathValue("slug"))
}

func (s *Server) handleAdminForkPlatformBot(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	s.forkBotSourceFor(w, s.platformBotReq(r), botsource.PlatformTenantID, s.requestUserID(r), r.PathValue("slug"))
}
