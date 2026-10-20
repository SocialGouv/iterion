package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/runview"
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
	// required check, each for the PRs it owns. A non-nil map is the exact
	// desired set: an empty one clears every pin, and a non-empty one that
	// would drop keys the repo pins today is refused with the keys named —
	// unless launch_vars_replace makes the replacement explicit.
	LaunchVars map[string]string `json:"launch_vars,omitempty"`
	// LaunchVarsReplace acknowledges that launch_vars replaces the whole map,
	// dropping pins the caller did not echo. The escape hatch for the drop
	// refusal — without it a partial write never applies silently.
	LaunchVarsReplace bool `json:"launch_vars_replace,omitempty"`
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

	// Routing is the repo's routing policy (pkg/llmroute, ADR-129) — the
	// binding level for every launch this integration makes. ABSENT leaves
	// the stored block untouched (an enable that adds one bot must not drop
	// the repo's pin), `null` clears it, an object replaces it wholesale —
	// decoded strictly through the ONE routing decoder; locks are refused
	// (the binding level holds no lock primitive) and the block is
	// validated before anything is written.
	Routing json.RawMessage `json:"routing,omitempty"`
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
	// The routing block decodes at the edge, through the ONE decoder: a
	// typo must refuse here, before anything is provisioned or parked, so
	// what the operator (or the org, at approve time) sees is a validated
	// block. Absent keeps the stored one; null clears; an object replaces.
	routing, routingSpoke, rerr := decodeRoutingBlock(req.Routing, routingLocksRefused)
	if rerr != nil {
		httpError(w, rerr.status, "%s", rerr.err.Error())
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
		// This endpoint is ALSO the add-bots path for an already-connected
		// repo (Provision merges when the integration exists). Record which
		// case this request is — id + bot set snapshot — or the approve-time
		// staleness check would mistake an add-bots request for a new-repo
		// one and refuse it forever. A store error here fails closed like
		// the gate itself; only a clean not-found means "new repo".
		integrationID, baseBots := "", []string(nil)
		baseHolds, baseAllow := []string(nil), []string(nil)
		if s.forgeIntegrations != nil {
			ri, ierr := s.forgeIntegrations.GetByConnRepo(store.WithTenant(r.Context(), teamID), teamID, req.ConnectionID, req.Repo)
			switch {
			case ierr == nil:
				integrationID, baseBots = ri.ID, ri.BotIDs
				baseHolds, baseAllow = ri.HoldLabels, ri.LabelAllowlist
			case errors.Is(ierr, forge.ErrIntegrationNotFound):
				// genuinely new repo
			default:
				httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", ierr)
				return
			}
		}
		s.parkProvisionRequest(w, r, id, orgID, teamID, req, integrationID, false, baseBots, baseHolds, baseAllow, routing, routingSpoke)
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:          teamID,
		ConnectionID:      req.ConnectionID,
		RepoFullName:      strings.TrimSpace(req.Repo),
		BotIDs:            req.BotIDs,
		ScheduleCrons:     req.ScheduleCrons,
		LaunchVars:        req.LaunchVars,
		LaunchVarsReplace: req.LaunchVarsReplace,
		Overlap:           req.Overlap,
		AutoFix:           req.AutoFixOnGateFailure,
		HoldLabels:        req.HoldLabels,
		LabelAllowlist:    req.LabelAllowlist,
		Routing:           routing,
		RoutingSet:        routingSpoke,
		ActorID:           id.UserID,
	})
	if err != nil {
		s.writeForgeProvisionError(w, err)
		return
	}
	s.writeProvisionResult(w, r, teamID, "forge.integration.provisioned", res, s.routingBlastRadiusWarnings(ctx, teamID, res.BotIDs, routing),
		map[string]any{"repo": req.Repo, "bots": res.BotIDs, "connection_id": req.ConnectionID})
}

