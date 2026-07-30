package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// registerGitLabWebhookRoute wires the inbound GitLab delivery endpoint
// behind webhookAuth. The single route dispatches on X-Gitlab-Event so
// MR opens and `/revi` note commands share one provider URL — exactly
// the path the operator pastes into GitLab's "Webhook URL" field.
func (s *Server) registerGitLabWebhookRoute() {
	s.mux.Handle("POST /api/webhooks/gitlab/{id}", s.webhookAuth(webhooks.ProviderGitLab, http.HandlerFunc(s.handleGitLabWebhook)))
}

// handleGitLabWebhook is the entry point for every GitLab delivery on
// this route. Auth, rate-limit, quota, suspend-check and tenant
// stamping are already done by webhookAuth; the config is on ctx. The
// only thing this function does itself is pick the per-event-kind sub-
// handler — everything provider-specific lives in handleGitLab* below.
func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, ok := webhookConfigFromContext(ctx)
	if !ok {
		httpError(w, http.StatusInternalServerError, "webhook context missing")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	payloadHash := knowledge.ChecksumHex(body)
	srcIP := s.clientIP(r)

	switch r.Header.Get("X-Gitlab-Event") {
	case gitlab.EventHeaderMergeRequest:
		s.handleGitLabMergeRequestEvent(ctx, w, r, cfg, body, payloadHash, srcIP)
	case gitlab.EventHeaderNote:
		// Conversational layer: a /revi command (or, later, a reply to
		// the bot's thread). See docs/forge-conversations.md.
		s.handleGitLabNote(ctx, w, r, cfg, body, payloadHash, srcIP)
	case gitlab.EventHeaderIssue:
		// Issue lifecycle: adding a trigger label (e.g. "implement") launches
		// an implementer bot that opens an MR back-linked to the issue, and
		// materialises a one-way tracking card on the board.
		s.handleGitLabIssueEvent(ctx, w, r, cfg, body, payloadHash, srcIP)
	default:
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{}, webhooks.StatusInvalid, payloadHash, srcIP, "unsupported X-Gitlab-Event")
		httpError(w, http.StatusBadRequest, "unsupported event (merge_request, note or issue only)")
	}
}

