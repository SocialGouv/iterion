package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

// Platform runtime-settings families beyond the usage caps: bot_roles and
// sandbox (pkg/platformcfg). Same doctrine — env/const = deployment
// default, DB record = runtime override, effective on every replica within
// the resolver TTL, super-admin API/CLI as the write surface.

// effectiveBotRoles is the resolved role→bot-id set every webhook consumer
// reads. The hardcoded constants remain the DEFAULTS; a platform record
// overrides field-by-field.
type effectiveBotRoles struct {
	Reviewer     string `json:"reviewer"`
	ReviConverse string `json:"revi_converse"`
	Brancher     string `json:"brancher"`
	Implementer  string `json:"implementer"`
}

// roleBots resolves the effective role bindings. Every site that used to
// read a role constant goes through here — re-pointing a role at another
// bot is a settings write, not a rollout. (The consts are referenced only
// as defaults; the symbol-sweep test enforces it.)
func (s *Server) roleBots() effectiveBotRoles {
	out := effectiveBotRoles{
		Reviewer:     defaultWebhookBotReviewPR,
		ReviConverse: defaultWebhookBotReviConverse,
		Brancher:     branchImproveBotID,
		Implementer:  featureDevBotID,
	}
	// The resolver's own fetchTimeout bounds the read; a request deadline
	// would add nothing but ctx-threading through every webhook helper.
	rec := s.botRoles.Get(context.Background())
	if rec == nil {
		return out
	}
	if rec.Reviewer != nil {
		out.Reviewer = *rec.Reviewer
	}
	if rec.ReviConverse != nil {
		out.ReviConverse = *rec.ReviConverse
	}
	if rec.Brancher != nil {
		out.Brancher = *rec.Brancher
	}
	if rec.Implementer != nil {
		out.Implementer = *rec.Implementer
	}
	return out
}

// effectiveSandboxImageSetting resolves the platform sandbox default-image
// override ("" = inherit env/built-in). Consumed by the cloud publisher,
// which pins the value on the RunMessage so redelivery reruns identically.
func (s *Server) effectiveSandboxImageSetting(ctx context.Context) string {
	return s.sandboxCfg.Get(ctx).EffectiveImage()
}

// registerAdminSettingsFamilyRoutes wires the bot_roles + sandbox families'
// admin surfaces, mirroring the usage-caps routes.
func (s *Server) registerAdminSettingsFamilyRoutes() {
	if s.authSvc == nil {
		return
	}
	if s.botRolesStore != nil {
		s.mux.Handle("GET /api/admin/settings/bot-roles", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetBotRoles)))
		s.mux.Handle("PUT /api/admin/settings/bot-roles", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutBotRoles)))
	}
	if s.sandboxCfgStore != nil {
		s.mux.Handle("GET /api/admin/settings/sandbox", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetSandboxSettings)))
		s.mux.Handle("PUT /api/admin/settings/sandbox", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutSandboxSettings)))
	}
}

func (s *Server) handleAdminGetBotRoles(w http.ResponseWriter, r *http.Request) {
	rec, err := s.botRolesStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	origin := "default"
	if rec != nil && (rec.Reviewer != nil || rec.ReviConverse != nil || rec.Brancher != nil || rec.Implementer != nil) {
		origin = "db"
	}
	s.writeJSONFor(w, r, map[string]any{
		"stored":    rec,
		"effective": s.roleBots(),
		"origin":    origin,
	})
}

// handleAdminPutBotRoles applies MERGE semantics like the caps route: a
// field absent from the body keeps its stored state; an explicit null
// clears the override.
func (s *Server) handleAdminPutBotRoles(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var patch map[string]*string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if len(patch) == 0 {
		// `null` / `{}` decode to an empty map; writing a record for them
		// would flip origin to "db" and forge an audit row for a no-op.
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty patch: name at least one field (reviewer|revi_converse|brancher|implementer, null to clear)")
		return
	}
	rec, err := s.botRolesStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if rec == nil {
		rec = &platformcfg.BotRoles{}
	}
	for field, v := range patch {
		switch field {
		case "reviewer":
			rec.Reviewer = v
		case "revi_converse":
			rec.ReviConverse = v
		case "brancher":
			rec.Brancher = v
		case "implementer":
			rec.Implementer = v
		default:
			s.httpErrorFor(w, r, http.StatusBadRequest, "unknown field %q (want reviewer|revi_converse|brancher|implementer)", field)
			return
		}
	}
	if err := rec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	rec.UpdatedBy = s.requestUserID(r)
	if err := s.botRolesStore.Put(r.Context(), *rec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	s.botRoles.Invalidate()
	s.auditPlatform(r, "", "platform.settings.bot_roles.updated", "platform_settings", platformcfg.FamilyBotRoles, map[string]any{
		"reviewer": strPtrOr(rec.Reviewer, ""), "revi_converse": strPtrOr(rec.ReviConverse, ""),
		"brancher": strPtrOr(rec.Brancher, ""), "implementer": strPtrOr(rec.Implementer, ""),
	})
	s.handleAdminGetBotRoles(w, r)
}

func (s *Server) handleAdminGetSandboxSettings(w http.ResponseWriter, r *http.Request) {
	rec, err := s.sandboxCfgStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	origin := "default"
	if rec != nil && rec.DefaultImage != nil {
		origin = "db"
	}
	s.writeJSONFor(w, r, map[string]any{
		"stored":                  rec,
		"effective_default_image": s.effectiveSandboxImageSetting(r.Context()),
		"origin":                  origin,
	})
}

func (s *Server) handleAdminPutSandboxSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var patch map[string]*string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if len(patch) == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty patch: name default_image (null to clear)")
		return
	}
	rec, err := s.sandboxCfgStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if rec == nil {
		rec = &platformcfg.Sandbox{}
	}
	for field, v := range patch {
		switch field {
		case "default_image":
			rec.DefaultImage = v
		default:
			s.httpErrorFor(w, r, http.StatusBadRequest, "unknown field %q (want default_image)", field)
			return
		}
	}
	if err := rec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	rec.UpdatedBy = s.requestUserID(r)
	if err := s.sandboxCfgStore.Put(r.Context(), *rec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	s.sandboxCfg.Invalidate()
	s.auditPlatform(r, "", "platform.settings.sandbox.updated", "platform_settings", platformcfg.FamilySandbox, map[string]any{
		"default_image": strPtrOr(rec.DefaultImage, ""),
	})
	s.handleAdminGetSandboxSettings(w, r)
}

func strPtrOr(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}