// forgeUpdateReq sets an integration's EXACT bot set when bot_ids is present
// (replace semantics — the per-bot unbind). An ABSENT bot_ids keeps the
// stored set, so a caller changing only the launch vars (the gate switch)
// need not re-state the bots; an EMPTY list is a 400: removing the last bot
// is the DELETE (full deprovision), which also tears down webhook + hook.
type forgeUpdateReq struct {
	BotIDs []string `json:"bot_ids"`
	// ScheduleCrons follows forgeEnableReq semantics for bots (re)gaining a
	// schedule through this update.
	ScheduleCrons map[string]string `json:"schedule_crons,omitempty"`
	// LaunchVars are operator overrides stamped onto every run this repo's
	// bots launch, layered after their manifest vars and re-applied on every
	// re-provision. Nil leaves the stored ones untouched. The canonical use is
	// naming this repo's merge gate (`gate_context`) so several bots fill ONE
	// required check, each for the PRs it owns. A non-nil map is the exact
	// desired set: an empty one clears every pin, and a non-empty one that
	// would drop keys the repo pins today is refused with the keys named —
	// unless launch_vars_replace makes the replacement explicit.
	LaunchVars map[string]string `json:"launch_vars,omitempty"`
	// LaunchVarsReplace acknowledges that launch_vars replaces the whole map,
	// dropping pins the caller did not echo. The escape hatch for the drop
	// refusal — without it a partial write never applies silently.
	LaunchVarsReplace bool `json:"launch_vars_replace,omitempty"`
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

	// Routing is the repo's routing policy (pkg/llmroute, ADR-129); the
	// same three-way rule as the enable request (absent keeps, null
	// clears, an object replaces), through the ONE routing decoder with
	// locks refused at the binding level.
	Routing json.RawMessage `json:"routing,omitempty"`
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
	if req.BotIDs == nil {
		// Absent means "leave the bot set alone" — a launch-vars-only patch
		// (the gate switch) must not have to echo it. An explicit empty list
		// stays the 400 it always was.
		req.BotIDs = ri.BotIDs
	} else if len(req.BotIDs) == 0 {
		httpError(w, http.StatusBadRequest, "bot_ids must be non-empty — use DELETE to remove the integration entirely")
		return
	}
	// The routing block, decoded at the edge like the enable path — the ONE
	// decoder, the binding surface's lock posture.
	routing, routingSpoke, rerr := decodeRoutingBlock(req.Routing, routingLocksRefused)
	if rerr != nil {
		httpError(w, rerr.status, "%s", rerr.err.Error())
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
			LaunchVarsReplace:    req.LaunchVarsReplace,
			Overlap:              req.Overlap,
			AutoFixOnGateFailure: req.AutoFixOnGateFailure,
			HoldLabels:           req.HoldLabels,
			LabelAllowlist:       req.LabelAllowlist,
			Routing:              req.Routing,
		}, ri.ID, true, ri.BotIDs, ri.HoldLabels, ri.LabelAllowlist, routing, routingSpoke)
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	res, err := s.forgeOrchestrator.Provision(ctx, forge.ProvisionRequest{
		TenantID:          teamID,
		ConnectionID:      ri.ConnectionID,
		RepoFullName:      ri.RepoFullName,
		BotIDs:            req.BotIDs,
		ScheduleCrons:     req.ScheduleCrons,
		LaunchVars:        req.LaunchVars,
		LaunchVarsReplace: req.LaunchVarsReplace,
		Overlap:           req.Overlap,
		AutoFix:           req.AutoFixOnGateFailure,
		HoldLabels:        req.HoldLabels,
		LabelAllowlist:    req.LabelAllowlist,
		Routing:           routing,
		RoutingSet:        routingSpoke,
		ActorID:           id.UserID,
		Replace:           true,
	})
	if err != nil {
		s.writeForgeProvisionError(w, err)
		return
	}
	s.writeProvisionResult(w, r, teamID, "forge.integration.bots_updated", res,
		s.routingBlastRadiusWarnings(ctx, teamID, res.BotIDs, routing),
		map[string]any{"repo": ri.RepoFullName, "bots": res.BotIDs})
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
	// GatesMerges is the merge-gate posture the enable installs: one of the
	// selected bots posts a commit status, so merges become gateable and
	// every push is re-reviewed to keep the status on the current head.
	GatesMerges bool `json:"gates_merges"`
	// HookBaseURL is the origin the webhook will be registered under — the
	// connection's pin, else the deployment's public URL. Shown because a
	// forge that only delivers to allowlisted hosts refuses the other one,
	// and the operator can read that mismatch before the write.
	HookBaseURL string `json:"hook_base_url"`
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
	// ProjectBot is true when the connection posts as a per-repository bot
	// iterion mints at enable: Handle is then the minted bot's login when the
	// repo already has one, and empty before the first enable — the studio
	// says "a dedicated project bot" rather than naming nobody.
	ProjectBot bool `json:"project_bot,omitempty"`
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
	pv := forge.PreviewEnable(r.Context(), teamID, s.forgeOrchestrator.Bots, s.forgeOrchestrator.Invocations, botIDs)
	binds := make([]forgePreviewBind, 0, len(pv.Binds))
	for _, b := range botIDs {
		if secret, ok := pv.Binds[b]; ok {
			binds = append(binds, forgePreviewBind{BotID: b, Secret: secret})
		}
	}
	commands := make([]forgePreviewCommand, 0, len(pv.Commands))
	for _, command := range pv.Commands {
		commands = append(commands, forgePreviewCommand{Command: command.Command, BotID: command.BotID})
	}
	// The PR's sort carries the BotID tiebreaker: a preview can list the same
	// command once per bot now that the response exposes Commands' BotID.
	sort.Slice(commands, func(i, j int) bool {
		if commands[i].Command == commands[j].Command {
			return commands[i].BotID < commands[j].BotID
		}
		return commands[i].Command < commands[j].Command
	})
	ident := forgePreviewIdent{Handle: conn.AccountLogin, Provider: string(conn.Provider), BaseURL: conn.BaseURL()}
	if conn.MintsProjectBots() {
		// Not the authorising person: the repo's bot, named once it exists.
		ident.ProjectBot, ident.Handle = true, ""
		if repo := strings.TrimSpace(r.URL.Query().Get("repo")); repo != "" && s.forgeIntegrations != nil {
			if ri, err := s.forgeIntegrations.GetByConnRepo(store.WithTenant(r.Context(), teamID), teamID, conn.ID, repo); err == nil && ri.ProjectBot != nil {
				ident.Handle = ri.ProjectBot.Login
			}
		}
	}
	writeJSON(w, forgeEnablePreview{
		EventsNormalized:  pv.Events,
		ForgeNativeEvents: forge.ToNativeEvents(conn.Provider, pv.Events),
		Scopes:            pv.Scopes,
		Secrets:           binds,
		Commands:          commands,
		Identity:          ident,
		Conflicts:         pv.Conflicts,
		GatesMerges:       pv.GatesMerges,
		HookBaseURL:       s.forgeOrchestrator.HookBaseURL(conn),
	})
}