// handleGitLabMergeRequestEvent handles a verified MR open/reopen. The
// merge-request path covers the auto-launch ("review on open") leg;
// pushes do NOT re-trigger (see gitlab.IsReviewable) — re-review is
// on-demand via the `/revi` Note hook below.
func (s *Server) handleGitLabMergeRequestEvent(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	p, err := gitlab.ParseMergeRequest(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "merge_request"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid merge_request payload")
		return
	}
	meta := gitlabMRMeta(p)

	// Filter: only review on open/reopen, allowed event + project.
	// A filtered delivery returns 200 (a 4xx would make GitLab disable
	// the webhook after repeated metadata-only edits).
	// ReviewOnSync opts a push-to-MR ("update" with a new head) back into
	// review so the merge-gate status re-evaluates on the new head SHA.
	reviewable := p.IsReviewable() || (cfg.ReviewOnSync && p.IsSynchronize())
	if !reviewable ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "merge_request", "merge_request", "note") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
		!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderUsername) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Hold-label gate (bot-agnostic, opt-in): a configured hold label on the MR
	// vetoes the auto-review. Same escape hatch as the GitHub PR path.
	if s.suppressedByHoldLabel(ctx, w, cfg, meta, p.Labels, payloadHash, srcIP) {
		return
	}

	// Iterion-bot guard: an MR opened by iterion's own forge bot already
	// converged in its own loop — skip the auto-review (a human can still run
	// `/revi`). Mirror of the GitHub PR-open path.
	if s.isIterionForgeBotAuthor(ctx, cfg, p.SenderUsername) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"MR authored by iterion's forge bot — auto-review skipped (self-produced; run /revi to force a review)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Same per-bot fan-out as the GitHub PR lane — this MR lane is a separate
	// function, so parity is not free and must be kept explicitly.
	//
	// Routes on the event actor rather than the MR's author, unlike GitHub:
	// GitLab's merge_request payload carries only `author_id`, so resolving
	// the author's username would need an extra API call on the hot path. The
	// difference shows only on a re-review triggered by someone other than
	// the MR author, which needs ReviewOnSync to be on in the first place.
	rules := s.resolveForgeEventBots(cfg, bundle.ForgeEventPullRequest, p.SenderUsername)
	if len(rules) == 0 {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "no enabled bot claims this MR (event/author routing)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Idempotency: one launch per (tenant, webhook, project, MR, head sha) —
	// per bot once the delivery fans out. The "mr|" prefix keeps the key space
	// disjoint from the Note hook ("note|") so a /revi on the same MR can't
	// collide with the open.
	idemBase := fmt.Sprintf("mr|%s|%s|%d|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.MRIID, p.HeadSHA)

	targets := forgePREventTargets(cfg, rules, idemBase, p.MRURL, p.TargetBranch,
		strings.TrimSpace(p.Title+"\n\n"+p.Description), p.CloneURL, p.SourceBranch,
		map[string]string{"pr_author": p.SenderUsername, "source_branch": p.SourceBranch, "head_sha": p.HeadSHA})

	s.insertAndLaunchWebhookMulti(ctx, w, r, cfg, meta, targets, payloadHash, srcIP)
}

// handleGitLabIssueEvent handles a verified GitLab "Issue Hook". GitLab has no
// dedicated "labeled" action, so the parser diffs changes.labels; a launch
// fires only when a freshly-added label passes the webhook's LabelAllowlist on
// an OPEN issue (allowed event + project). It routes through dispatchInvocation
// (same as the slash-command path) so a one-way tracking card is materialised
// and the run launches with the issue as the feature task. A filtered delivery
// returns 200 so GitLab doesn't auto-disable the hook on routine issue edits.
func (s *Server) handleGitLabIssueEvent(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	p, err := gitlab.ParseIssue(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "issues"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid issue payload")
		return
	}
	meta := gitlabIssueMeta(p)
	label := webhooks.FirstMatchingLabel(cfg.LabelAllowlist, p.AddedLabels)
	if p.State != "opened" || label == "" ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "issues", "issues") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Hold-label gate (bot-agnostic, opt-in): a configured hold label on the
	// issue vetoes the labeled auto-launch. Same escape hatch as GitHub.
	if s.suppressedByHoldLabel(ctx, w, cfg, meta, p.Labels, payloadHash, srcIP) {
		return
	}

	botID, ok := s.selectIssueLabeledBot(ctx, w, cfg, meta, payloadHash, srcIP)
	if !ok {
		return
	}

	// Idempotency: one launch per (tenant, webhook, project, issue, label).
	// "gl|issue|" keeps the key space disjoint from the mr|/note|/cmd| paths.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("gl|issue|%s|%s|%d|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.IssueIID, label)))

	route := s.boardRouteForLabel(botID)
	vars := applyWebhookVarLayers(gitlabIssueLabeledVars(p, nil, route.ArgsVar), cfg)
	// An issue carries no MR source branch — the bot opens its MR from the
	// project default branch (finalize_mr cuts the branch from there).
	s.dispatchInvocation(ctx, w, r, cfg, meta, idemKey, route, vars, p.CloneURL, p.DefaultBranch, payloadHash, srcIP)
}

// gitlabIssueLabeledVars composes the implementer-bot launch vars for a
// labeled GitLab issue: the issue title+body as the feature prompt (under the
// bot's args var), open_mr, and source_issue_ref (the issue URL) for the
// back-link. Operator-pinned LaunchVars win last.
func gitlabIssueLabeledVars(p gitlab.ParsedIssue, launchVars map[string]string, argsVar string) map[string]string {
	if argsVar == "" {
		argsVar = "feature_prompt"
	}
	vars := map[string]string{
		argsVar:            strings.TrimSpace(p.Title + "\n\n" + p.Description),
		"open_mr":          "true",
		"source_issue_ref": p.URL,
	}
	mergeVarsInto(vars, launchVars)
	return vars
}

// gitlabIssueMeta flattens a parsed GitLab issue into webhookEventMeta.
// SubjectURL carries the issue URL — the back-link target ensureBoardCard
// stamps into source_issue_ref.
func gitlabIssueMeta(p gitlab.ParsedIssue) webhookEventMeta {
	return webhookEventMeta{
		Kind:         "issues",
		Action:       "labeled",
		ProjectPath:  p.ProjectPath,
		SubjectID:    p.SubjectID(),
		SubjectURL:   p.URL,
		SenderHandle: p.AuthorUsername,
	}
}

