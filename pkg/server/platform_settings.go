package server

import (
	"context"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"net/http"
	"time"

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
	if s.botVarsStore != nil {
		s.mux.Handle("GET /api/admin/settings/bot-vars", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetBotVars)))
		s.mux.Handle("PUT /api/admin/settings/bot-vars", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutBotVars)))
	}
	if s.platformCredsStore != nil {
		s.mux.Handle("GET /api/admin/settings/platform-credentials", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetPlatformCredentials)))
		s.mux.Handle("PUT /api/admin/settings/platform-credentials", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutPlatformCredentials)))
	}
	if s.budgetFloorStore != nil {
		s.mux.Handle("GET /api/admin/settings/budget-floor", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminGetBudgetFloor)))
		s.mux.Handle("PUT /api/admin/settings/budget-floor", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminPutBudgetFloor)))
	}
}

func (s *Server) handleAdminGetBudgetFloor(w http.ResponseWriter, r *http.Request) {
	rec, err := s.budgetFloorStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	var pol budgetfloor.Policy
	origin := "default"
	if rec != nil {
		pol = *rec
		if len(pol.Reservations) > 0 || len(pol.RepoQuotas) > 0 {
			origin = "db"
		}
	}
	s.writeJSONFor(w, r, map[string]any{
		"stored": pol,
		"origin": origin,
		// The reserved bot ids, sorted — what an operator scans for before
		// asking why a bot is being held back.
		"reserved_bots": pol.Bots(),
	})
}

// handleAdminPutBudgetFloor REPLACES the policy, unlike its merge-semantics
// siblings. A reservation set is read as a whole — "these workloads hold
// these bands" — and merging per key would make removing one reservation
// impossible without a null-for-every-field dance. The read-modify-write is
// the operator's, and the CAS token on the record is what stops two admins
// from silently dropping each other's edits.
func (s *Server) handleAdminPutBudgetFloor(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var pol budgetfloor.Policy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&pol); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	// The client does not get to stamp the CAS token.
	pol.UpdatedAt = time.Time{}
	// Validated BEFORE it is stored: a policy whose reservations sum past the
	// window, or whose repository takes a share of something unshareable, is
	// refused where the operator can still fix it — not resolved to a silent
	// zero at the gate hours later.
	if err := pol.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	if err := s.budgetFloorStore.Put(r.Context(), pol); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	// Reach this replica's own gates now instead of at the TTL — the same
	// courtesy the other families extend.
	s.budgetFloor.Invalidate()
	s.handleAdminGetBudgetFloor(w, r)
}

func (s *Server) handleAdminGetPlatformCredentials(w http.ResponseWriter, r *http.Request) {
	rec, err := s.platformCredsStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	origin := "default"
	if rec != nil && (rec.Enforce != nil || len(rec.Teams) > 0 || len(rec.Orgs) > 0) {
		origin = "db"
	}
	s.writeJSONFor(w, r, map[string]any{
		"stored":   rec,
		"enforced": rec.Enforced(),
		"origin":   origin,
	})
}