// projectBotNotPermittedDetail is the operator's reading of a refused mint:
// the forge's own words first, then the two settings that produce them.
func projectBotNotPermittedDetail(err error) string {
	return err.Error() + " — the authorising account must hold the Maintainer role on the project, project access tokens must be allowed on it" +
		" (GitLab: the group's Settings → General → Permissions and group features → Project access tokens)," +
		" and the instance or group must allow a token lifetime of at least 7 days (iterion asks for 365, then shorter);" +
		" otherwise connect the repository with a project access token created by hand (the personal-token method)"
}

// hookPinGesture names where an operator pins a connection's webhook base:
// the studio row, and the CLI for whoever is not in the studio.
func hookPinGesture(connectionID string) string {
	if connectionID == "" {
		connectionID = "<conn-id>"
	}
	return "(Integrations → Repositories → the connection's \"Webhook delivery\" row, " +
		"or iterion remote forge connections webhook-base " + connectionID + " --url <base>)"
}

// hookRefusedRetry is the gesture after the setting. An org that requires
// provisioning approval answered the refusal to the APPROVER and kept the
// request parked, so "enable the repo again" would ask the team admin to
// park a duplicate — which the store refuses.
const hookRefusedRetry = "then retry the enable (on an org that requires approval, approve the pending request again)."