// handleGitLabNote handles an inbound GitLab note (comment / reply) — the
// conversational path (docs/forge-conversations.md). A note triggers a
// run when it is a /revi command on an OPEN MR, the author is authorized
// (allowlist OR role-gate), and it is not the bot's own note (loop-guard).
// The launch funnels through insertAndLaunchWebhook so the per-org
// admission gate, idempotent delivery flow and metrics apply exactly as
// on every other provider path. The note's id (not the head SHA) drives
// idempotency so a fresh `/revi` after a new push re-launches.
func (s *Server) handleGitLabNote(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	p, err := gitlab.ParseNote(body)
	if err != nil {
		// A structurally-valid note on a non-MR noteable (issue, commit,
		// snippet — ParseNote errors but still returns the decoded note)
		// is FILTERED with 200, not 400: operators commonly enable note
		// events broadly, and repeated 4xx make GitLab auto-disable the
		// webhook.
		if p.NoteID != 0 {
			s.recordNoteDelivery(ctx, cfg, webhooks.StatusFiltered, payloadHash, srcIP, p, "not a merge-request note")
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
			return
		}
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusInvalid, payloadHash, srcIP, p, err.Error())
		httpError(w, http.StatusBadRequest, "invalid note payload")
		return
	}
	filtered := func(reason string) {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusFiltered, payloadHash, srcIP, p, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	// Issue-surface branch: a /command on an OPEN issue routes to the generic
	// command handler with surface="issue" (so the bot opens an MR back-linking
	// the issue). Handled BEFORE the MR/`/revi` machinery — /revi + reply-in-
	// thread stay MR-only. A non-command issue note is filtered (200).
	if p.IsIssueNote() {
		if p.IssueState != "opened" ||
			!webhooks.MatchEvent(cfg.EventAllowlist, "note", "issue", "note") ||
			!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
			filtered("out of scope (not an open-issue note / event / project)")
			return
		}
		cmd, cmdArgs := p.Command()
		if cmd == "" {
			filtered("no slash-command on issue note")
			return
		}
		s.handleGitLabCommandNote(ctx, w, r, cfg, p, cmd, cmdArgs, "issue", payloadHash, srcIP)
		return
	}
	if !p.IsMergeRequestNote() || p.MRState != "opened" ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "note", "merge_request", "note") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
		filtered("out of scope (not an open-MR note / event / project)")
		return
	}
	// Trigger: a `/revi` command, OR — when the converse bot is enabled — a
	// plain reply in a thread Revi is part of (so "just replying" to Revi's
	// comment works, no command). The reply-in-thread check needs the bot
	// identity, so it runs in the gate (which resolves the forge token). A
	// non-command note with the converse bot disabled can't trigger anything.
	cmd, cmdArgs := p.Command()
	converseEnabled := s.canRouteToConverseBot(cfg)
	// Generic slash-command routing: any command but "revi" (the Revi
	// conversation pair below keeps its bespoke reply-in-thread + thread-
	// context handling). A non-revi command resolves through the command
	// registry to a bot + execution mode; an unknown command is filtered.
	if cmd != "" && cmd != "revi" {
		s.handleGitLabCommandNote(ctx, w, r, cfg, p, cmd, cmdArgs, "pr", payloadHash, srcIP)
		return
	}
	if cmd != "revi" && !converseEnabled {
		filtered("no /revi trigger")
		return
	}
	botID, ok := s.resolveReviewBot(ctx, w, cfg, gitlabNoteMeta(p), payloadHash, srcIP)
	if !ok {
		return
	}
	// Gate the replier (forge-token resolution → loop-guard → reply-in-thread
	// classification → allowlist OR role-gate) — externalities live behind a
	// seam so handler tests don't need a live GitLab. Authorisation is
	// identical whether we route to review-pr (re-review) or revi-converse
	// (in-thread answer); the forge_token binding name is shared across bots.
	gate := s.webhookNoteGate
	if gate == nil {
		gate = s.realWebhookNoteGate
	}
	authorized, replyInThread, threadContext, reason, aerr := gate(ctx, cfg, p, botID)
	if aerr != nil {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		filtered(reason)
		return
	}
	triggerLabel := "/" + cmd
	if replyInThread {
		triggerLabel = "reply"
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: gitlab note %s!%d (%s) by %s authorized (%s)", p.ProjectPath, p.MRIID, triggerLabel, p.AuthorUsername, reason)
	}

	// Route to the converse bot when there's a question to answer: `/revi
	// <question>`, or any reply in a Revi thread (the reply itself is the
	// question). Bare `/revi` (no args) stays a review-pr re-review. A reply
	// always reaches revi-converse — replyInThread implies the bot is enabled
	// (the early gate above). `/revi <question>` with the converse bot absent
	// falls back to re-review (matching pre-A5 behaviour).
	vars := applyWebhookVarLayers(s.buildGitLabNoteVars(p, cmd, cmdArgs, nil), cfg)
	question := cmdArgs
	if replyInThread {
		question = strings.TrimSpace(p.NoteBody)
	}
	if question != "" && converseEnabled {
		botID = defaultWebhookBotReviConverse
		// Drop the re_review flag for the converse path — it's a question,
		// not a fresh review — and pass the question explicitly. Other
		// conversation vars (discussion_id, trigger_note, replier) are in vars.
		delete(vars, "re_review")
		vars["converse_question"] = question
		// The discussion transcript the gate fetched — the operator's reply
		// typically references Revi's earlier review note; the bot grounds
		// its answer in it. Only the converse bot declares the var.
		if threadContext != "" {
			vars["thread_context"] = threadContext
		}
		if s.logger != nil {
			s.logger.Debug("webhooks: gitlab note %s!%d routed to %s (%s)", p.ProjectPath, p.MRIID, botID, triggerLabel)
		}
	}

	// Idempotency: one launch per note.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.SubjectID())))

	s.insertAndLaunchWebhook(ctx, w, r, cfg, gitlabNoteMeta(p), idemKey, botID, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
}

