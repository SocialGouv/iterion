package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
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

	// LabelAllowlist narrows which freshly-applied issue label dispatches the
	// implementer (e.g. ["implement"]); nil keeps the current set
	// (forge.RepoIntegration.LabelAllowlist).
	LabelAllowlist []string `json:"label_allowlist,omitempty"`
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
	conn, ok := s.forgeConnForTenant(w, r, teamID, req.ConnectionID)
	if !ok {
		return
	}
	// A watch-only connection cannot carry a webhook or a bot's forge token.
	// The orchestrator refuses it as well; naming it here keeps the operator's
	// mistake attached to the choice they just made.
	if conn.IsSecurityReadOnly() {
		httpError(w, http.StatusUnprocessableEntity,
			"connection %s is watch-only (Dependabot alerts only) — it cannot host webhooks or hand a bot a forge token; pick the team's runtime connection", conn.ID)
		return
	}
	// Org ex-ante validation (Org.RequireProvisionApproval): a team admin's
	// request is parked for an org admin's decision — nothing is created
	// forge-side until approved. Validated exactly like the direct path
	// above, so what the admin approves is what would have run. A store
	// error here FAILS the request (503): the gate never fails open.
	orgID, gerr := s.provisionOrgRequiringApproval(r.Context(), id, teamID)
	if gerr != nil {
		httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", gerr)
		return
	}
	if orgID != "" {
		req.Repo = strings.TrimSpace(req.Repo)
		// An enable on an ALREADY-connected repo is the ADD-BOTS gesture:
		// Provision merges (existing ∪ requested) whenever an integration
		// exists and Replace is false, which is exactly how the studio's
		// BindBotWizard calls this endpoint. Snapshot that live integration
		// so approve compares against what existed at park time via the
		// IntegrationID branch — recording it as a NEW-repo request instead
		// makes approve refuse every such request forever, with the false
		// reason "provisioned after the request was parked".
		var base forge.RepoIntegration
		if s.forgeIntegrations != nil {
			ri, gerr := s.forgeIntegrations.GetByConnRepo(store.WithTenant(r.Context(), teamID), teamID, req.ConnectionID, req.Repo)
			switch {
			case gerr == nil:
				base = ri
			case errors.Is(gerr, forge.ErrIntegrationNotFound):
				// Genuinely a new repo — the new-repo branch is correct.
			default:
				// Fail CLOSED, like the gate itself: parking on an unreadable
				// store would record a new-repo request for what may be a
				// live integration, and approve would then REPLACE it.
				httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: resolve the repo's current integration: %v", gerr)
				return
			}
		}
		s.parkProvisionRequest(w, r, id, orgID, teamID, req, base.ID, false, base)
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:       teamID,
		ConnectionID:   req.ConnectionID,
		RepoFullName:   strings.TrimSpace(req.Repo),
		BotIDs:         req.BotIDs,
		ScheduleCrons:  req.ScheduleCrons,
		LaunchVars:     req.LaunchVars,
		Overlap:        req.Overlap,
		AutoFix:        req.AutoFixOnGateFailure,
		HoldLabels:     req.HoldLabels,
		LabelAllowlist: req.LabelAllowlist,
		ActorID:        id.UserID,
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

	// LabelAllowlist narrows which freshly-applied issue label dispatches the
	// implementer (e.g. ["implement"]); nil keeps the current set
	// (forge.RepoIntegration.LabelAllowlist).
	LabelAllowlist []string `json:"label_allowlist,omitempty"`
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
	// Org ex-ante validation: only an update EXPANDING the automated
	// surface needs the org's approval — removals and tightenings go
	// through directly (see expandsProvisionSurface). A store error FAILS
	// the request (503): the gate never fails open.
	uOrgID, ugerr := s.provisionOrgRequiringApproval(r.Context(), id, teamID)
	if ugerr != nil {
		httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", ugerr)
		return
	}
	if orgID := uOrgID; orgID != "" && expandsProvisionSurface(ri, req) {
		s.parkProvisionRequest(w, r, id, orgID, teamID, forgeEnableReq{
			ConnectionID:         ri.ConnectionID,
			Repo:                 ri.RepoFullName,
			BotIDs:               req.BotIDs,
			ScheduleCrons:        req.ScheduleCrons,
			LaunchVars:           req.LaunchVars,
			Overlap:              req.Overlap,
			AutoFixOnGateFailure: req.AutoFixOnGateFailure,
			HoldLabels:           req.HoldLabels,
			LabelAllowlist:       req.LabelAllowlist,
		}, ri.ID, true, ri)
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:       teamID,
		ConnectionID:   ri.ConnectionID,
		RepoFullName:   ri.RepoFullName,
		BotIDs:         req.BotIDs,
		ScheduleCrons:  req.ScheduleCrons,
		LaunchVars:     req.LaunchVars,
		Overlap:        req.Overlap,
		AutoFix:        req.AutoFixOnGateFailure,
		HoldLabels:     req.HoldLabels,
		LabelAllowlist: req.LabelAllowlist,
		ActorID:        id.UserID,
		Replace:        true,
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

// addsBots reports whether `requested` contains a bot id absent from
// `current` — part of the surface-expansion predicate of the org
// approval gate.
func addsBots(current, requested []string) bool {
	have := make(map[string]bool, len(current))
	for _, b := range current {
		have[b] = true
	}
	for _, b := range requested {
		if !have[b] {
			return true
		}
	}
	return false
}

// expandsProvisionSurface is the org-approval predicate for UPDATES of an
// existing integration: true when the request grows what automation may do
// on the repo without a human. Keying on the bot set alone would let a
// team admin bypass the gate through the switches replayed by the same
// endpoint — turning the zero-touch fixer lane on, lifting a hold label,
// or widening the issue-label dispatch — so those expansions park too.
// Tightenings (removing bots, turning auto-fix off, adding hold labels,
// narrowing the allowlist) and LaunchVars edits go through directly.
func expandsProvisionSurface(ri forge.RepoIntegration, req forgeUpdateReq) bool {
	if addsBots(ri.BotIDs, req.BotIDs) {
		return true
	}
	// Zero-touch lane turned ON (nil leaves the stored choice alone).
	if req.AutoFixOnGateFailure != nil && *req.AutoFixOnGateFailure && !ri.AutoFixOnGateFailure {
		return true
	}
	// HoldLabels is the operator's automation brake: removing any current
	// entry re-arms lanes the label was pausing. nil keeps the stored set.
	if req.HoldLabels != nil && removesAny(ri.HoldLabels, req.HoldLabels) {
		return true
	}
	// An EMPTY allowlist means "any freshly-applied label dispatches", so
	// clearing a non-empty one — or adding a label to it — widens dispatch.
	if req.LabelAllowlist != nil && len(ri.LabelAllowlist) > 0 &&
		(len(req.LabelAllowlist) == 0 || addsBots(ri.LabelAllowlist, req.LabelAllowlist)) {
		return true
	}
	// Any schedule change arms (or re-arms) unattended recurring launches.
	// The stored cadence lives in the scheduler, not on the integration, so
	// a tightening cannot be told apart here — park every non-empty set
	// (fail toward approval, never past it).
	if len(req.ScheduleCrons) > 0 {
		return true
	}
	// Overlap: "allow" is the only unbounded concurrency policy (skip and
	// supersede both cap the repo at one live run); moving to it from a
	// bounded stored policy widens. An empty stored value already means
	// allow (the historical default), so that transition changes nothing.
	if req.Overlap == "allow" && ri.Overlap != "" && ri.Overlap != "allow" {
		return true
	}
	return false
}

// openAllowlist reports whether a gate-style allowlist admits EVERYTHING:
// pkg/webhooks treats an empty list as allow-all, and a bare "*" entry is
// the explicit spelling of the same thing (MatchProject, MatchLabel,
// MatchAuthor). Normalising the two together is what lets the widening
// test below call ["*"] → ["group/api"] a narrowing instead of "an entry
// this list did not have".
func openAllowlist(list []string) bool {
	if len(list) == 0 {
		return true
	}
	for _, e := range list {
		if strings.TrimSpace(e) == "*" {
			return true
		}
	}
	return false
}

// widensAllowlist reports whether replacing `before` with `after` lets MORE
// through, for the gate-style allowlists whose empty value means allow-all.
// Opening a bounded list (cleared, or given a "*") widens; so does any entry
// the old list did not carry — including a "*foo" suffix wildcard added
// beside the exact "foo" it generalises. Comparison is trimmed but
// case-SENSITIVE: the matchers fold case, so a case-only edit is a no-op
// that this reads as a new entry, erring toward approval.
//
// NOT valid for EventAllowlist, whose empty value means "the provider's
// defaults", not allow-all — see the caller.
func widensAllowlist(before, after []string) bool {
	if openAllowlist(before) {
		return false // already open — nothing can widen it
	}
	if openAllowlist(after) {
		return true // bounded → allow-all
	}
	have := make(map[string]bool, len(before))
	for _, e := range before {
		have[strings.TrimSpace(e)] = true
	}
	for _, e := range after {
		if !have[strings.TrimSpace(e)] {
			return true
		}
	}
	return false
}

// expandsWebhookSurface is expandsProvisionSurface's twin for a DIRECT
// PATCH of a provisioned webhook config. The provisioning gate would be
// theatre without it: the webhook config is where these settings are
// actually ENFORCED at delivery time, and PATCH /api/teams/{id}/webhooks/
// {webhook_id} accepts every one of them behind canManageTeam alone. A team
// admin could get one modest bot approved and then widen the surface at
// will on the config the approval produced.
//
// Scope is deliberately the AUTHORIZATION/DISPATCH surface — who and what
// may launch without a human — mirroring what the provisioning request
// carries. Budget dials (RateLimit, MonthlyCallLimit) and credential
// bindings (KeyOverrides, SecretOverrides) are governed elsewhere and are
// not classified here.
func expandsWebhookSurface(before, after webhooks.Config) bool {
	// Re-enabling a disabled webhook restores the ENTIRE surface at once
	// (middleware_webhook.go answers 410 while off).
	if !before.Enabled && after.Enabled {
		return true
	}
	// Bot scope: normalizeBotScope has already turned wildcard into ["*"].
	if after.WildcardBots && !before.WildcardBots {
		return true
	}
	if addsBots(before.BotIDs, after.BotIDs) {
		return true
	}
	// Gate-style allowlists: empty (or "*") means allow-all, so clearing or
	// extending one widens which deliveries dispatch.
	if widensAllowlist(before.ProjectAllowlist, after.ProjectAllowlist) ||
		widensAllowlist(before.AuthorAllowlist, after.AuthorAllowlist) ||
		widensAllowlist(before.LabelAllowlist, after.LabelAllowlist) {
		return true
	}
	// EventAllowlist is NOT a plain allowlist: empty falls back to the
	// provider's default set (MatchEvent), so a clear can either widen or
	// narrow depending on what the list held. Undecidable here — park every
	// change, the same "fail toward approval, never past it" rule the
	// schedule arm of expandsProvisionSurface applies.
	if !equalStringSets(before.EventAllowlist, after.EventAllowlist) {
		return true
	}
	// AuthorizedRepliers is an inverted list: it GRANTS the command right
	// (empty = nobody bypasses the role check), so additions widen.
	if addsBots(before.AuthorizedRepliers, after.AuthorizedRepliers) {
		return true
	}
	// Lowering the role floor widens who may launch a /command. "" reads as
	// developer on both sides (webhooks.ReplierRoleRank), so an unset field
	// compares as the default rather than as zero.
	if webhooks.ReplierRoleRank(after.MinReplierRole) < webhooks.ReplierRoleRank(before.MinReplierRole) {
		return true
	}
	// The operator's automation brake: removing any hold label re-arms the
	// lanes it was pausing.
	if removesAny(before.HoldLabels, after.HoldLabels) {
		return true
	}
	// Zero-touch lanes.
	if after.AutoImplementOnOpen && !before.AutoImplementOnOpen {
		return true
	}
	if after.ReviewOnSync && !before.ReviewOnSync {
		return true
	}
	// Turning a declared protection off. (The GitHub auto path blocks fork
	// PRs unconditionally today, so this is belt-and-braces there — but the
	// switch is documented as a protection, and the predicate should be
	// right the day the guard becomes conditional.)
	if before.BlockForkPRs && !after.BlockForkPRs {
		return true
	}
	// Overlap: "allow" is the only unbounded concurrency policy; the empty
	// stored value already behaves as allow on this path (overlapSupersedes
	// short-circuits on ""), so that transition changes nothing.
	if after.Overlap == "allow" && before.Overlap != "" && before.Overlap != "allow" {
		return true
	}
	return false
}

// removesAny reports whether any entry of `current` is absent from `next`.
func removesAny(current, next []string) bool {
	keep := make(map[string]bool, len(next))
	for _, s := range next {
		keep[s] = true
	}
	for _, s := range current {
		if !keep[s] {
			return true
		}
	}
	return false
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