// absoluteHTTPURL reports an address a forge can dial: an http(s) scheme AND
// a host. "Parses" is not the question — a deployment public URL whose scheme
// was forgotten ("iterion.example.org") parses fine and reaches the forge
// verbatim, which is how it gets refused.
func absoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// hookRefusedDetail is the operator's remedy for a forge that refused the
// webhook address, quoting what the forge itself said: a 422 is also how a
// forge refuses a hook for a reason that has nothing to do with the address
// (a project hook limit, a rejected field), and the remedy differs. Four
// cases, four gestures — the connection pins a base the forge does not
// accept, the deployment has no public URL at all, its public URL is not an
// absolute http(s) URL, or nothing is pinned yet — and the generic
// "provisioning failed: HTTP 422" names none of them.
func hookRefusedDetail(e *forge.HookRefusedError) string {
	head := fmt.Sprintf("%s refused to register the webhook at %s (HTTP 422", e.Provider, e.URL)
	if e.Detail != "" {
		head += fmt.Sprintf(": %q", e.Detail)
	}
	head += "). "
	switch {
	case e.Pinned != "":
		return head + fmt.Sprintf("This connection pins %s: check that the forge accepts that host "+
			"(GitLab: Admin area → Settings → Network → Outbound requests), or change the pin.", e.Pinned)
	case strings.HasPrefix(e.URL, "/"):
		// A relative address: the deployment's public URL is unset, and no
		// forge accepts that. Naming the allowlist here would send the
		// operator to pin a per-connection escape hatch for a one-setting
		// deployment gap.
		return head + "This deployment has no public URL configured (auth.publicUrl / ITERION_PUBLIC_URL), " +
			"so the webhook address is relative: set it, or pin a base URL on this connection " +
			hookPinGesture(e.ConnectionID) + ", " + hookRefusedRetry
	case !absoluteHTTPURL(e.URL):
		// An address no forge can dial — a public URL whose scheme was
		// forgotten is the one this deployment ships with, since
		// config.Validate only checks PublicURL's scheme and host when
		// canonical redirects are on (off by default). The allowlist is the
		// wrong door, and so is "no public URL configured": there is one, and
		// adding the scheme is the gesture.
		return head + fmt.Sprintf("The webhook address %s is not an absolute http(s) URL: this deployment's "+
			"public URL (auth.publicUrl / ITERION_PUBLIC_URL) must be scheme+host, so set it to https://… "+
			"(a forgotten scheme is the usual cause), or pin a base URL on this connection %s, %s",
			e.URL, hookPinGesture(e.ConnectionID), hookRefusedRetry)
	default:
		return head + "The usual cause is a forge that only delivers to allowlisted hosts: pin a webhook base URL " +
			"it accepts on this connection " + hookPinGesture(e.ConnectionID) + ", " + hookRefusedRetry
	}
}