// buildGitLabNoteVars composes the launch-vars map common to both note
// routing paths (review-pr re-review and revi-converse in-thread
// answer). The conversation vars (discussion_id / trigger_note /
// replier / trigger_command / trigger_args) carry the thread context
// for both bots; re_review marks the posted summary with the 🔁
// prefix on the re-review path (the converse path deletes it before
// launching). scope_notes carries the MR context.
func (s *Server) buildGitLabNoteVars(p gitlab.ParsedNote, cmd, cmdArgs string, launchVars map[string]string) map[string]string {
	return reviewPRVars(p.MRURL, p.TargetBranch, strings.TrimSpace(p.MRTitle+"\n\n"+p.MRDesc), launchVars, map[string]string{
		"conversation_mode": "reply",
		"discussion_id":     p.DiscussionID,
		"trigger_note":      p.NoteBody,
		"trigger_command":   cmd,
		"trigger_args":      cmdArgs,
		"replier":           p.AuthorUsername,
		"re_review":         "true",
	})
}

// handleGitLabCommandNote routes a generic slash-command note (any command
// but /revi) to its bot via the command registry. It resolves the route,
// checks scope + webhook bot-scope, gates the replier (loop-guard +
// allowlist/role authz, honouring the route's per-command MinReplierRole),
// composes the launch vars (the command args land in the route's args_var),
// then hands off to dispatchInvocation. The surface ("pr" for an MR note,
// "issue" for an issue note) selects both the scope check and the vars builder.
// Every benign refusal is a 200/filtered so GitLab doesn't disable the hook.
func (s *Server) handleGitLabCommandNote(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, p gitlab.ParsedNote, cmd, cmdArgs, surface, payloadHash, srcIP string) {
	filtered := func(reason string) {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusFiltered, payloadHash, srcIP, p, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	route, ok := webhooks.ResolveCommandRoute(cfg, cmd, cmdArgs, s.cmdDiscovery())
	if !ok {
		filtered("no command route for /" + cmd)
		return
	}
	if !route.AllowsScope(surface) {
		filtered("/" + cmd + " is not enabled on " + surface + " comments")
		return
	}
	if !cfg.AllowsBot(route.BotID) {
		filtered("bot " + route.BotID + " not permitted by this webhook")
		return
	}
	gate := s.webhookCommandGate
	if gate == nil {
		gate = s.realWebhookCommandGate
	}
	authorized, reason, aerr := gate(ctx, cfg, p, route)
	if aerr != nil {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		filtered(reason)
		return
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: gitlab note %s/%s (/%s) by %s → %s (%s)", p.ProjectPath, surface, cmd, p.AuthorUsername, route.BotID, reason)
	}
	vars := applyWebhookVarLayers(buildCommandVars(p, route, cmdArgs, nil), cfg)
	if surface == "issue" {
		vars = applyWebhookVarLayers(buildGitLabIssueCommandVars(p, route, cmdArgs, nil), cfg)
	} else {
		stampBranchImprovePushBack(vars, route.BotID, p.SourceBranch, cfg.BranchImproveAsPR)
		// `/billy` on an MR seeds the run with Revi's most recent review of it
		// (best-effort — see stampPriorReview). Symmetric with the GitHub path.
		s.stampPriorReview(ctx, cfg, route.BotID, vars, p.MRURL, p.ProjectPath, int(p.MRIID))
	}
	// "cmd|" prefix keeps the key space disjoint from the mr|/note| paths.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("cmd|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.SubjectID())))
	// An issue note has no MR source branch — the repo-bound bot works off the
	// project default branch (finalize_mr cuts the MR branch from there).
	ref := p.SourceBranch
	if surface == "issue" {
		ref = p.DefaultBranch
	}
	s.dispatchInvocation(ctx, w, r, cfg, gitlabNoteMeta(p), idemKey, route, vars, p.CloneURL, ref, payloadHash, srcIP)
}

// buildCommandVars composes the launch vars for a generic command on a GitLab
// MR note: the PR context ({pr_url, base_ref, scope_notes}), the route's
// manifest ContextVars, the operator's webhook LaunchVars, then the command
// args into the route's args_var LAST so the explicit trigger payload always
// wins for its key.
func buildCommandVars(p gitlab.ParsedNote, route webhooks.CommandRoute, args string, launchVars map[string]string) map[string]string {
	vars := map[string]string{
		"pr_url":      p.MRURL,
		"base_ref":    p.TargetBranch,
		"scope_notes": strings.TrimSpace(p.MRTitle + "\n\n" + p.MRDesc),
	}
	mergeVarsInto(vars, route.ContextVars)
	mergeVarsInto(vars, launchVars)
	if route.ArgsVar != "" && strings.TrimSpace(args) != "" {
		vars[route.ArgsVar] = args
	}
	return vars
}

// buildGitLabIssueCommandVars composes the launch vars for a generic command
// on a GitLab ISSUE note (the open-MR-and-back-link surface). An issue note
// carries no MR, so pr_url is empty and base_ref is the project's default
// branch (the branch a command's MR is opened against). issue_url is the issue
// the human commented on; the opens_mr stamp on the materialised card carries
// open_mr + source_issue_ref via BotArgs (see ensureBoardCard). Ordering
// mirrors buildCommandVars: context → manifest ContextVars → operator
// LaunchVars → the command args into args_var LAST.
func buildGitLabIssueCommandVars(p gitlab.ParsedNote, route webhooks.CommandRoute, args string, launchVars map[string]string) map[string]string {
	vars := map[string]string{
		"pr_url":      "",
		"issue_url":   p.IssueURL,
		"base_ref":    p.DefaultBranch,
		"scope_notes": strings.TrimSpace(p.IssueTitle + "\n\n" + p.IssueDesc),
	}
	mergeVarsInto(vars, route.ContextVars)
	mergeVarsInto(vars, launchVars)
	if route.ArgsVar != "" && strings.TrimSpace(args) != "" {
		vars[route.ArgsVar] = args
	}
	return vars
}

// realWebhookCommandGate is the production replier gate for a generic
// slash-command: resolve the bot's forge token, reject the bot's own note
// (loop-guard), then authorize the replier (allowlist OR role-gate, using the
// route's per-command MinReplierRole, falling back to the webhook default).
// Returns ok=false + a human reason for benign refusals; err only for an
// authz infrastructure failure (502 upstream).
func (s *Server) realWebhookCommandGate(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, route webhooks.CommandRoute) (bool, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, route.BotID)
	if terr != nil || token == "" {
		return false, "no forge token resolved (configure a forge_token binding)", nil
	}
	// An issue note carries no MR URL; fall back to the issue URL so the forge
	// host (for the loop-guard + role-gate API calls) resolves on the issue
	// surface too — without this every GitLab issue command is silently refused.
	ref := p.MRURL
	if ref == "" {
		ref = p.IssueURL
	}
	baseURL, refusal := resolveForgeBaseURL(cfg, ref)
	if refusal != "" {
		return false, refusal, nil
	}
	api := gitlab.API{HTTP: s.forgeHTTPClient(), BaseURL: baseURL, Token: token}
	if bot, berr := api.CurrentUser(ctx); berr == nil && bot.ID == p.AuthorID {
		return false, "self note (loop-guard)", nil
	}
	minRole := route.MinReplierRole
	if minRole == "" {
		minRole = cfg.MinReplierRole
	}
	ok, reason, aerr := gitlab.AuthorizeReplier(ctx, api, gitlab.ReplierAuth{
		AuthorID: p.AuthorID, AuthorUsername: p.AuthorUsername, ProjectID: p.ProjectID,
		Allowlist: cfg.AuthorizedRepliers, MinRole: minRole,
	})
	if aerr != nil {
		return false, "", aerr
	}
	if !ok {
		return false, "replier not authorized: " + p.AuthorUsername, nil
	}
	return true, reason, nil
}

