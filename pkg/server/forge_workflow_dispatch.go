package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Workflow dispatch through the deterministic publish surface.
//
// A bot that declares the `actions` scope in its manifest
// (bundle.ForgeScopeActions), launched by a repo-bound schedule, carries
// publishCapDispatchWorkflow on its run's publish grant — bound to the ONE
// workflow file the schedule row names — and may ask the server to dispatch
// that workflow of the grant repository: POST /api/v1/forge/dispatch-workflow,
// bearer = the grant token (X-Iterion-Run). The server dispatches with a
// GitHub-App installation token minted for that one call, narrowed to that
// repository and to {actions: write, metadata: read} — the run's own forge
// token never carries the Actions permission, and this one never reaches the
// pod. A grant dispatches at most once.
//
// Every answer that is not a dispatch carries a typed code: `code` in a 4xx
// body, `error_code` in a 5xx one (a 5xx body is reduced to its code at the
// HTTP boundary; the reason stays in the server log). Every refusal the
// server decides is decided before the forge is asked.
// ---------------------------------------------------------------------------

// forgeDispatchWorkflowPath is the dispatch endpoint. Like the other grant
// surfaces it authenticates its own bearer, so the operator-JWT gate does not
// front it (isAuthExempt).
const forgeDispatchWorkflowPath = "/api/v1/forge/dispatch-workflow"

// dispatchWorkflowVar is the launch var naming the workflow file a dispatch
// bot starts. The schedule lane binds the grant to the value on the schedule
// row — the operator's own, written by a team admin — so the run can dispatch
// that file and no other, whatever its own inputs say.
const dispatchWorkflowVar = "workflow"

// The typed codes the dispatch endpoint answers with — what a bot reads from
// the `code` (or, on a 5xx, `error_code`) field, a log line greps for and a
// test asserts. grantUntrustedRunReason and grantTenantMismatchReason are
// runOwnsGrant's own words, reused rather than respelled.
const (
	dispatchRefusedNoToken      = "workflow_dispatch_no_grant_token"
	dispatchRefusedUnknownGrant = "workflow_dispatch_grant_unknown"
	dispatchRefusedNotEnabled   = "workflow_dispatch_not_enabled"
	dispatchRefusedBadRequest   = "workflow_dispatch_bad_request"
	dispatchRefusedNoCapability = "workflow_dispatch_no_capability"
	dispatchRefusedBadFile      = "workflow_dispatch_bad_workflow_file"
	dispatchRefusedWorkflow     = "workflow_dispatch_workflow_outside_grant"
	dispatchRefusedRepo         = "workflow_dispatch_repo_outside_grant"
	dispatchRefusedRun          = "workflow_dispatch_run_not_grant_holder"
	dispatchRefusedRunOver      = "workflow_dispatch_run_over"
	dispatchRefusedNoConnection = "workflow_dispatch_connection_missing"
	dispatchRefusedUnsupported  = "workflow_dispatch_unsupported"
	dispatchRefusedRepeat       = "workflow_dispatch_repeat"
	dispatchRefusedPermission   = "workflow_dispatch_permission_missing"
	dispatchRefusedCredential   = "workflow_dispatch_credential_unavailable"
	dispatchRefusedByForge      = "workflow_dispatch_refused_by_forge"
	dispatchUnconfirmed         = "workflow_dispatch_unconfirmed"
	dispatchUnavailable         = "workflow_dispatch_unavailable"
)

// auditActionWorkflowDispatch is the audit action of every dispatch the
// endpoint attempted on the forge — landed or not — on the grant's team.
const auditActionWorkflowDispatch = "forge.workflow.dispatch"

// errWorkflowDispatchUnsupported marks a connection the server cannot
// dispatch through: a provider with no dispatch capability in iterion, or a
// GitHub connection that is not a GitHub App — the only kind a narrow
// per-call token can be minted from.
var errWorkflowDispatchUnsupported = errors.New("workflow dispatch is not supported on this connection")

