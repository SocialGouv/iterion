package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

func (s *Server) registerForgeProvisioningRoutes() {
	s.mux.Handle("GET /api/teams/{id}/forge/repo-bots", s.requireAuth(http.HandlerFunc(s.handleListForgeRepoBots)))
	s.mux.Handle("POST /api/teams/{id}/forge/repo-bots", s.requireAuth(http.HandlerFunc(s.handleEnableForgeRepoBots)))
	s.mux.Handle("GET /api/teams/{id}/forge/repo-bots/preview", s.requireAuth(http.HandlerFunc(s.handlePreviewForgeEnable)))
	s.mux.Handle("PATCH /api/teams/{id}/forge/repo-bots/{integration_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateForgeRepoBots)))
	s.mux.Handle("DELETE /api/teams/{id}/forge/repo-bots/{integration_id}", s.requireAuth(http.HandlerFunc(s.handleDisableForgeRepoBots)))
}

func (s *Server) handleListForgeRepoBots(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.forgeIntegrations.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.RepoIntegration{}
	}
	writeJSON(w, struct {
		Integrations []forge.RepoIntegration `json:"integrations"`
	}{Integrations: list})
}

type forgeEnableReq struct {
	ConnectionID string   `json:"connection_id"`
	Repo         string   `json:"repo"`
	BotIDs       []string `json:"bot_ids"`
	// ScheduleCrons overrides a scheduled bot's cron (botID → 5-field cron)
	// from the enable dialog's cron picker; empty falls back to the manifest
	// suggested_cron.
	ScheduleCrons map[string]string `json:"schedule_crons,omitempty"`
	// LaunchVars are operator overrides stamped onto every run this repo's
	// bots launch, layered after their manifest vars and re-applied on every
	// re-provision. Nil leaves the stored ones untouched. The canonical use is
	// naming this repo's merge gate (`gate_context`) so several bots fill ONE
	// required check, each for the PRs it owns.
	LaunchVars map[string]string `json:"launch_vars,omitempty"`
	// Overlap is the repo's launch concurrency policy (pkg/schedgate
	// vocabulary: allow | skip | supersede). Empty leaves the stored one
	// untouched; on a review webhook `supersede` is the one worth setting.
	Overlap string `json:"overlap,omitempty"`

	// AutoFixOnGateFailure opts the repo into the zero-touch lane; a nil pointer
	// leaves its current choice alone (forge.RepoIntegration.AutoFixOnGateFailure).
	AutoFixOnGateFailure *bool `json:"auto_fix_on_gate_failure,omitempty"`

	// HoldLabels is the repo's automation pause; nil keeps the current set
	// (forge.RepoIntegration.HoldLabels).
	HoldLabels []string `json:"hold_labels,omitempty"`
}

func (s *Server) handleEnableForgeRepoBots(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req forgeEnableReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ConnectionID) == "" || strings.TrimSpace(req.Repo) == "" || len(req.BotIDs) == 0 {
		httpError(w, http.StatusBadRequest, "connection_id, repo and bot_ids are required")
		return
	}
	// Assert the connection belongs to this team before we provision.
	if _, ok := s.forgeConnForTenant(w, r, teamID, req.ConnectionID); !ok {
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:      teamID,
		ConnectionID:  req.ConnectionID,
		RepoFullName:  strings.TrimSpace(req.Repo),
		BotIDs:        req.BotIDs,
		ScheduleCrons: req.ScheduleCrons,
		LaunchVars:    req.LaunchVars,
		Overlap:       req.Overlap,
		AutoFix:       req.AutoFixOnGateFailure,
		HoldLabels:    req.HoldLabels,
		ActorID:       id.UserID,
	})
	if err != nil {
		s.writeForgeProvisionError(w, err)
		return
	}
	s.auditTenant(r, teamID, "forge.integration.provisioned", "forge_integration", res.IntegrationID, map[string]any{
		"repo": req.Repo, "bots": res.BotIDs, "connection_id": req.ConnectionID,
	})
	writeJSON(w, res)
}

// forgeUpdateReq sets an integration's EXACT bot set (replace semantics —
// the per-bot unbind). An empty list is a 400: removing the last bot is
// the DELETE (full deprovision), which also tears down webhook + hook.
type forgeUpdateReq struct {
	BotIDs []string `json:"bot_ids"`
	// ScheduleCrons follows forgeEnableReq semantics for bots (re)gaining a
	// schedule through this update.
	ScheduleCrons map[string]string `json:"schedule_crons,omitempty"`
	// LaunchVars are operator overrides stamped onto every run this repo's
	// bots launch, layered after their manifest vars and re-applied on every
	// re-provision. Nil leaves the stored ones untouched. The canonical use is
	// naming this repo's merge gate (`gate_context`) so several bots fill ONE
	// required check, each for the PRs it owns.
	LaunchVars map[string]string `json:"launch_vars,omitempty"`
	// Overlap is the repo's launch concurrency policy (pkg/schedgate
	// vocabulary: allow | skip | supersede). Empty leaves the stored one
	// untouched; on a review webhook `supersede` is the one worth setting.
	Overlap string `json:"overlap,omitempty"`

	// AutoFixOnGateFailure opts the repo into the zero-touch lane; a nil pointer
	// leaves its current choice alone (forge.RepoIntegration.AutoFixOnGateFailure).
	AutoFixOnGateFailure *bool `json:"auto_fix_on_gate_failure,omitempty"`

	// HoldLabels is the repo's automation pause; nil keeps the current set
	// (forge.RepoIntegration.HoldLabels).
	HoldLabels []string `json:"hold_labels,omitempty"`
}