// canRouteToConverseBot reports whether the conversational bot can be
// launched on this webhook: both permitted by the webhook scope AND
// resolvable on disk (older deploys without the bundle gracefully
// fall back to the re-review path). The check is cheap — a single
// botregistry.ResolveBotPath scan — but happens only on `/revi
// <question>` deliveries, not on every note.
func (s *Server) canRouteToConverseBot(cfg webhooks.Config) bool {
	if !cfg.AllowsBot(defaultWebhookBotReviConverse) {
		return false
	}
	_, _, err := s.resolveBotSource(defaultWebhookBotReviConverse)
	return err == nil
}

// recordNoteDelivery inserts a terminal note-event audit row with a
// human-readable reason (richer than the generic terminal recorder —
// the conversational path has many distinct filter causes operators
// want to tell apart in the deliveries view). Metrics parity with the
// common helpers via markWebhookOutcome.
func (s *Server) recordNoteDelivery(ctx context.Context, cfg webhooks.Config, status, payloadHash, srcIP string, p gitlab.ParsedNote, errMsg string) {
	s.markWebhookOutcome(cfg.Provider, status)
	if s.webhookDeliveries == nil {
		return
	}
	d := newWebhookDelivery(cfg, gitlabNoteMeta(p), status, payloadHash, srcIP)
	d.IdempotencyKey = uuid.NewString()
	d.Error = errMsg
	_ = s.webhookDeliveries.Insert(ctx, d)
}