// grantDispatch is the dispatch claimed on a grant: which run presented it,
// for which workflow, when.
type grantDispatch struct {
	RunID    string    `json:"run_id"`
	Workflow string    `json:"workflow"`
	At       time.Time `json:"at"`
}

type dispatchWorkflowRequest struct {
	// RunID is the run presenting the grant ({{run.id}}). It is checked
	// against the run record, which must hold this very grant token: the
	// trust, tenant and liveness of THAT run decide.
	RunID string `json:"run_id"`
	// Repo is the repository the caller means ("owner/name"). It must be
	// the grant's: a mismatch is refused, never retargeted.
	Repo string `json:"repo"`
	// Workflow is the workflow file's basename ("renovate.yml"). It must be
	// the one the grant is bound to.
	Workflow string `json:"workflow"`
}

type dispatchWorkflowResponse struct {
	Dispatched bool `json:"dispatched"`
	// Code is the typed refusal (empty when dispatched).
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// Workflow / Ref name what was dispatched — the ref is the repository's
	// default branch, read from the forge at dispatch time.
	Workflow string `json:"workflow,omitempty"`
	Ref      string `json:"ref,omitempty"`
	// WorkflowURL is the workflow's runs page on the forge.
	WorkflowURL string `json:"workflow_url,omitempty"`
}

