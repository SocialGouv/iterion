package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/configshare"
)

const defaultShareTTLDays = 14

// registerConfigShareAdminRoutes wires the operator (JWT) CRUD for config-share
// grants — team-scoped, canManageTeam-gated, audited. A real user identity
// (Kind == user) is required; the RBAC gates reject a synthetic principal.
func (s *Server) registerConfigShareAdminRoutes() {
	s.mux.Handle("GET /api/teams/{id}/config-shares", s.requireAuth(http.HandlerFunc(s.handleListConfigShares)))
	s.mux.Handle("POST /api/teams/{id}/config-shares", s.requireAuth(http.HandlerFunc(s.handleCreateConfigShare)))
	s.mux.Handle("POST /api/teams/{id}/config-shares/{sid}/rotate", s.requireAuth(http.HandlerFunc(s.handleRotateConfigShare)))
	s.mux.Handle("DELETE /api/teams/{id}/config-shares/{sid}", s.requireAuth(http.HandlerFunc(s.handleDeleteConfigShare)))
	s.mux.Handle("GET /api/teams/{id}/config-shares/{sid}/deliveries", s.requireAuth(http.HandlerFunc(s.handleConfigShareDeliveries)))
}

type createConfigShareReq struct {
	BotID        string   `json:"bot_id"`
	Label        string   `json:"label"`
	RepoURL      string   `json:"repo_url"`
	RepoRef      string   `json:"repo_ref"`
	ConfigPath   string   `json:"config_path"`
	Category     string   `json:"category"`
	SchemaRef    string   `json:"schema_ref"`
	AllowedPaths []string `json:"allowed_paths"`
	VisiblePaths []string `json:"visible_paths"`
	// EditableFields optionally narrows a derived (config_share-block) grant to
	// a SUBSET of the bot's declared editable fields (by leaf name, e.g.
	// ["feeds"]) — least privilege per share. Empty = the full declared
	// surface. Ignored for a bot with no declared block.
	EditableFields []string `json:"editable_fields"`
	ReadOnly       bool     `json:"read_only"`
	ExpiresDays    int      `json:"expires_days"`
	// NeverExpires opts a share out of the default TTL entirely (durable access
	// for standing, semi-trusted editors). Revocation is then only via rotate /
	// delete — there is no time-bounded safety net, so it is opt-in.
	NeverExpires bool `json:"never_expires"`
}

// shareView is the operator-facing projection — never the token hash.
func (s *Server) shareView(sh *configshare.Share) map[string]any {
	v := map[string]any{
		"id": sh.ID, "bot_id": sh.BotID, "label": sh.Label, "repo_url": sh.RepoURL,
		"repo_ref": sh.RepoRef, "config_path": sh.ConfigPath, "category": sh.Category,
		"schema_ref": sh.SchemaRef, "allowed_paths": sh.AllowedPaths, "visible_paths": sh.VisiblePaths,
		"read_only": sh.ReadOnly, "enabled": sh.Enabled, "token_last4": sh.TokenLast4,
		"fingerprint": sh.Fingerprint, "created_at": sh.CreatedAt,
		"revoked_at": sh.RevokedAt, "last_used_at": sh.LastUsedAt,
	}
	// Omit expires_at for a never-expiring share (zero time) so the UI renders
	// "never" instead of the Go zero year.
	if !sh.ExpiresAt.IsZero() {
		v["expires_at"] = sh.ExpiresAt
	}
	return v
}

func (s *Server) shareURL(id, token string) string {
	if s.authSvc == nil {
		return ""
	}
	base := s.authSvc.PublicURL()
	if base == "" {
		return ""
	}
	return base + "/config/" + id + "#" + token
}

// botConfigShareSpec resolves a bot_id to its declared config-share surface
// (manifest config_share: block), or nil when the bot declares none or is not
// resolvable on this server (a loose .bot, or a bot absent from the effective
// paths). Best-effort by design: the operator is trusted (canManageTeam), so a
// bot without a discoverable surface mints with explicit operator-supplied
// paths — the block is a guard-rail + convenience for the common case, not the
// trust boundary against the operator.
// botManifest loads a bot's manifest.yaml (persona display_name, config_share
// surface, …) resolving the bot id against the effective bot paths. Returns nil
// when the bot isn't resolvable on this server (e.g. a loose .bot).
func (s *Server) botManifest(botID string) *bundle.Manifest {
	mainFile, err := botregistry.ResolveBotPath(botID, s.effectivePaths())
	if err != nil {
		return nil
	}
	m, err := bundle.LoadManifest(filepath.Join(filepath.Dir(mainFile), "manifest.yaml"))
	if err != nil {
		return nil
	}
	return m
}

func (s *Server) botConfigShareSpec(botID string) *bundle.ConfigShareSpec {
	if m := s.botManifest(botID); m != nil {
		return m.ConfigShare
	}
	return nil
}