// maxThreadContextChars caps the discussion transcript injected as the
// converse bot's {{vars.thread_context}} (~4k tokens). Revi's review
// summary (the typical thread anchor) fits comfortably; pathological
// threads keep the anchor + the most recent notes (see
// gitlab.FormatThreadTranscript).
const maxThreadContextChars = 16000

// realWebhookNoteGate is the production replier gate: resolve the
// bot's forge token, reject the bot's own notes (loop-guard), then
// authorize the replier via allowlist OR GitLab role-gate. Returns
// ok=false with a human filter reason for the benign refusals; err
// only for an authz infrastructure failure (502 upstream).
//
// As a by-product it returns threadContext — the note's discussion
// transcript — fetched ONCE and used both to classify a plain reply
// ("is this a Revi thread?") and to ground the converse bot's answer
// in what was said before (typically Revi's own review comment).
func (s *Server) realWebhookNoteGate(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, botID string) (authorized, replyInThread bool, threadContext, reason string, err error) {
	// Resolve the bot's forge token (honouring per-webhook secret overrides):
	// it authenticates the auth checks AND is the identity the bot posts as.
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil || token == "" {
		return false, false, "", "no forge token resolved (configure a forge_token binding)", nil
	}
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if refusal != "" {
		return false, false, "", refusal, nil
	}
	api := gitlab.API{HTTP: s.forgeHTTPClient(), BaseURL: baseURL, Token: token}
	bot, berr := api.CurrentUser(ctx)
	// Loop-guard: never act on the bot's own notes (else its reply re-fires).
	if berr == nil && bot.ID == p.AuthorID {
		return false, false, "", "self note (loop-guard)", nil
	}
	// Fetch the discussion when its content can be used: always for a
	// non-command note (load-bearing — it classifies the reply), and for
	// `/revi <question>` when the converse bot can be routed to (additive —
	// the transcript grounds the answer). Bare `/revi` (re-review) never
	// needs it.
	cmd, cmdArgs := p.Command()
	var notes []gitlab.DiscussionNote
	if berr == nil && p.DiscussionID != "" && (cmd != "revi" || (cmdArgs != "" && s.canRouteToConverseBot(cfg))) {
		var derr error
		notes, derr = api.Discussion(ctx, p.ProjectID, p.MRIID, p.DiscussionID)
		if derr != nil {
			if cmd != "revi" {
				// Classification needs the thread — fail the request (502)
				// so the forge retries, exactly as before.
				return false, false, "", "", derr
			}
			// Context for an explicit /revi command is best-effort: answer
			// without the transcript rather than dropping the question.
			if s.logger != nil {
				s.logger.Warn("webhooks: gitlab discussion %s fetch failed (continuing without thread context): %v", p.DiscussionID, derr)
			}
		}
	}
	// A note with no /revi command is a trigger ONLY when it is a reply in a
	// thread Revi is part of (so "just replying" to Revi's comment works).
	// We need a confirmed bot identity to classify the thread; without it we
	// can't tell a Revi thread from any other, so we don't trigger.
	if cmd != "revi" {
		if berr != nil {
			return false, false, "", "bot identity unresolved; cannot classify reply", nil
		}
		if !gitlab.NotesHaveAuthor(notes, bot.ID) {
			return false, false, "", "not a /revi command or a reply in a Revi thread", nil
		}
		replyInThread = true
	}
	threadContext = gitlab.FormatThreadTranscript(notes, bot.ID, maxThreadContextChars)
	// Trigger confirmed (/revi command or reply-in-thread) → authorize the
	// replier: allowlist OR role-gate.
	ok, reason, aerr := gitlab.AuthorizeReplier(ctx, api, gitlab.ReplierAuth{
		AuthorID: p.AuthorID, AuthorUsername: p.AuthorUsername, ProjectID: p.ProjectID,
		Allowlist: cfg.AuthorizedRepliers, MinRole: cfg.MinReplierRole,
	})
	if aerr != nil {
		return false, replyInThread, "", "", aerr
	}
	if !ok {
		return false, replyInThread, "", "replier not authorized: " + p.AuthorUsername, nil
	}
	return true, replyInThread, threadContext, reason, nil
}