// handleForgeDispatchWorkflow is POST /api/v1/forge/dispatch-workflow.
func (s *Server) handleForgeDispatchWorkflow(w http.ResponseWriter, r *http.Request) {
	var req dispatchWorkflowRequest
	repo := ""
	refuse := func(status int, code, reason string) {
		s.logWarn("forge dispatch: %q by run %q on %q refused (%s): %s", req.Workflow, req.RunID, repo, code, reason)
		writeDispatchAnswer(w, status, dispatchWorkflowResponse{Code: code, Reason: reason, Repo: repo, Workflow: req.Workflow})
	}
	token := strings.TrimSpace(r.Header.Get("X-Iterion-Run"))
	if token == "" {
		refuse(http.StatusUnauthorized, dispatchRefusedNoToken, "missing X-Iterion-Run header (the run's forge_publish_token)")
		return
	}
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		refuse(http.StatusNotFound, dispatchRefusedNotEnabled, "workflow dispatch is not enabled on this server (no forge connections wired)")
		return
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		refuse(http.StatusUnauthorized, dispatchRefusedUnknownGrant, "unknown or expired run token")
		return
	}
	repo = grant.Repo
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	// Decision 3 (#3130): the default branch, no inputs. A body naming
	// anything else — a ref, inputs — is refused, never silently dropped.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		refuse(http.StatusBadRequest, dispatchRefusedBadRequest,
			fmt.Sprintf("invalid request body (want exactly {run_id, repo, workflow}; a dispatch runs on the default branch with no inputs): %v", err))
		return
	}
	req.RunID, req.Repo, req.Workflow = strings.TrimSpace(req.RunID), strings.TrimSpace(req.Repo), strings.TrimSpace(req.Workflow)
	if req.RunID == "" || req.Repo == "" || req.Workflow == "" {
		refuse(http.StatusBadRequest, dispatchRefusedBadRequest, "run_id, repo and workflow are all required")
		return
	}

	// (1) The capability: minted at a repo-bound schedule's tick from the
	// manifest of the bundle it launched, never a request parameter.
	if !slices.Contains(grant.Capabilities, publishCapDispatchWorkflow) {
		refuse(http.StatusForbidden, dispatchRefusedNoCapability,
			"the publish grant lacks capability "+publishCapDispatchWorkflow+" — it is minted only for a repo-bound schedule's run of a bot declaring forge.token_scopes actions: write")
		return
	}
	// (2) The file: it lands in a forge API path.
	if !forge.ValidWorkflowFile(req.Workflow) {
		refuse(http.StatusBadRequest, dispatchRefusedBadFile,
			"workflow must be a plain *.yml or *.yaml file name (no directory, no traversal), e.g. renovate.yml")
		return
	}
	// (3) The workflow: the one the grant was bound to at mint, never one
	// the run picks.
	if grant.DispatchWorkflow == "" || req.Workflow != grant.DispatchWorkflow {
		refuse(http.StatusForbidden, dispatchRefusedWorkflow,
			fmt.Sprintf("the grant is bound to workflow %q (the schedule's %s var), not %q", grant.DispatchWorkflow, dispatchWorkflowVar, req.Workflow))
		return
	}
	// (4) The repository: the grant's own, never retargeted.
	if !strings.EqualFold(req.Repo, grant.Repo) {
		refuse(http.StatusForbidden, dispatchRefusedRepo,
			fmt.Sprintf("the run token is scoped to repository %q, not %q", grant.Repo, req.Repo))
		return
	}
	// (5) The run: it must hold this very grant, be one the grant may speak
	// for — runOwnsGrant decides trust (a fork-lane run never dispatches)
	// and tenancy, and audits a refusal itself — and still be running.
	if s.cfg.Store == nil {
		refuse(http.StatusServiceUnavailable, dispatchUnavailable, "no run store is wired, so the run presenting the grant cannot be verified")
		return
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(r.Context()), req.RunID)
	if err != nil && !errors.Is(err, store.ErrRunNotFound) {
		// The store did not answer: that says nothing about the run, so it
		// is not a refusal of it.
		refuse(http.StatusServiceUnavailable, dispatchUnavailable, "the run presenting the grant could not be read: "+err.Error())
		return
	}
	if err != nil || run == nil || !sameGrantToken(runInputString(run, forgePublishVarToken), token) {
		refuse(http.StatusForbidden, dispatchRefusedRun, "run "+req.RunID+" does not hold this publish grant")
		return
	}
	if !s.runOwnsGrant(run, grant, "workflow dispatch") {
		code := grantTenantMismatchReason
		if !run.Trust.Trusted() {
			code = grantUntrustedRunReason
		}
		refuse(http.StatusForbidden, code, "the run may not present this grant: a run whose workspace holds code the tenant did not write, or of another tenant, never dispatches")
		return
	}
	if runIsOver(run) {
		refuse(http.StatusForbidden, dispatchRefusedRunOver,
			fmt.Sprintf("run %s is over (%s) — a grant outlives its run for the gate repairs, never for a dispatch", run.ID, run.Status))
		return
	}
	// (6) The connection: a provider the server can dispatch through.
	conn, err := s.forgeConnections.Get(r.Context(), grant.TeamID, grant.ConnectionID)
	if err != nil {
		refuse(http.StatusNotFound, dispatchRefusedNoConnection, "the grant's forge connection "+grant.ConnectionID+" could not be read: "+err.Error())
		return
	}
	if err := workflowDispatchSupported(conn); err != nil {
		refuse(http.StatusUnprocessableEntity, dispatchRefusedUnsupported, err.Error())
		return
	}
	// (7) One dispatch per grant, claimed before the forge is asked, so no
	// replay, loop, fork or sub-bot presenting the same grant fans it out.
	claimed, prior, err := s.claimWorkflowDispatch(token, run.ID, req.Workflow)
	if err != nil {
		refuse(http.StatusServiceUnavailable, dispatchUnavailable, "the dispatch claim could not be recorded on the grant: "+err.Error())
		return
	}
	if !claimed {
		refuse(http.StatusConflict, dispatchRefusedRepeat,
			fmt.Sprintf("this grant already dispatched (or attempted) %s, by run %s at %s — one dispatch per grant; the schedule's next tick is the retry",
				prior.Workflow, prior.RunID, prior.At.Format(time.RFC3339)))
		return
	}

	out := dispatchWorkflowResponse{Repo: grant.Repo, Workflow: req.Workflow}
	status := http.StatusOK
	wd, err := s.workflowDispatcherFor(r.Context(), conn, grant.Repo)
	switch {
	case errors.Is(err, forge.ErrPermissionsNotGranted):
		status, out.Code, out.Reason = http.StatusForbidden, dispatchRefusedPermission, err.Error()
	case err != nil:
		status, out.Code, out.Reason = http.StatusBadGateway, dispatchRefusedCredential, err.Error()
	default:
		res, derr := wd.DispatchWorkflow(r.Context(), grant.Repo, req.Workflow, "")
		switch {
		case errors.Is(derr, forge.ErrWorkflowNotDispatched):
			// The forge answered, and its answer is the caller's to read: an
			// unknown workflow, one without a manual trigger. A 4xx keeps the
			// reason in the body (a 5xx body is reduced to its code).
			status, out.Code, out.Reason = http.StatusUnprocessableEntity, dispatchRefusedByForge, derr.Error()
		case derr != nil:
			status, out.Code, out.Reason = http.StatusBadGateway, dispatchUnconfirmed,
				"the forge did not answer the dispatch, so whether the workflow was queued is unknown: "+derr.Error()
		default:
			out.Dispatched, out.Ref = true, res.Ref
			out.WorkflowURL = strings.TrimRight(conn.BaseURL(), "/") + "/" + grant.Repo + "/actions/workflows/" + req.Workflow
		}
	}
	s.auditSystem(grant.TeamID, "forge-dispatch", auditActionWorkflowDispatch, "run", run.ID, map[string]any{
		"repo":       grant.Repo,
		"workflow":   req.Workflow,
		"ref":        out.Ref,
		"dispatched": out.Dispatched,
		"code":       out.Code,
		"bot":        grant.Bot,
	})
	if out.Dispatched {
		s.infof("forge dispatch: run %s dispatched %s on %s@%s", run.ID, req.Workflow, grant.Repo, out.Ref)
		writeJSON(w, out)
		return
	}
	s.logWarn("forge dispatch: run %s: %s on %s not dispatched (%s): %s", run.ID, req.Workflow, grant.Repo, out.Code, out.Reason)
	writeDispatchAnswer(w, status, out)
}