func (s *Server) handleCreateConfigShare(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req createConfigShareReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.BotID == "" {
		httpError(w, http.StatusBadRequest, "bot_id required")
		return
	}
	if _, err := configshare.RepoSlug(req.RepoURL); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := configshare.ValidateRepoRef(req.RepoRef); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// When the bot DECLARES a config-share surface (manifest config_share:),
	// it is authoritative: the mint DERIVES the config file + editable/visible
	// paths from it (expanding {category}), so a share can never be minted
	// outside what the bot committed to git. The operator supplies only
	// bot_id + category. A bot with no declared surface (or one not resolvable
	// on this server — e.g. a loose .bot) falls back to explicit
	// operator-supplied paths, unchanged.
	configPath := req.ConfigPath
	allowed := req.AllowedPaths
	visible := req.VisiblePaths
	derivedFromSpec := false
	if spec := s.botConfigShareSpec(req.BotID); spec != nil {
		a, v, err := configshare.DeriveGrant(spec.EditablePaths, spec.VisiblePaths, req.Category, req.EditableFields...)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		allowed, visible = a, v
		if spec.ConfigPath != "" {
			configPath = spec.ConfigPath
		}
		derivedFromSpec = true
	} else if len(visible) == 0 {
		visible = allowed
	}
	if err := configshare.ValidateConfigPath(configPath); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := configshare.ValidatePaths(allowed, visible); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// Guard the common mistake: minting a category share for a category that
	// doesn't exist in the config file yet (e.g. "design" when the real ones
	// are "design-systems"/"design-sp"). A best-effort projected read — if the
	// forge is reachable AND the projection is empty, the category (or its
	// editable fields) isn't there, so the editor would land on an empty form.
	// Reject with a clear error. A forge/transport failure does NOT block the
	// mint (the operator is trusted; this is a guard-rail, not a gate).
	if derivedFromSpec && req.Category != "" {
		probe := &configshare.Share{
			TenantID: teamID, BotID: req.BotID, RepoURL: req.RepoURL, RepoRef: req.RepoRef,
			ConfigPath: configPath, Category: req.Category, AllowedPaths: allowed, VisiblePaths: visible,
		}
		if fc, ferr := s.resolveShareFC(r.Context(), probe); ferr == nil {
			if proj, _, perr := s.configShareSvc.ProjectedRead(r.Context(), fc, probe); perr == nil && len(proj) == 0 {
				httpError(w, http.StatusBadRequest, "category %q has no editable fields in %s — create it in the config file first", req.Category, configPath)
				return
			}
		}
	}
	plaintext, hash, last4, fp, err := configshare.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint failed")
		return
	}
	now := time.Now().UTC()
	// A zero ExpiresAt never expires (Share.Active). never_expires opts into
	// that; otherwise 0/omitted days falls back to the default TTL.
	var expiresAt time.Time
	if !req.NeverExpires {
		ttl := req.ExpiresDays
		if ttl <= 0 {
			ttl = defaultShareTTLDays
		}
		expiresAt = now.AddDate(0, 0, ttl)
	}
	sh := &configshare.Share{
		ID: uuid.NewString(), TenantID: teamID, BotID: req.BotID, Label: req.Label,
		RepoURL: req.RepoURL, RepoRef: req.RepoRef, ConfigPath: configPath,
		Category: req.Category, SchemaRef: req.SchemaRef,
		AllowedPaths: allowed, VisiblePaths: visible, ReadOnly: req.ReadOnly,
		TokenHash: hash, TokenLast4: last4, Fingerprint: fp,
		Enabled: true, CreatedBy: id.UserID, CreatedAt: now, ExpiresAt: expiresAt,
	}
	if err := s.configShares.Create(r.Context(), sh); err != nil {
		httpError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.auditTenant(r, teamID, "config_share.created", "config_share", sh.ID, map[string]any{"bot_id": sh.BotID, "repo": sh.RepoURL})
	view := s.shareView(sh)
	view["token"] = plaintext // shown ONCE
	view["url"] = s.shareURL(sh.ID, plaintext)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, view)
}

func (s *Server) handleListConfigShares(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	rows, err := s.configShares.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]map[string]any, 0, len(rows))
	for _, sh := range rows {
		views = append(views, s.shareView(sh))
	}
	writeJSON(w, map[string]any{"shares": views})
}

func (s *Server) loadTeamShare(w http.ResponseWriter, r *http.Request, manage bool) (*configshare.Share, bool) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	authorized := s.canViewTeam(r.Context(), id, teamID)
	if manage {
		authorized = s.canManageTeam(r.Context(), id, teamID)
	}
	if !authorized {
		httpError(w, http.StatusForbidden, "forbidden")
		return nil, false
	}
	sh, err := s.configShares.GetByID(r.Context(), r.PathValue("sid"))
	if err != nil || sh == nil || sh.TenantID != teamID {
		httpError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return sh, true
}

func (s *Server) handleRotateConfigShare(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadTeamShare(w, r, true)
	if !ok {
		return
	}
	plaintext, hash, last4, fp, err := configshare.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint failed")
		return
	}
	sh.TokenHash, sh.TokenLast4, sh.Fingerprint = hash, last4, fp
	if err := s.configShares.Update(r.Context(), sh); err != nil {
		httpError(w, http.StatusInternalServerError, "rotate failed")
		return
	}
	s.auditTenant(r, sh.TenantID, "config_share.rotated", "config_share", sh.ID, nil)
	view := s.shareView(sh)
	view["token"] = plaintext
	view["url"] = s.shareURL(sh.ID, plaintext)
	writeJSON(w, view)
}

func (s *Server) handleDeleteConfigShare(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadTeamShare(w, r, true)
	if !ok {
		return
	}
	if err := s.configShares.Delete(r.Context(), sh.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.auditTenant(r, sh.TenantID, "config_share.deleted", "config_share", sh.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConfigShareDeliveries(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadTeamShare(w, r, false)
	if !ok {
		return
	}
	rows, err := s.configShares.ListDeliveries(r.Context(), sh.ID, 200)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rows == nil {
		rows = []*configshare.Delivery{}
	}
	writeJSON(w, map[string]any{"deliveries": rows})
}