// writeForgeProvisionError maps orchestrator/admin failures to stable HTTP
// shapes the studio can act on.
func (s *Server) writeForgeProvisionError(w http.ResponseWriter, err error) {
	var restoreFailed *forge.ConfigRestoreError
	var hookRefused *forge.HookRefusedError
	var indeterminate *forge.HookWriteIndeterminateError
	switch {
	case errors.As(err, &restoreFailed):
		// First: the repo has stopped receiving deliveries, which outranks the
		// hook failure it is joined with — whichever case matched that one would
		// answer with the hook's story alone. Logged whole: the answer goes to
		// one operator, and this one needs ops to see it too.
		s.logWarn("forge: provisioning failed and the stored webhook config could not be restored: %v", err)
		if errors.As(err, &hookRefused) {
			// A refused ADDRESS joined with a failed restore is two facts, not
			// one: the repo stopped receiving (500, the restore's story, said
			// first) AND the only gesture that makes the next attempt succeed
			// is a webhook base the forge accepts. The machine code travels
			// with it, since a 500 whose cause is a refused URL is still that
			// refusal for whatever the studio renders for it.
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
				"error":  "hook_url_refused",
				"detail": restoreFailed.Error() + " " + hookRefusedDetail(hookRefused),
			})
			return
		}
		httpError(w, http.StatusInternalServerError, "%v", err)
	case errors.As(err, &hookRefused):
		// Ahead of the upstream mapping, which would mirror the forge's 422
		// with "provisioning failed: …HTTP 422" — a status the operator
		// cannot act on, for the one provisioning failure whose fix is a
		// setting on this very connection. Logged whole, because the detail
		// is capped and the forge's message may name a cause the remedy only
		// hedges at.
		s.logWarn("forge: the webhook address was refused: %v", err)
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "hook_url_refused",
			"detail": hookRefusedDetail(hookRefused),
		})
	case errors.Is(err, forge.ErrLaunchVarsDrop):
		// The operator's own request is inadmissible as sent: it would drop
		// pins it did not name. 400, with the echo and the escape hatch in
		// the message — never a silent partial apply.
		httpError(w, http.StatusBadRequest, "%v", err)
	case errors.Is(err, forge.ErrProvisionDiverged):
		// iterion's own stores diverged mid-write; the message carries the
		// converge-on-rerun remedy. Not the forge's fault, so not a 502.
		httpError(w, http.StatusInternalServerError, "%v", err)
	case errors.Is(err, forge.ErrProjectBotNotPermitted):
		// The forge refused to mint the repo's bot for the authorising
		// account. GitLab answers one 400 for two causes, so both remedies
		// are named; its own message rides along and says which.
		writeJSONStatus(w, http.StatusForbidden, map[string]any{
			"error":  "project_bot_not_permitted",
			"detail": projectBotNotPermittedDetail(err),
		})
	case errors.Is(err, forge.ErrProjectBotUnsupported):
		// A configuration contradiction (a project-bot connection on a client
		// that cannot mint one), not a forge answer: the operator reconnects.
		httpError(w, http.StatusUnprocessableEntity, "%v — reconnect the forge without the project-bot identity", err)
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
	case errors.As(err, &indeterminate):
		// The stored config may not match the forge's live hook, so the repo
		// may have stopped authenticating until someone re-runs the request.
		// Ahead of the upstream mapping only to log it: a state that needs an
		// operator gesture cannot live in the one browser tab that asked, and
		// the mapping (a 502 for the forge's 5xx, a 429 with its Retry-After)
		// is what the answer keeps. The text already went through the hook
		// secret's scrub.
		s.logWarn("forge: webhook %s: a hook write's outcome is unknown, so the repo may be delivering with a secret the forge does not hold — re-run the request: %v",
			indeterminate.WebhookID, err)
		if !writeForgeUpstreamError(w, err, "provisioning failed: %v", err) {
			httpError(w, http.StatusBadGateway, "provisioning failed: %v", err)
		}
	case writeForgeUpstreamError(w, err, "provisioning failed: %v", err):
		// A forge that throttled or fell over says so itself.
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
	// (fail toward approval, by the predicate's own doctrine).
	if len(req.ScheduleCrons) > 0 {
		return true
	}
	// A routing block (ADR-129): a per-repo provider/model/effort pin moves
	// the repo's unattended spend — the binding level outranks the team and
	// org records in the launch fold — and this predicate cannot tell a
	// tightening (a `providers:` narrowing, a `null` clear) from a
	// widening (an `overrides:` cell onto a pricier backend) at that
	// surface. The ScheduleCrons precedent above: park every SPOKEN block,
	// null included. Absent keeps going direct — enabling one more bot
	// must not demand the org's approval for a pin nobody touched.
	if len(req.Routing) > 0 {
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

// webhookPatchExpandsSurface is expandsProvisionSurface's twin for the
// generic webhook CRUD: a MANAGED (forge-provisioned) config carries the
// same automation switches the approval gate parks, and PATCHing them
// directly would bypass the org's decision (found live by review round 3).
// Same doctrine: only EXPANSIONS are caught; tightenings stay direct.
func webhookPatchExpandsSurface(cfg webhooks.Config, req webhookConfigReq) bool {
	if req.BotIDs != nil && addsBots(cfg.BotIDs, req.BotIDs) {
		return true
	}
	if req.WildcardBots != nil && *req.WildcardBots && !cfg.WildcardBots {
		return true
	}
	if req.HoldLabels != nil && removesAny(cfg.HoldLabels, req.HoldLabels) {
		return true
	}
	if req.LabelAllowlist != nil && len(cfg.LabelAllowlist) > 0 &&
		(len(req.LabelAllowlist) == 0 || addsBots(cfg.LabelAllowlist, req.LabelAllowlist)) {
		return true
	}
	// The zero-touch implement-on-open lane, turned ON.
	if req.AutoImplementOnOpen != nil && *req.AutoImplementOnOpen && !cfg.AutoImplementOnOpen {
		return true
	}
	if req.Overlap != nil && strings.TrimSpace(*req.Overlap) == "allow" &&
		cfg.Overlap != "" && cfg.Overlap != "allow" {
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

// writeProvisionResult audits and answers a provisioning write. warnings is
// the blast-radius screen's output (a WARN is not a refusal — the write
// landed and the response carries what it could not gate); an empty list
// answers the bare result, byte-identical to the pre-screening response.
func (s *Server) writeProvisionResult(w http.ResponseWriter, r *http.Request, teamID, auditAction string, res forge.ProvisionResult, warnings []string, auditMeta map[string]any) {
	s.auditTenant(r, teamID, auditAction, "forge_integration", res.IntegrationID, auditMeta)
	if len(warnings) > 0 {
		writeJSON(w, struct {
			forge.ProvisionResult
			Warnings []string `json:"warnings,omitempty"`
		}{ProvisionResult: res, Warnings: warnings})
		return
	}
	writeJSON(w, res)
}

// routingBlastRadiusWarnings screens a submitted routing block against the
// manifests of the bots ENABLED on the integration, naming every bot and
// node whose effective ask-rules the overridden backend could not gate.
// The write surface cannot see the blast radius of a `*` cell on its own —
// the block applies to every bot the webhook fans out to, and the launch
// refusal (runview.ValidateModelOverridePermissions, folded AFTER these
// cells at service_launch.go) is the only thing that would tell the
// operator, one failed delivery at a time. So the surface screens and
// WARNS (C1xx posture, philosophy §1: warn rather than reject). NOT an
// enforcement: a bot enabled after the write is never screened here, and
// a more specific cell below the binding (the bot's manifest, the run
// layer) can still win where this screen saw a crossing.
func (s *Server) routingBlastRadiusWarnings(ctx context.Context, teamID string, botIDs []string, block *llmroute.Policy) []string {
	if block == nil || len(block.Overrides) == 0 {
		return nil
	}
	snapshot := &store.RunLLMRoutePolicy{}
	for _, o := range block.Overrides {
		snapshot.Overrides = append(snapshot.Overrides, store.RunLLMRouteOverride{
			Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort,
		})
	}
	var warnings []string
	for _, botID := range botIDs {
		lb, err := s.resolveBotSource(ctx, teamID, botID)
		if err != nil {
			// An unresolvable bot cannot be screened — say so rather than
			// answer a silent all-clear: the launch refusal is the
			// enforcement, and this warning tells the operator which bot
			// the screen could not judge.
			warnings = append(warnings, fmt.Sprintf("routing: bot %q could not be resolved for the blast-radius screen (%v); its launches are screened at launch time only", botID, err))
			continue
		}
		// The screen owns what resolution materialized, exactly like the
		// launch lanes that defer lb.Cleanup(): a stored bot's bundle lives
		// in a temp dir, and a screen that skips the collection leaks one
		// directory per screened bot per spoken routing write.
		reasons, serr := runview.ScreenPolicyOverrides(lb.Path, lb.Source, lb.BundleDir, snapshot)
		lb.Cleanup()
		if serr != nil {
			warnings = append(warnings, fmt.Sprintf("routing: bot %q could not be screened (%v); its launches are screened at launch time only", botID, serr))
			continue
		}
		for _, reason := range reasons {
			warnings = append(warnings, fmt.Sprintf("routing: bot %q: %s", botID, reason))
		}
	}
	return warnings
}