func (s *Server) handleUpdateForgeRepoBots(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	integrationID := r.PathValue("integration_id")
	ri, err := s.forgeIntegrations.Get(r.Context(), integrationID)
	if err != nil || ri.TenantID != teamID {
		httpError(w, http.StatusNotFound, "integration not found")
		return
	}
	var req forgeUpdateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.BotIDs) == 0 {
		httpError(w, http.StatusBadRequest, "bot_ids must be non-empty — use DELETE to remove the integration entirely")
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:      teamID,
		ConnectionID:  ri.ConnectionID,
		RepoFullName:  ri.RepoFullName,
		BotIDs:        req.BotIDs,
		ScheduleCrons: req.ScheduleCrons,
		LaunchVars:    req.LaunchVars,
		Overlap:       req.Overlap,
		AutoFix:       req.AutoFixOnGateFailure,
		HoldLabels:    req.HoldLabels,
		ActorID:       id.UserID,
		Replace:       true,
	})
	if err != nil {
		s.writeForgeProvisionError(w, err)
		return
	}
	s.auditTenant(r, teamID, "forge.integration.bots_updated", "forge_integration", res.IntegrationID, map[string]any{
		"repo": ri.RepoFullName, "bots": res.BotIDs,
	})
	writeJSON(w, res)
}

func (s *Server) handleDisableForgeRepoBots(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	integrationID := r.PathValue("integration_id")
	ctx := store.WithTenant(r.Context(), teamID)
	if err := s.forgeOrchestrator.Deprovision(ctx, teamID, integrationID); err != nil {
		if errors.Is(err, forge.ErrIntegrationNotFound) {
			httpError(w, http.StatusNotFound, "integration not found")
			return
		}
		s.writeForgeProvisionError(w, err)
		return
	}
	s.auditTenant(r, teamID, "forge.integration.deprovisioned", "forge_integration", integrationID, nil)
	w.WriteHeader(http.StatusNoContent)
}

type forgeEnablePreview struct {
	EventsNormalized  []string              `json:"events_normalized"`
	ForgeNativeEvents []string              `json:"forge_native_events"`
	Scopes            map[string]string     `json:"scopes"`
	Secrets           []forgePreviewBind    `json:"secrets"`
	Commands          []forgePreviewCommand `json:"commands,omitempty"`
	Identity          forgePreviewIdent     `json:"identity"`
	Conflicts         []string              `json:"conflicts"`
}

type forgePreviewBind struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// forgePreviewCommand is one /slash-command the enabled bots add to the
// webhook, shown in the enable dialog so the operator knows what to type.
type forgePreviewCommand struct {
	Command string `json:"command"`
	BotID   string `json:"bot_id"`
}

type forgePreviewIdent struct {
	Handle   string `json:"handle"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
}

// handlePreviewForgeEnable computes exactly what enabling a set of bots on a
// repo will subscribe to + request — read-only, no forge writes — so the
// studio can show "Revi will subscribe to … and post as …" before the
// operator commits.
func (s *Server) handlePreviewForgeEnable(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.URL.Query().Get("connection_id"))
	if !ok {
		return
	}
	botIDs := splitCSV(r.URL.Query().Get("bots"))
	if len(botIDs) == 0 {
		httpError(w, http.StatusBadRequest, "bots query param required")
		return
	}

	// Mirror Provision exactly: a forge: block is optional, a command-only bot
	// subscribes to the comment event. Without this, command-only bots would
	// be (wrongly) flagged as conflicts and the Enable button disabled.
	pv := forge.PreviewEnable(s.forgeOrchestrator.Bots, s.forgeOrchestrator.Invocations, botIDs)
	binds := make([]forgePreviewBind, 0, len(pv.Binds))
	for _, b := range botIDs {
		if secret, ok := pv.Binds[b]; ok {
			binds = append(binds, forgePreviewBind{BotID: b, Secret: secret})
		}
	}
	commands := make([]forgePreviewCommand, 0, len(pv.Commands))
	for cmd, bot := range pv.Commands {
		commands = append(commands, forgePreviewCommand{Command: cmd, BotID: bot})
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Command < commands[j].Command })
	writeJSON(w, forgeEnablePreview{
		EventsNormalized:  pv.Events,
		ForgeNativeEvents: forge.ToNativeEvents(conn.Provider, pv.Events),
		Scopes:            pv.Scopes,
		Secrets:           binds,
		Commands:          commands,
		Identity:          forgePreviewIdent{Handle: conn.AccountLogin, Provider: string(conn.Provider), BaseURL: conn.BaseURL()},
		Conflicts:         pv.Conflicts,
	})
}

// writeForgeProvisionError maps orchestrator/admin failures to stable HTTP
// shapes the studio can act on.
func (s *Server) writeForgeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forge.ErrForbidden):
		writeJSONStatus(w, http.StatusForbidden, map[string]any{
			"error":  "insufficient_scope",
			"detail": "the connection's token cannot manage webhooks on this repo — reconnect with broader scope or paste a PAT with hook-admin rights",
		})
	case errors.Is(err, forge.ErrUnauthorized):
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":  "connection_unauthorized",
			"detail": "the connection credential was rejected — reconnect",
		})
	case errors.Is(err, forge.ErrConnectionNotFound):
		httpError(w, http.StatusNotFound, "connection not found")
	default:
		httpError(w, http.StatusBadGateway, "provisioning failed: %v", err)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