// writeDispatchAnswer writes a dispatch answer that is not a success. A 5xx
// body is reduced to its error_code at the HTTP boundary (httpx.WriteErrorJSON
// keeps the detail in the log, never on the wire), so the typed code rides
// that one field there; a 4xx keeps the whole answer. No answer is
// retryable: the claim is spent before the forge is asked, so the same grant
// can only be refused again — the schedule's next tick, under a grant of its
// own, is the retry, and for an unconfirmed dispatch a retry is exactly what
// must not happen.
func writeDispatchAnswer(w http.ResponseWriter, status int, out dispatchWorkflowResponse) {
	if status >= 500 {
		writeJSONStatus(w, status, map[string]any{"error_code": out.Code, "error": out.Reason, "retryable": false})
		return
	}
	writeJSONStatus(w, status, out)
}

// sameGrantToken compares a run's recorded grant token with the presented
// one in constant time; an empty record never matches.
func sameGrantToken(recorded, presented string) bool {
	recorded, presented = strings.TrimSpace(recorded), strings.TrimSpace(presented)
	return recorded != "" && subtle.ConstantTimeCompare([]byte(recorded), []byte(presented)) == 1
}

// workflowDispatchSupported is THE predicate deciding which connections the
// server dispatches through: a GitHub App connection. GitLab and Forgejo have
// no dispatch path in iterion; a GitHub OAuth or PAT connection cannot mint
// the narrow per-call token the dispatch is made with, and its own credential
// carries whatever scope its owner gave it. A provider gaining a dispatch
// path brings its credential with it (workflowDispatcherFor), never the
// connection's runtime one.
func workflowDispatchSupported(conn forge.Connection) error {
	if conn.Provider != forge.ProviderGitHub {
		return fmt.Errorf("%w: provider %s has no workflow dispatch in iterion", errWorkflowDispatchUnsupported, conn.Provider)
	}
	if conn.Kind != forge.KindGitHubApp {
		return fmt.Errorf("%w: connection %s is a %s connection — a dispatch is made with a token minted per call from a GitHub App connection", errWorkflowDispatchUnsupported, conn.ID, conn.Kind)
	}
	return nil
}