// resolveForgeToken resolves the bot's forge_token for the webhook's tenant,
// honouring its per-webhook secret override. Used at handler time for the
// auth checks (the per-run sealed bundle isn't available pre-launch).
func (s *Server) resolveForgeToken(ctx context.Context, cfg webhooks.Config, botID string) (string, error) {
	if s.genericSecrets == nil || s.sealer == nil {
		return "", nil
	}
	ctx = store.WithTenant(ctx, cfg.TenantID)
	res, err := secrets.ResolveGenericWithBindings(ctx, s.genericSecrets, s.botBindings, cfg.TenantID, "", botID, []string{"forge_token"}, cfg.SecretOverrides, s.sealer, s.logger)
	if err != nil {
		return "", err
	}
	if r, ok := res["forge_token"]; ok {
		return string(r.Plaintext), nil
	}
	return "", nil
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		// Reject unparseable URLs and any URL carrying userinfo
		// (https://user:pass@host) — the bot's forge_token must never be
		// sent to a credential-confusing host derived from the payload.
		return ""
	}
	return u.Host
}

// forgeHostAllowed gates which forge host the bot's forge_token may be sent
// to. The host is derived from the (secret-authenticated) webhook payload's
// MR URL so iterion can call back arbitrary self-hosted GitLab instances; an
// operator running against a known, fixed set of instances can pin them via
// ITERION_WEBHOOK_FORGE_HOSTS (comma-separated host[:port]) so a hostile
// payload cannot exfiltrate the token elsewhere. Empty env = no restriction
// (any well-formed host), preserving prior behaviour.
func forgeHostAllowed(host string) bool {
	raw := strings.TrimSpace(os.Getenv("ITERION_WEBHOOK_FORGE_HOSTS"))
	if raw == "" {
		return true
	}
	for _, h := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// resolveForgeBaseURL decides the forge base URL the bot's forge_token may be
// sent to for a delivery, returning a non-empty refusal when it must not be
// sent at all. Precedence:
//   - cfg.ForgeBaseURL set (per-webhook pin): the payload MR-URL host MUST
//     match the configured host, else refuse — the precise per-tenant control
//     for multi-instance cloud deployments.
//   - otherwise: derive the host from the (secret-authenticated) payload,
//     gated by the optional global ITERION_WEBHOOK_FORGE_HOSTS allowlist.
func resolveForgeBaseURL(cfg webhooks.Config, mrURL string) (baseURL, refusal string) {
	payloadHost := hostFromURL(mrURL)
	if payloadHost == "" {
		return "", "merge_request URL has no usable forge host"
	}
	if cfg.ForgeBaseURL != "" {
		want := hostFromURL(cfg.ForgeBaseURL)
		if want == "" {
			return "", "webhook forge_base_url is malformed"
		}
		if !strings.EqualFold(want, payloadHost) {
			return "", fmt.Sprintf("merge_request host %q does not match the webhook's pinned forge %q", payloadHost, want)
		}
		return "https://" + want, ""
	}
	if !forgeHostAllowed(payloadHost) {
		return "", "forge host not in ITERION_WEBHOOK_FORGE_HOSTS allowlist"
	}
	return "https://" + payloadHost, ""
}

// gitlabMRMeta flattens a Parsed merge-request into the generic
// webhookEventMeta the shared helpers consume.
func gitlabMRMeta(p gitlab.Parsed) webhookEventMeta {
	subject := ""
	if p.MRIID != 0 {
		subject = p.SubjectID()
	}
	return webhookEventMeta{
		Kind:         "merge_request",
		Action:       p.Action,
		ProjectPath:  p.ProjectPath,
		SubjectID:    subject,
		SubjectSHA:   p.HeadSHA,
		SenderHandle: p.SenderUsername,
	}
}

// gitlabNoteMeta flattens a ParsedNote. The audit row's EventKind is
// "note" — different from "merge_request" — so a per-tenant analytics
// query can split "auto-review on open" vs "operator-triggered /revi".
func gitlabNoteMeta(p gitlab.ParsedNote) webhookEventMeta {
	// SubjectURL is the comment's subject — the MR for an MR note, the issue for
	// an issue note. It is the back-link target the ensureBoardCard open_mr stamp
	// writes into source_issue_ref, so a comment-triggered MR posts back onto the
	// very issue the human commented on.
	subjectURL := p.MRURL
	if p.IsIssueNote() {
		subjectURL = p.IssueURL
	}
	return webhookEventMeta{
		Kind:         "note",
		Action:       "comment",
		ProjectPath:  p.ProjectPath,
		SubjectID:    p.SubjectID(),
		SubjectSHA:   p.HeadSHA,
		SenderHandle: p.AuthorUsername,
		SubjectURL:   subjectURL,
	}
}

// updateWebhookDelivery is the best-effort delivery row update used by
// insertAndLaunchWebhook; an audit-store error must NOT poison the
// inbound request.
func (s *Server) updateWebhookDelivery(ctx context.Context, d webhooks.Delivery) {
	if s.webhookDeliveries == nil {
		return
	}
	_ = s.webhookDeliveries.Update(ctx, d)
}

// resolveBotSource loads a bot's workflow source by bot id.
func (s *Server) resolveBotSource(botID string) (path, source string, err error) {
	path, err = botregistry.ResolveBotPath(botID, s.effectivePaths())
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read bot %q: %w", botID, err)
	}
	return path, string(b), nil
}