// handleAdminPutPlatformCredentials applies MERGE semantics like its
// siblings: a field absent from the body keeps its stored state, an
// explicit null clears it.
//
// Validate refuses enforcing an audience that names nobody — a state
// reachable by accident (enable enforcement, forget the lists) whose
// symptom is every credential-less run failing at its first LLM call.
func (s *Server) handleAdminPutPlatformCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var patch struct {
		Enforce *bool     `json:"enforce,omitempty"`
		Teams   *[]string `json:"teams,omitempty"`
		Orgs    *[]string `json:"orgs,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if patch.Enforce == nil && patch.Teams == nil && patch.Orgs == nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty patch: name at least one field (enforce|teams|orgs)")
		return
	}
	rec, err := s.platformCredsStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if rec == nil {
		rec = &platformcfg.PlatformCredentials{}
	}
	if patch.Enforce != nil {
		rec.Enforce = patch.Enforce
	}
	if patch.Teams != nil {
		rec.Teams = *patch.Teams
	}
	if patch.Orgs != nil {
		rec.Orgs = *patch.Orgs
	}
	if err := rec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	rec.UpdatedBy = s.requestUserID(r)
	if err := s.platformCredsStore.Put(r.Context(), *rec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if s.platformCreds != nil {
		s.platformCreds.Invalidate()
	}
	s.auditPlatform(r, "", "platform.settings.platform_credentials.updated", "platform_settings", platformcfg.FamilyPlatformCredentials, map[string]any{
		"enforce": rec.Enforced(), "teams": rec.Teams, "orgs": rec.Orgs,
	})
	s.handleAdminGetPlatformCredentials(w, r)
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

func (s *Server) handleAdminGetBotVars(w http.ResponseWriter, r *http.Request) {
	rec, err := s.botVarsStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	origin := "default"
	if rec != nil && len(rec.Vars) > 0 {
		origin = "db"
	}
	s.writeJSONFor(w, r, map[string]any{
		"stored": rec,
		"origin": origin,
		// The bound every replica converges within after a write — the
		// operator-facing answer to "when does my var take effect".
		"propagation_bound_seconds": int(platformcfg.DefaultTTL.Seconds()),
	})
}

// handleAdminPutBotVars applies MERGE semantics per key, like the caps
// route does per field: a string sets the override, an explicit null
// removes the key, a key absent from the body keeps its stored state.
func (s *Server) handleAdminPutBotVars(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var patch map[string]*string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if len(patch) == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty patch: name at least one ITERION_* var (null to clear)")
		return
	}
	rec, err := s.botVarsStore.Get(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	var prevUpdatedAt time.Time
	if rec != nil {
		prevUpdatedAt = rec.UpdatedAt
	}
	if rec == nil {
		rec = &platformcfg.BotVars{}
	}
	// Work on a COPY of the map: the store may hand back a record that
	// shares it (MemoryStore does), and a patch that fails validation
	// below must leave the stored record untouched.
	vars := make(map[string]string, len(rec.Vars))
	for k, v := range rec.Vars {
		vars[k] = v
	}
	rec.Vars = vars
	// Audit meta carries old→new per touched key. Validate refuses
	// credential-SHAPED NAMES, but nothing can vouch for the VALUE an
	// operator chooses to store under an innocent name: values live in
	// clear in the settings doc, this audit trail and every GET — the
	// CLI help says so, and the surface is super-admin-only.
	changes := map[string]any{}
	for name, v := range patch {
		old := rec.Vars[name]
		if v == nil {
			if _, had := rec.Vars[name]; !had {
				// Clearing a key that was never set: refuse loudly rather
				// than audit a no-op — the operator probably misspelled it.
				s.httpErrorFor(w, r, http.StatusBadRequest, "%s is not set — nothing to clear", name)
				return
			}
			delete(rec.Vars, name)
			changes[name] = map[string]string{"old": old, "new": ""}
			continue
		}
		rec.Vars[name] = *v
		changes[name] = map[string]string{"old": old, "new": *v}
	}
	if err := rec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	rec.UpdatedBy = s.requestUserID(r)
	// Conditional write: two replicas merging concurrently must not
	// silently drop each other's keys through a blind ReplaceOne. A lost
	// race is a loud 409 — the operator re-runs the command against the
	// fresh state.
	if cas, ok := s.botVarsStore.(platformcfg.CASStore[platformcfg.BotVars]); ok {
		wrote, err := cas.PutIfUnchanged(r.Context(), *rec, prevUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if !wrote {
			s.httpErrorFor(w, r, http.StatusConflict, "bot vars changed concurrently — re-read and retry")
			return
		}
	} else if err := s.botVarsStore.Put(r.Context(), *rec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	s.botVars.Invalidate()
	s.auditPlatform(r, "", "platform.settings.bot_vars.updated", "platform_settings", platformcfg.FamilyBotVars, changes)
	s.handleAdminGetBotVars(w, r)
}