// claimWorkflowDispatch records the grant's one dispatch — runID presenting
// it, for workflow — unless a claim is already there, atomically under the
// grant store's update. claimed is false when an earlier call holds the
// claim, which prior then describes.
func (s *Server) claimWorkflowDispatch(token, runID, workflow string) (claimed bool, prior grantDispatch, err error) {
	found, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		// Reset on every call: the Valkey update re-runs fn when the grant
		// changed under it, and a pass that found a claim must not inherit
		// an earlier pass's "claimed".
		claimed, prior = false, grantDispatch{}
		if len(g.Dispatches) > 0 {
			prior = g.Dispatches[0]
			return
		}
		g.Dispatches = append(g.Dispatches, grantDispatch{RunID: runID, Workflow: workflow, At: time.Now().UTC()})
		claimed = true
	})
	if err != nil {
		return false, grantDispatch{}, err
	}
	if !found {
		return false, grantDispatch{}, errors.New("the grant expired or was revoked")
	}
	return claimed, prior, nil
}

// workflowDispatcherFor resolves the dispatch capability for (conn, repo),
// backed by a token minted for this one call.
func (s *Server) workflowDispatcherFor(ctx context.Context, conn forge.Connection, repo string) (forge.WorkflowDispatcher, error) {
	tok, err := s.forgeWorkflowDispatchTokenMinter(ctx, conn, repo)
	if err != nil {
		return nil, err
	}
	client := &forgegithub.AdminClient{
		HTTP:    forge.InstallationClient(s.forgeHTTPClient(), conn.InstallationID),
		APIBase: forgegithub.APIBaseFor(conn.BaseURL()),
		Token:   tok,
	}
	return client, nil
}

// forgeWorkflowDispatchTokenMinter mints a GitHub App installation token for
// ONE workflow dispatch: forgegithub.WorkflowDispatchInstallationPermissions
// ({actions: write, metadata: read}), narrowed to the one repository. It
// follows forgeSecurityTokenMinter: a dedicated permission set, never the
// runtime or management set, and a grant KNOWN to lack the permission fails
// before the mint with the remediation named. An unknown grant set (a
// connection recorded before grants were) still attempts the mint, whose 422
// is typed forge.ErrPermissionsNotGranted.
func (s *Server) forgeWorkflowDispatchTokenMinter(ctx context.Context, conn forge.Connection, repo string) (string, error) {
	if err := workflowDispatchSupported(conn); err != nil {
		return "", err
	}
	if len(conn.GrantedPermissions) > 0 {
		switch conn.GrantedPermissions[forgegithub.PermissionActions] {
		case "write", "admin":
		default:
			org := conn.InstallationAccount
			if org == "" {
				org = conn.AccountLogin
			}
			return "", fmt.Errorf("%w: the installation for %q lacks 'Actions: Read and write' — add the permission on the GitHub App, have an org admin approve the pending request, then retry",
				forge.ErrPermissionsNotGranted, org)
		}
	}
	name := shortRepoName(repo)
	if name == "" {
		return "", fmt.Errorf("forge dispatch: %q names no repository", repo)
	}
	cfg, _, err := s.githubAppConfigForConnection(ctx, conn)
	if err != nil {
		return "", err
	}
	tok, _, err := forgegithub.MintInstallationToken(ctx, s.forgeHTTPClient(),
		forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC(),
		&forgegithub.InstallationTokenOptions{
			Repositories: []string{name},
			Permissions:  forgegithub.WorkflowDispatchInstallationPermissions(),
		})
	return tok, err
}