// launchScheduledBot is the cloudsched.LaunchFunc: it launches a recurring bot
// run for its tenant through the run service (cloud → publisher). The tenant
// identity is stamped on the ctx so the publisher seals credentials + scopes
// the run to the org. When the schedule pins a RepoURL, it is threaded onto
// the LaunchSpec so the runner clones the repo before the bot starts —
// mandatory for stateful bots that persist state to git (feed-watch
// state_commit=true), which need a workspace with push credentials wired.
// Generic secrets declared by the bot (e.g. `webhooks`) resolve via the
// publisher's bot-secret bindings, so SecretOverrides plumbing is not needed
// here.
func (s *Server) launchScheduledBot(ctx context.Context, sb cloudsched.ScheduledBot) error {
	if s.runs == nil {
		return errors.New("run service unavailable")
	}
	ctx = store.WithIdentity(ctx, sb.TenantID, "scheduler:"+sb.BotID)
	path, source, err := s.resolveBotSource(sb.BotID)
	if err != nil {
		return err
	}
	retry := s.resolveRunRetryPolicy(sb.BotID, retrypolicy.Layer{
		Source: retrypolicy.SourceSchedule,
		Policy: sb.RetryPolicy(),
	})
	_, err = s.runs.Launch(ctx, buildScheduledLaunchSpec(sb, path, source, retry))
	return err
}

// buildScheduledLaunchSpec is the pure-data half of launchScheduledBot,
// exposed for unit testing without wiring a full runview.Service. It carries
// the schedule's Vars + repo binding onto the LaunchSpec so the runner clones
// the pinned repo (mandatory for stateful bots persisting state to git) and
// stamps the BotID for the publisher's credential-resolution path.
func buildScheduledLaunchSpec(sb cloudsched.ScheduledBot, path, source string, retry *store.RunRetryPolicy) runview.LaunchSpec {
	return runview.LaunchSpec{
		FilePath: path,
		Source:   source,
		BotID:    sb.BotID,
		Vars:     sb.Vars,
		RepoURL:  sb.RepoURL,
		RepoRef:  sb.RepoRef,
		// Resolved by the caller across the schedule row, the bot manifest
		// and the machine default — the schedule is the layer an operator
		// reaches for when one bot's cadence needs different retry limits
		// from the same bot on another cadence.
		RetryPolicy: retry,
		// Typed provenance: the schedgate overlap gate counts this
		// schedule's live runs through source.schedule_id.
		SourceRef: &store.RunSource{
			Kind:         store.RunSourceKindSchedule,
			ScheduleID:   sb.ID,
			ScheduleName: sb.BotID,
		},
	}
}

// webhookLauncherFor builds the production launch path for one inbound
// webhook config. It is a closure rather than a plain method because the
// launch needs the config's retry policy, which the seam's positional
// signature does not carry.
func (s *Server) webhookLauncherFor(cfg webhooks.Config) func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
	return func(ctx context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, keyOverrides, secretOverrides map[string]string) (string, error) {
		return s.launchWebhookBot(ctx, cfg, botID, vars, repoURL, repoRef, projectPath, keyOverrides, secretOverrides)
	}
}

// launchWebhookBot resolves the bot's source and submits it through the run
// service (which, in cloud mode, routes to the publisher).
func (s *Server) launchWebhookBot(ctx context.Context, cfg webhooks.Config, botID string, vars map[string]string, repoURL, repoRef, projectPath string, keyOverrides, secretOverrides map[string]string) (string, error) {
	if s.runs == nil {
		return "", errors.New("run service unavailable")
	}
	path, source, err := s.resolveBotSource(botID)
	if err != nil {
		return "", err
	}
	res, err := s.runs.Launch(ctx, runview.LaunchSpec{
		FilePath:        path,
		Source:          source,
		Vars:            vars,
		RepoURL:         repoURL,
		RepoRef:         repoRef,
		ProjectPath:     projectPath,
		BotID:           botID,
		KeyOverrides:    keyOverrides,
		SecretOverrides: secretOverrides,
		// A webhook-launched run is often the one an author is waiting on,
		// so the config's own horizon usually wants to be shorter than a
		// nightly's — hence the layer.
		RetryPolicy: s.resolveRunRetryPolicy(botID, retrypolicy.Layer{
			Source: retrypolicy.SourceWebhook,
			Policy: cfg.RetryPolicy(),
		}),
	})
	if err != nil {
		return "", err
	}
	return res.RunID, nil
}