// scheduledDispatchGrant mints the publish grant of a repo-bound schedule's
// run, for a bot whose manifest declares the workflow-dispatch capability —
// the one gesture a run that names no pull request can make through the
// publish surface. m is the manifest of the bundle the tick launches
// (launchBot.Manifest, the tier that supplied the source), never a second
// lookup: a team row that shadows a catalogue slug without declaring the
// scope gets nothing. Every other scheduled launch mints nothing, exactly as
// before.
//
// The grant is bound to the schedule's integration — its team, connection
// and repository — carries ONLY publishCapDispatchWorkflow, is bound to the
// one workflow file the schedule row names (dispatchWorkflowVar) and to no
// pull request (NoPullRequest): a schedule names none, so the review, the
// merge-gate commit status, the verdict gestures, the pull-request read, the
// delivery preflight and the server's gate repairs all refuse it, typed,
// before any forge call (pullRequestGrant). The dispatch endpoint is the one
// surface it reaches, for that one workflow, once.
// vars is never mutated: a copy carries the grant.
//
// Three shapes launch without a grant, said in the log; the bot answers its
// own typed code. A schedule that names no integration (it cannot be bound to
// a repository); one whose row names no workflow (the bot ends NO_WORKFLOW);
// and a dispatch the operator disarmed with forge_publish_mutations set to
// anything but "true" — in the integration's launch policy, read through
// repoLaunchPolicy exactly as the pull-request lane reads it, or on the
// schedule row. A disarmed bot is minted nothing: a grant with no capability
// and no pull request serves no surface at all.
func (s *Server) scheduledDispatchGrant(ctx context.Context, sb cloudsched.ScheduledBot, m *bundle.Manifest, vars map[string]string) (map[string]string, mintedGrant, error) {
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		return vars, "", nil
	}
	if !declaresWorkflowDispatch(m) {
		return vars, "", nil
	}
	if sb.RepoIntegrationID == "" {
		s.logWarn("forge dispatch: schedule %s (%s) declares the workflow-dispatch capability but pins no repo integration — no grant minted, the run cannot dispatch", sb.ID, sb.BotID)
		return vars, "", nil
	}
	workflow := strings.TrimSpace(vars[dispatchWorkflowVar])
	if workflow == "" {
		s.infof("forge dispatch: schedule %s (%s) names no %s var — no grant minted, the run ends NO_WORKFLOW", sb.ID, sb.BotID, dispatchWorkflowVar)
		return vars, "", nil
	}
	if s.forgeIntegrations == nil {
		return vars, "", fmt.Errorf("schedule %s pins repo integration %s but the forge layer is not wired", sb.ID, sb.RepoIntegrationID)
	}
	integ, err := s.forgeIntegrations.Get(store.WithoutTenantFilter(ctx), sb.RepoIntegrationID)
	if err != nil {
		return vars, "", fmt.Errorf("schedule %s: resolve repo integration %s for the dispatch grant: %w", sb.ID, sb.RepoIntegrationID, err)
	}
	if integ.TenantID != sb.TenantID {
		return vars, "", fmt.Errorf("schedule %s: repo integration %s belongs to another tenant", sb.ID, sb.RepoIntegrationID)
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), integ.TenantID, integ.ConnectionID)
	if err != nil {
		return vars, "", fmt.Errorf("schedule %s: resolve forge connection %s for the dispatch grant: %w", sb.ID, integ.ConnectionID, err)
	}
	policy := s.repoLaunchPolicy(ctx, integ, sb.BotID)
	for _, pin := range []string{policy[publishMutationKillSwitchVar], vars[publishMutationKillSwitchVar]} {
		if ks := strings.TrimSpace(pin); ks != "" && ks != "true" {
			s.infof("forge dispatch: schedule %s on %s: workflow dispatch disarmed by operator pin %s=%q — no grant minted",
				sb.ID, integ.RepoFullName, publishMutationKillSwitchVar, ks)
			return vars, "", nil
		}
	}
	base := s.publicBaseURL(nil)
	if base == "" {
		return vars, "", fmt.Errorf("schedule %s: no public base URL (set PublicURL) — the dispatch endpoint the run calls cannot be named", sb.ID)
	}
	out := maps.Clone(vars)
	if out == nil {
		out = map[string]string{}
	}
	return s.registerRunGrant(ForgePublishGrant{
		TeamID: sb.TenantID, Bot: strings.TrimSpace(sb.BotID), ConnectionID: conn.ID, Repo: integ.RepoFullName,
		Capabilities:     []string{publishCapDispatchWorkflow},
		DispatchWorkflow: workflow,
		NoPullRequest:    true,
	}, hostOfURL(conn.BaseURL()), out, base)
}
