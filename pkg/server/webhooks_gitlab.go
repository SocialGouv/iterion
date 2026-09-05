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
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
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
// on-demand, via the `/revi` Note hook below or the forge-native
// "Re-request review" button on iterion's bot reviewer (reviewRequested).
func (s *Server) handleGitLabMergeRequestEvent(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	p, err := gitlab.ParseMergeRequest(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "merge_request"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid merge_request payload")
		return
	}
	meta := gitlabMRMeta(p)

	// A CLOSED or MERGED merge request ends every review it still owes —
	// the GitLab twin of the prforge closed lane: live runs stop, the
	// debounce's parked launch purges (a push at T then a merge at T+1m
	// must NOT fire a full review of a dead MR at T+3m), armed
	// usage-window retries disarm. Before every other filter for the same
	// reason as over there: stopping is not launching.
	if p.IsClosed() {
		stopped := s.stopRunsForDeadPR(ctx, cfg, meta)
		reason := "merge request closed — no runs to stop"
		if stopped > 0 {
			reason = fmt.Sprintf("merge request closed — stopped %d run(s) still bound to it", stopped)
		}
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Filter: only review on open/reopen, allowed event + project.
	// A filtered delivery returns 200 (a 4xx would make GitLab disable
	// the webhook after repeated metadata-only edits).
	// ReviewOnSync opts a push-to-MR ("update" with a new head) back into
	// review so the merge-gate status re-evaluates on the new head SHA —
	// but never on a closed/merged/locked MR (pushes to a dead MR's source
	// branch still deliver the update hook). Fail-open on a payload without
	// `state`: filtering it would strand the required check instead.
	gateResync := cfg.ReviewOnSync && p.IsSynchronize() && p.StateOpenOrUnknown()
	// A review request explicitly targeting iterion's own bot account — the
	// GitLab "Re-request review" sidebar button, or adding the bot to the
	// reviewer set — is the button form of `/revi`: a deliberate on-demand
	// re-review, PROVISIONAL until the replier gate below confirms the
	// clicker (GitLab lets an MR author edit reviewers without a project
	// role — "the forge gates it" is not an authorization story). Only on
	// an OPEN MR (reviewer edits arrive freely on closed/merged ones —
	// mirroring the Note lane's closed-MR filter). Never when the actor IS
	// the bot: the publish tail self-assigns the bot as reviewer after each
	// review, and that PUT echoes straight back to this handler.
	reviewRequested := p.Action == "update" &&
		strings.EqualFold(p.State, "opened") &&
		s.isIterionBotReviewRequest(ctx, cfg, p.ReviewRequestedFrom) &&
		!s.isIterionForgeBotAuthor(ctx, cfg, p.SenderUsername)
	reviewable := p.IsReviewable() || gateResync || reviewRequested
	if !reviewable ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "merge_request", "merge_request", "note") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
		!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderUsername) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Same per-bot fan-out as the GitHub PR lane — resolved BEFORE the
	// hold-label/bot guards so the replier gate below can name the bot whose
	// forge token authenticates the authz lookup.
	rules := s.resolveForgeEventBots(cfg, bundle.ForgeEventPullRequest, p.SenderUsername)
	if len(rules) == 0 {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "no enabled bot claims this MR (event/author routing)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Replier authorization on the re-request lane (R7e050f): every other
	// MANUAL trigger on this webhook (`/revi`, `/command`, reply-in-thread,
	// `/revi approve`) goes through the AuthorizedRepliers/MinReplierRole
	// gate — the button must too. "The forge gates who may edit reviewers"
	// is not sufficient: GitLab lets an MR AUTHOR edit their own MR's
	// reviewers without holding a project role, which would hand a fork
	// contributor a repeatable trigger. An unauthorized click simply
	// DEMOTES reviewRequested — the delivery then rides whatever lane
	// still admits it (resync) or is filtered. (The hold gate below runs
	// unconditionally either way: a re-request has no exemption.)
	if reviewRequested {
		gate := s.webhookReviewRequestGate
		if gate == nil {
			gate = s.realWebhookReviewRequestGate
		}
		authorized, reason, gerr := gate(ctx, cfg, p, rules[0].BotID)
		if gerr != nil {
			// R34eb8c: an authz ERROR must not strand an automatic lane
			// co-riding the same event (one update can be BOTH a push and a
			// reviewers change) — demote the gesture and let the resync
			// proceed. 502 only when the click was the delivery's sole
			// reason, so the forge redelivers it.
			if p.IsReviewable() || gateResync {
				if s.logger != nil {
					s.logger.Warn("webhooks: gitlab re-request authz errored (%v) — gesture demoted, the automatic lane proceeds", gerr)
				}
				reviewRequested = false
			} else {
				s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "re-request authz check: "+gerr.Error())
				httpError(w, http.StatusBadGateway, "authorization check failed")
				return
			}
		} else if !authorized {
			reviewRequested = false
			if !p.IsReviewable() && !gateResync {
				s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
				writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
				return
			}
		}
	}

	// Hold-label gate (bot-agnostic, opt-in): a configured hold label on the MR
	// vetoes the auto-review. Same escape hatch as the GitHub PR path.
	// A re-request is NOT exempt: the label freezes every automation on one
	// MR, and that promise is what makes it usable at all (see the GitHub
	// lane for the auto-request case the exemption could not tell apart).
	if s.suppressedByHoldLabel(ctx, w, cfg, meta, p.Labels, payloadHash, srcIP) {
		return
	}

	// Iterion-bot guard: an MR opened by iterion's own forge bot already
	// converged in its own loop — skip the auto-review (a human can still run
	// `/revi`). Mirror of the GitHub PR-open path, including its merge-gate
	// resync exception: a fixer's push is by construction sent by our own bot,
	// and the re-review it triggers is what re-posts the required check and
	// supersedes the fixer's self-verdict. (A reviewRequested delivery never
	// reaches this guard's condition with a bot sender — its own actor guard
	// already excluded that — and a human's re-request on a bot-authored MR
	// is deliberate, so it reviews.)
	if !gateResync && !reviewRequested && s.isIterionForgeBotAuthor(ctx, cfg, p.SenderUsername) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"MR authored by iterion's forge bot — auto-review skipped (self-produced; run /revi to force a review)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// (Routing note: the fan-out above resolves on the event actor rather
	// than the MR's author, unlike GitHub — GitLab's merge_request payload
	// carries only `author_id`, so resolving the author's username would
	// need an extra API call on the hot path.)

	// Idempotency: one launch per (tenant, webhook, project, MR, head sha) —
	// per bot once the delivery fans out. The "mr|" prefix keeps the key space
	// disjoint from the Note hook ("note|") so a /revi on the same MR can't
	// collide with the open.
	idemBase := fmt.Sprintf("mr|%s|%s|%d|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.MRIID, p.HeadSHA)
	extra := map[string]string{"pr_author": p.SenderUsername, "source_branch": p.SourceBranch, "head_sha": p.HeadSHA}
	if reviewRequested && !gateResync {
		// A deliberate re-request must relaunch even on a head the auto-review
		// already claimed — and again on a second click. The MR's updated_at
		// salts the key so each click is its own delivery, and the "rereq|"
		// prefix keeps it disjoint from the open/resync space. re_review marks
		// the posted summary like the `/revi` note path does. NOT when the
		// same event is also a gate resync (a push carrying a reviewers diff):
		// the resync already reviews this head under the per-head key, and
		// swapping key spaces would let the following pure resync review it a
		// second time.
		idemBase = fmt.Sprintf("rereq|%s|%s|%d|%d|%s|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.MRIID, p.HeadSHA, p.UpdatedAt)
		extra["re_review"] = "true"
	}

	targets := forgePREventTargets(cfg, rules, idemBase, p.MRURL, p.TargetBranch,
		strings.TrimSpace(p.Title+"\n\n"+p.Description), p.CloneURL, p.SourceBranch, extra)

	// Push debounce: a synchronize launch waits out a quiet window so a
	// volley of pushes costs one review of the final head (a re-request
	// click stays immediate — a human is waiting on it).
	if s.shouldDeferSyncLaunch(gateResync && !reviewRequested) {
		s.deferSyncLaunch(ctx, w, r, cfg, meta, targets, payloadHash, srcIP)
		return
	}
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
	cmd, cmdArgs := p.Command()
	// `/revi approve [reason]` is an OVERRIDE, not a re-review: a trusted
	// maintainer force-greens the merge gate. Intercept BEFORE the
	// command→bot routing so it never launches a run — the GitLab twin of
	// the GitHub/Forgejo lane (docs/merge-gate.md).
	if reason, isApprove := reviewApproveReason(cmd, cmdArgs); isApprove {
		s.handleGitLabReviewApprove(ctx, w, cfg, p, reason, payloadHash, srcIP)
		return
	}
	// Generic slash-command routing — EVERY command, `/revi` included: the
	// review-pr (when_args_empty) / revi-converse (when_args_present)
	// manifests declare the disambiguated pair, so ResolveCommandRoute picks
	// between them exactly like it would for any other two-bot command —
	// no bot-specific branch here. Only a PLAIN REPLY (no command at all)
	// stays bespoke below: it carries no command for the registry to
	// resolve, so "is this a reply in a Revi thread" has to be classified
	// some other way.
	if cmd != "" {
		s.handleGitLabCommandNote(ctx, w, r, cfg, p, cmd, cmdArgs, "pr", payloadHash, srcIP)
		return
	}
	// Reply-in-thread trigger: a plain reply (no /revi command) in a thread
	// Revi is part of routes to the converse bot with the reply body as the
	// question — "just replying" to Revi's comment works, no command needed.
	// Classifying "is this a Revi thread" needs the bot's own identity, so it
	// runs inside the gate (which resolves the forge token).
	converseBot := s.roleBots().ReviConverse
	if !s.canRouteToConverseBot(cfg, converseBot) {
		filtered("no /revi trigger")
		return
	}
	gate := s.webhookNoteGate
	if gate == nil {
		gate = s.realWebhookNoteGate
	}
	authorized, _, threadContext, reason, aerr := gate(ctx, cfg, p, converseBot)
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
		s.logger.Debug("webhooks: gitlab note %s!%d (reply) by %s authorized (%s)", p.ProjectPath, p.MRIID, p.AuthorUsername, reason)
	}
	question := strings.TrimSpace(p.NoteBody)
	vars := applyWebhookVarLayers(reviewPRVars(p.MRURL, p.TargetBranch, strings.TrimSpace(p.MRTitle+"\n\n"+p.MRDesc), nil, map[string]string{
		"conversation_mode": "reply",
		"discussion_id":     p.DiscussionID,
		"trigger_note":      p.NoteBody,
		"replier":           p.AuthorUsername,
		"converse_question": question,
	}), cfg)
	// The discussion transcript the gate fetched — the operator's reply
	// typically references Revi's earlier review note; the bot grounds its
	// answer in it.
	if threadContext != "" {
		vars["thread_context"] = threadContext
	}

	// Idempotency: one launch per note.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectID, p.SubjectID())))

	s.insertAndLaunchWebhook(ctx, w, r, cfg, gitlabNoteMeta(p), idemKey, converseBot, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
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
	outcome, reason, aerr := gate(ctx, cfg, p, route)
	if aerr != nil {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if outcome != gateAuthorized {
		filtered(reason)
		return
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: gitlab note %s/%s (/%s) by %s → %s (%s)", p.ProjectPath, surface, cmd, p.AuthorUsername, route.BotID, reason)
	}
	var vars map[string]string
	if surface == "issue" {
		vars = applyWebhookVarLayers(buildGitLabIssueCommandVars(p, route, cmdArgs, nil), cfg)
	} else {
		// Fork guard, fail-CLOSED — the same class of gap #683 closed on the
		// GitHub/Forgejo command lane: the launch pair (this project's
		// CloneURL + p.SourceBranch) does NOT name one repository when the
		// MR's head lives elsewhere, so the checkout would miss (or, worse,
		// hit a same-named branch here and the bot answers grounded in the
		// wrong code, under the bot's identity). The note payload carries
		// neither source_project_id nor target_project_id, so proving
		// same-project needs the API (pkg/forge/gitlab/ci.go toRef names
		// HeadRepoFullName only when they agree).
		resolve := s.webhookGitLabPRResolver
		if resolve == nil {
			resolve = s.realWebhookGitLabPRResolver
		}
		resolved, rerr := resolve(ctx, cfg, p, route.BotID)
		if rerr != nil {
			s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, "MR resolution: "+rerr.Error())
			httpError(w, http.StatusBadGateway, "could not resolve the MR head project")
			return
		}
		if !resolved.SameRepoAs(p.ProjectPath) {
			filtered("fork MR or unverifiable head project — /" + cmd + " runs are same-project only")
			return
		}
		vars = applyWebhookVarLayers(buildCommandVars(p, route, cmdArgs, nil), cfg)
		stampBranchImprovePushBack(vars, route.BotID, s.roleBots().Brancher, p.SourceBranch, cfg.BranchImproveAsPR)
		// The revision the command is about, so the shared launch tail can tell a
		// consumer whether the review it is handed still matches the MR head.
		vars["head_sha"] = p.HeadSHA
		// Conversational plumbing every pr-surface command carries, GitLab's
		// counterpart to a GitHub issue_comment's pr_url/base_ref/scope_notes
		// above: unlike GitHub, every GitLab note sits in a discussion, so
		// discussion_id (needed to post an answer AS A REPLY in the same
		// thread) and the note's own trigger metadata cost nothing to carry
		// generically — no bot-specific branch needed to decide who gets it.
		vars["conversation_mode"] = "reply"
		vars["discussion_id"] = p.DiscussionID
		vars["trigger_note"] = p.NoteBody
		vars["trigger_command"] = cmd
		vars["trigger_args"] = cmdArgs
		vars["replier"] = p.AuthorUsername
		// re_review marks the posted summary the way review-pr's bare
		// `/revi` always has: a when_args_present sibling sharing the same
		// command (revi-converse) is answering a QUESTION, not re-reviewing,
		// so it does not get the flag. Keyed on the route's own declared
		// shape, not a bot id — any future when_args_empty/when_args_present
		// pair inherits the same convention for free.
		if route.Disambiguator == "when_args_empty" {
			vars["re_review"] = "true"
		}
		// thread_context is best-effort and gate-fetched: one API call,
		// worth paying only when the command carries text to ground (a bare
		// command has nothing to ground). A fetch failure degrades to an
		// absent var, never a refusal.
		if strings.TrimSpace(cmdArgs) != "" {
			if tc := s.gitlabNoteThreadContext(ctx, cfg, p, route.BotID); tc != "" {
				vars["thread_context"] = tc
			}
		}
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

// gitlabNoteAPI is the minimal GitLab surface the note gates need: the
// bot's own identity (loop-guard + reply-in-thread classification), a
// discussion's notes (classification + transcript), and a replier's project
// access level (role authorization). gitlab.API satisfies it structurally;
// tests supply a fake to drive gitlabCommandGateWithAPI / gitlabNoteGateWithAPI
// without a live GitLab (the token-free split the GitHub twin's
// reviewReplyGateWithAPI already has).
type gitlabNoteAPI interface {
	CurrentUser(ctx context.Context) (gitlab.User, error)
	Discussion(ctx context.Context, projectID, mrIID int64, discussionID string) ([]gitlab.DiscussionNote, error)
	MemberAccessLevel(ctx context.Context, projectID, userID int64) (level int, member bool, err error)
}

// gitlabAuthorizeReplier decides whether a note's author may trigger the
// bot — allowlist OR a project role >= minRole — re-sequenced over the
// gitlabNoteAPI interface so it is fakeable in tests. gitlab.AuthorizeReplier
// makes the SAME decision but takes the concrete API struct (always a real
// HTTP call); this is not a fork of that logic, just its two already-pure
// building blocks (gitlab.InAllowlist, gitlab.RoleLevel) re-sequenced around
// the one impure call through an interface.
func gitlabAuthorizeReplier(ctx context.Context, api gitlabNoteAPI, in gitlab.ReplierAuth) (ok bool, reason string, err error) {
	if gitlab.InAllowlist(in.Allowlist, in.AuthorUsername, in.AuthorID) {
		return true, "allowlist", nil
	}
	level, member, err := api.MemberAccessLevel(ctx, in.ProjectID, in.AuthorID)
	if err != nil {
		return false, "", err
	}
	if member && level >= gitlab.RoleLevel(in.MinRole) {
		return true, "role", nil
	}
	return false, "", nil
}

// realWebhookCommandGate is the production replier gate for a generic
// slash-command: resolve the bot's forge token, then hand the loop-guard +
// authorization to gitlabCommandGateWithAPI. The outcome is the SAME
// three-state prforgeGateOutcome as the GitHub/Forgejo command gate
// (gateUnevaluable for a configuration gap the caller may want to explain,
// gateRefused for an evaluated refusal that stays silent) — /revi approve
// needs that distinction; every other command just checks != gateAuthorized.
func (s *Server) realWebhookCommandGate(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, route webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, route.BotID)
	if terr != nil || token == "" {
		return gateUnevaluable, "no forge token resolved (configure a forge_token binding)", nil
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
		return gateUnevaluable, refusal, nil
	}
	api := gitlab.API{HTTP: s.forgeHTTPClient(), BaseURL: baseURL, Token: token}
	return s.gitlabCommandGateWithAPI(ctx, cfg, p, route, api)
}

// gitlabCommandGateWithAPI is the token-free core of the command gate —
// split from the wrapper so tests drive it with a fake GitLab API. Self-note
// loop-guard, then allowlist/role authorization honouring the route's
// MinReplierRole (falling back to the webhook default).
func (s *Server) gitlabCommandGateWithAPI(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, route webhooks.CommandRoute, api gitlabNoteAPI) (prforgeGateOutcome, string, error) {
	if bot, berr := api.CurrentUser(ctx); berr == nil && bot.ID == p.AuthorID {
		return gateRefused, "self note (loop-guard)", nil
	}
	minRole := route.MinReplierRole
	if minRole == "" {
		minRole = cfg.MinReplierRole
	}
	ok, reason, aerr := gitlabAuthorizeReplier(ctx, api, gitlab.ReplierAuth{
		AuthorID: p.AuthorID, AuthorUsername: p.AuthorUsername, ProjectID: p.ProjectID,
		Allowlist: cfg.AuthorizedRepliers, MinRole: minRole,
	})
	if aerr != nil {
		return gateUnevaluable, "", aerr
	}
	if !ok {
		return gateRefused, "replier not authorized: " + p.AuthorUsername, nil
	}
	return gateAuthorized, reason, nil
}

// realWebhookGitLabPRResolver fetches the merge request a command note sits
// on, purely to prove the SAME-PROJECT fork guard: forge.PullRef.HeadRepoFullName
// is only named when source_project_id == target_project_id (see
// pkg/forge/gitlab/ci.go toRef) — the note payload carries neither id, so
// proving same-project needs the API. Resolution order mirrors
// prforgeReplierAPIFor (the GitHub/Forgejo twin): the team connection
// covering the MR's host+repo first, the webhook's own forge_token binding
// otherwise.
func (s *Server) realWebhookGitLabPRResolver(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, botID string) (forge.PullRef, error) {
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if refusal != "" {
		return forge.PullRef{}, fmt.Errorf("forge base URL: %s", refusal)
	}
	gc, apiRefusal := s.gitlabGateClientFor(ctx, cfg, baseURL, p.ProjectPath, botID)
	if apiRefusal != "" {
		return forge.PullRef{}, fmt.Errorf("%s", apiRefusal)
	}
	return gc.GetPullRequest(ctx, p.ProjectPath, int(p.MRIID))
}

// gitlabGateClientFor resolves the client every GitLab lane that reads or
// writes a merge-gate status through the forge uses (the command lane's
// fork guard, and /revi approve's status write): the team connection
// covering host+repo first — via the SAME s.gateClientFor seam the
// publish path and the gate reconciler use, so an App-shaped connection
// mints its own token AND a test can inject a fake with
// s.forgeGateClientFor — falling back to the webhook's own forge_token
// binding for a hand-owned webhook with no connection row, or when the
// covering connection's client cannot serve. Mirrors prforgeReplierAPIFor
// (the GitHub/Forgejo twin) so every provider lane resolves its write
// credential the same way. need, when non-empty, is preflighted on a
// connection-resolved client (an App client mints its token lazily, so a
// successful build can still be unable to serve a permission the
// installation withholds); the plain fork-guard read never needs it.
func (s *Server) gitlabGateClientFor(ctx context.Context, cfg webhooks.Config, baseURL, projectPath, botID string, need ...string) (forgeGateClient, string) {
	host := hostOfURL(baseURL)
	connRefusal := ""
	if conn, ok := s.forgeConnectionForPR(ctx, cfg.TenantID, "", host, projectPath); ok {
		gc, err := s.gateClientFor(ctx, conn)
		if err == nil && gc != nil && len(need) > 0 {
			err = preflightForgeClient(ctx, gc, need...)
		}
		switch {
		case err != nil:
			connRefusal = "connection " + conn.ID + " covers " + host + "/" + projectPath + " but its client cannot serve (" + err.Error() + ")"
			if s.logger != nil {
				s.logger.Warn("webhooks: %s — reading through the forge_token binding instead", connRefusal)
			}
		case gc == nil:
			return nil, "provider " + string(conn.Provider) + " has no commit-status capability"
		default:
			return gc, ""
		}
	}
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil {
		return nil, "forge token resolution failed: " + terr.Error()
	}
	if token == "" {
		if connRefusal != "" {
			return nil, connRefusal + ", and this webhook has no forge_token binding to fall back on"
		}
		return nil, "no forge token resolved and no team connection covers " + host + "/" + projectPath + " (bind forge_token on the webhook, or connect a forge integration for this repo)"
	}
	return forgegitlab.New(s.forgeHTTPClient(), baseURL, token), ""
}

// gitlabNoteThreadContext best-effort fetches the transcript of the
// discussion a command note sits in, so any pr-surface command's bot can
// ground itself in what was said before (typically Revi's own review
// comment) — generic context every such command carries the SAME way, not
// a Revi-converse specific one. It costs one API call (plus resolving the
// bot's own identity, needed to label its own notes in the transcript), so
// it is only worth attempting for a command that carries text to ground; a
// bare command has nothing to ground and the caller skips this entirely.
// A fetch failure degrades to an absent var, never a refusal — grounding is
// additive, the command already cleared authorization without it.
func (s *Server) gitlabNoteThreadContext(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, botID string) string {
	if p.DiscussionID == "" {
		return ""
	}
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil || token == "" {
		return ""
	}
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if refusal != "" {
		return ""
	}
	api := gitlab.API{HTTP: s.forgeHTTPClient(), BaseURL: baseURL, Token: token}
	var botUserID int64
	if bot, berr := api.CurrentUser(ctx); berr == nil {
		botUserID = bot.ID
	}
	notes, derr := api.Discussion(ctx, p.ProjectID, p.MRIID, p.DiscussionID)
	if derr != nil {
		if s.logger != nil {
			s.logger.Warn("webhooks: gitlab discussion %s fetch failed (continuing without thread context): %v", p.DiscussionID, derr)
		}
		return ""
	}
	return gitlab.FormatThreadTranscript(notes, botUserID, maxThreadContextChars)
}

// realWebhookReviewRequestGate is the production replier gate of the
// re-request-review lane: the SAME controls every other manual trigger
// honours — cfg.AuthorizedRepliers OR a project role ≥ cfg.MinReplierRole
// (empty → developer) — applied to the account that clicked the button.
// Resolution failures refuse (fail closed): this gate is the lane's whole
// authorization story.
func (s *Server) realWebhookReviewRequestGate(ctx context.Context, cfg webhooks.Config, p gitlab.Parsed, botID string) (bool, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil || token == "" {
		return false, "re-request refused: no forge token resolved (configure a forge_token binding)", nil
	}
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if refusal != "" {
		return false, refusal, nil
	}
	api := gitlab.API{HTTP: s.forgeHTTPClient(), BaseURL: baseURL, Token: token}
	ok, reason, err := gitlab.AuthorizeReplier(ctx, api, gitlab.ReplierAuth{
		AuthorID: p.SenderID, AuthorUsername: p.SenderUsername, ProjectID: p.ProjectID,
		Allowlist: cfg.AuthorizedRepliers, MinRole: cfg.MinReplierRole,
	})
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "re-request review by unauthorized replier: " + p.SenderUsername, nil
	}
	return true, reason, nil
}

// canRouteToConverseBot reports whether the given conversational bot id
// (the caller's roleBots snapshot — passed in so the gate and the launch
// agree on ONE bot) is allowed by this webhook config and resolvable on
// this deployment. The existence probe is metadata-only (cached platform
// entries + a baked-path stat) — it must never materialize a bundle just
// to answer a boolean on the note hot path.
func (s *Server) canRouteToConverseBot(cfg webhooks.Config, converseBot string) bool {
	return cfg.AllowsBot(converseBot) && s.botExists(converseBot)
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

// realWebhookNoteGate is the production replier gate of the reply-in-thread
// trigger: resolve the bot's forge token, then hand the classification +
// authorization to gitlabNoteGateWithAPI. Only reached for a note with NO
// slash-command (handleGitLabNote routes every command through
// realWebhookCommandGate instead) — replyInThread is therefore always true
// alongside authorized=true; it is still returned so the caller's intent
// ("this is a reply, not a command") stays explicit at the call site.
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
	return s.gitlabNoteGateWithAPI(ctx, cfg, p, api)
}

// gitlabNoteGateWithAPI is the token-free core of the reply-in-thread gate —
// split from the wrapper so tests drive it with a fake GitLab API, mirroring
// the GitHub twin's reviewReplyGateWithAPI split. Self-note loop-guard, then
// "is this a reply in a thread the bot is part of" classification (needs a
// confirmed bot identity — without one we can't tell a Revi thread from any
// other, so an unresolved identity refuses rather than guesses), then
// allowlist/role authorization. Returns the discussion transcript for the
// converse bot's grounding.
func (s *Server) gitlabNoteGateWithAPI(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, api gitlabNoteAPI) (authorized, replyInThread bool, threadContext, reason string, err error) {
	bot, berr := api.CurrentUser(ctx)
	if berr == nil && bot.ID == p.AuthorID {
		return false, false, "", "self note (loop-guard)", nil
	}
	if berr != nil {
		return false, false, "", "bot identity unresolved; cannot classify reply", nil
	}
	var notes []gitlab.DiscussionNote
	if p.DiscussionID != "" {
		notes, err = api.Discussion(ctx, p.ProjectID, p.MRIID, p.DiscussionID)
		if err != nil {
			// Classification needs the thread — fail the request (502) so
			// the forge retries.
			return false, false, "", "", err
		}
	}
	if !gitlab.NotesHaveAuthor(notes, bot.ID) {
		return false, false, "", "not a /revi command or a reply in a Revi thread", nil
	}
	replyInThread = true
	threadContext = gitlab.FormatThreadTranscript(notes, bot.ID, maxThreadContextChars)
	ok, authReason, aerr := gitlabAuthorizeReplier(ctx, api, gitlab.ReplierAuth{
		AuthorID: p.AuthorID, AuthorUsername: p.AuthorUsername, ProjectID: p.ProjectID,
		Allowlist: cfg.AuthorizedRepliers, MinRole: cfg.MinReplierRole,
	})
	if aerr != nil {
		return false, replyInThread, "", "", aerr
	}
	if !ok {
		return false, replyInThread, "", "replier not authorized: " + p.AuthorUsername, nil
	}
	return true, replyInThread, threadContext, authReason, nil
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
		// Preserve the operator's OWN scheme (a self-hosted instance behind
		// plain http, or a test fixture) instead of reconstructing "https://"
		// — mirrors prforgeBaseURL, the GitHub/Forgejo twin, which returns
		// cfg.ForgeBaseURL verbatim for the same reason.
		return cfg.ForgeBaseURL, ""
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
		Kind:           "merge_request",
		Action:         p.Action,
		ProjectPath:    p.ProjectPath,
		SubjectID:      subject,
		SubjectSHA:     p.HeadSHA,
		SenderHandle:   p.SenderUsername,
		EventUpdatedAt: p.UpdatedAt,
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
	lb, err := s.resolveBotSource(ctx, sb.BotID)
	if err != nil {
		return err
	}
	defer lb.Cleanup()
	retry := s.resolveRunRetryPolicy(sb.BotID, retrypolicy.Layer{
		Source: retrypolicy.SourceSchedule,
		Policy: sb.RetryPolicy(),
	})
	spec := buildScheduledLaunchSpec(sb, lb.Path, lb.Source, retry)
	overrides, err := s.scheduledForgeOverrides(ctx, sb)
	if err != nil {
		return err
	}
	spec.SecretOverrides = overrides
	lb.Stamp(&spec)
	_, err = s.runs.Launch(ctx, spec)
	return err
}

// scheduledForgeOverrides resolves a repo-bound schedule's forge connection
// AT THE TICK — the same managed-token mint every studio/API launch performs.
// Without it the clone leans on a hand-set `forge_token` team secret, which
// expires eventually and then kills every tick at clone with "Invalid
// username or token" while manual launches (which mint fresh) keep working —
// exactly the masked failure docs/scheduling.md warns about. Explicit error,
// never a silent fall-through: a schedule that PINNED an integration wants
// its token; letting the tick limp on to a doomed clone would just move the
// failure three minutes later and strip its cause. A schedule with no pinned
// integration keeps the legacy bot-secret-binding resolution (nil overrides).
func (s *Server) scheduledForgeOverrides(ctx context.Context, sb cloudsched.ScheduledBot) (map[string]string, error) {
	if sb.RepoIntegrationID == "" {
		return nil, nil
	}
	if s.forgeIntegrations == nil || s.forgeConnections == nil || s.forgeOrchestrator == nil {
		return nil, fmt.Errorf("schedule %s pins repo integration %s but the forge layer is not wired", sb.ID, sb.RepoIntegrationID)
	}
	integ, err := s.forgeIntegrations.Get(store.WithoutTenantFilter(ctx), sb.RepoIntegrationID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: resolve repo integration %s: %w", sb.ID, sb.RepoIntegrationID, err)
	}
	if integ.TenantID != sb.TenantID {
		return nil, fmt.Errorf("schedule %s: repo integration %s belongs to another tenant", sb.ID, sb.RepoIntegrationID)
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), integ.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: resolve forge connection %s: %w", sb.ID, integ.ConnectionID, err)
	}
	secID, err := s.forgeOrchestrator.EnsureManagedSecret(store.WithTenant(ctx, conn.TenantID), &conn, "scheduler:"+sb.BotID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: forge token for the clone: %w", sb.ID, err)
	}
	return map[string]string{"forge_token": secID}, nil
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
	lb, err := s.resolveBotSource(ctx, botID)
	if err != nil {
		return "", err
	}
	defer lb.Cleanup()
	spec := runview.LaunchSpec{
		Vars:            vars,
		RepoURL:         repoURL,
		RepoRef:         repoRef,
		ProjectPath:     projectPath,
		KeyOverrides:    keyOverrides,
		SecretOverrides: secretOverrides,
		// A webhook-launched run is often the one an author is waiting on,
		// so the config's own horizon usually wants to be shorter than a
		// nightly's — hence the layer.
		RetryPolicy: s.resolveRunRetryPolicy(botID, retrypolicy.Layer{
			Source: retrypolicy.SourceWebhook,
			Policy: cfg.RetryPolicy(),
		}),
	}
	lb.Stamp(&spec)
	res, err := s.runs.Launch(ctx, spec)
	if err != nil {
		return "", err
	}
	return res.RunID, nil
}

// ---------------------------------------------------------------------------
// /revi approve (GitLab) — the merge-gate override lane, parity with the
// GitHub/Forgejo lane in webhooks_review_approve.go (docs/merge-gate.md).
// The shared, provider-agnostic pieces (approveFloor, resolveGateContext,
// approveIdempotencyKey, isSelfApprove, the approveRefusal type,
// forgeConnectionForPR/gateClientFor/preflightForgeClient) are reused
// verbatim; only the payload type (gitlab.ParsedNote) and the write-path
// resolution (gitlabGateClientFor, which GitLab's single AdminClient type
// lets serve both the status write and the reply) differ.
// ---------------------------------------------------------------------------

// gitlabPullCommenter is the capability the /revi approve reply needs:
// posting a note ON THE MERGE REQUEST specifically. Unlike GitHub/Forgejo
// (whose issues endpoint already serves PRs, see forgeIssueCommenter),
// GitLab addresses merge requests as a resource separate from issues — an
// MR and an issue can share the same iid in one project — so
// forgeIssueCommenter.CommentIssue would land on, or 404 against, the wrong
// resource. Satisfied by pkg/forge/gitlab.AdminClient's CommentPullRequest.
type gitlabPullCommenter interface {
	CommentPullRequest(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error)
}

// gitlabPullCommenterFor resolves a connection's GitLab MR-comment
// capability, mirroring issueCommenterFor. The forgeGitlabPullCommenterFor
// field is a test seam; nil uses the real admin client. A connection whose
// provider is not GitLab (or whose client cannot serve) yields (nil, nil) —
// "this connection has no MR-comment capability" is not an error.
func (s *Server) gitlabPullCommenterFor(ctx context.Context, conn forge.Connection) (gitlabPullCommenter, error) {
	if s.forgeGitlabPullCommenterFor != nil {
		return s.forgeGitlabPullCommenterFor(ctx, conn)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, err
	}
	c, ok := admin.(gitlabPullCommenter)
	if !ok {
		return nil, nil
	}
	return c, nil
}

// handleGitLabReviewApprove handles `/revi approve [reason]` on a GitLab MR
// note: the same maintainer force-green of the merge gate as the GitHub/
// Forgejo lane — a trusted maintainer overrides a finding the author
// disputes, backstopping the admin merge-queue bypass. It is NOT a
// re-review: no bot launches.
//
// Response discipline is IDENTICAL to handlePRForgeReviewApprove: a
// configuration refusal answers 200/filtered, a forge failure 200/
// launch_error — never a 5xx, which GitLab answers by disabling the hook
// and every future launch, re-review and override with it (#662). The
// command gate runs before anything this lane could say on the MR: a
// commenter it REFUSES is answered in silence (a reply there is a bot
// comment any drive-by could drive, N times for N comments); past the gate
// the commenter is a maintainer, and every refusal or failure is told on
// the MR in one voice.
func (s *Server) handleGitLabReviewApprove(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, p gitlab.ParsedNote, reason, payloadHash, srcIP string) {
	meta := gitlabNoteMeta(p)
	filtered := func(why string) {
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusFiltered, payloadHash, srcIP, p, why)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	reviewer := s.roleBots().Reviewer

	// Replay check, before any forge I/O — same semantics as the GitHub
	// lane: a prior delivery that already approved or is mid-write (accepted,
	// younger than approveClaimStaleAfter) answers `duplicate`; a prior
	// launch_error or a stale claim is reused below.
	idemKey := approveIdempotencyKey(cfg, p.ProjectPath, p.SubjectID())
	var priorRow *webhooks.Delivery
	if s.webhookDeliveries != nil {
		if existing, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey); err == nil {
			staleClaim := existing.Status == webhooks.StatusAccepted && time.Since(existing.ReceivedAt) > approveClaimStaleAfter
			if existing.Status != webhooks.StatusLaunchError && !staleClaim {
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusDuplicate, "delivery_id": existing.ID})
				return
			}
			if staleClaim && s.logger != nil {
				s.logger.Warn("webhooks: gitlab %s!%d /revi approve: reusing claim %s left accepted %s ago — its writer never recorded an outcome", p.ProjectPath, p.MRIID, existing.ID, time.Since(existing.ReceivedAt).Round(time.Minute))
			}
			ex := existing
			priorRow = &ex
		}
	}

	// Authorize FIRST, through the SAME command gate every other /command
	// uses — not the note gate — with the route's floor raised to
	// approveFloor and the allowlist cleared: the webhook's
	// AuthorizedRepliers is "who may talk back to the bot", not "who may
	// force-green a required check".
	route := webhooks.CommandRoute{BotID: reviewer, MinReplierRole: approveFloor(cfg.MinReplierRole)}
	gateCfg := cfg
	gateCfg.AuthorizedRepliers = nil
	gate := s.webhookCommandGate
	if gate == nil {
		gate = s.realWebhookCommandGate
	}
	outcome, gateReason, aerr := gate(ctx, gateCfg, p, route)
	if aerr != nil {
		why := "authz check: " + aerr.Error()
		s.warnApproveDidNotLand(cfg.Provider, p.ProjectPath, int(p.MRIID), p.AuthorUsername, why)
		s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, why)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": why})
		return
	}
	switch outcome {
	case gateRefused:
		if s.logger != nil {
			s.logger.Info("webhooks: gitlab %s!%d /revi approve by @%s refused (%s) — no reply, the delivery audit records it", p.ProjectPath, p.MRIID, p.AuthorUsername, gateReason)
		}
		filtered(gateReason)
		return
	case gateUnevaluable:
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, forge.Connection{}, false, approveRefusal{
			detail: gateReason,
			reply:  "this webhook cannot reach the forge to check who may approve — connect a forge integration for this repository, or bind forge_token on the webhook; its delivery audit names what was tried",
		}, payloadHash, srcIP)
		return
	}

	// From here on the commenter cleared the maintainer floor: every refusal
	// below is a configuration miss or a forge failure, and is told on the MR.
	if !cfg.AllowsBot(reviewer) {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, forge.Connection{}, false, sameRefusal("the review bot "+reviewer+" is not enabled on this webhook"), payloadHash, srcIP)
		return
	}
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if refusal != "" {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, forge.Connection{}, false, sameRefusal(refusal), payloadHash, srcIP)
		return
	}
	host := hostOfURL(baseURL)
	if host == "" {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, forge.Connection{}, false, sameRefusal("the payload URL has no usable forge host"), payloadHash, srcIP)
		return
	}
	path, pathRefusal, resolveErr := s.resolveGitLabApproveWritePath(ctx, cfg, reviewer, baseURL, host, p.ProjectPath)
	if resolveErr != nil {
		s.gitlabApproveFailWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, approveRefusal{
			detail: "resolve forge client: " + resolveErr.Error(),
			reply:  "the forge connection covering this repository could not be used — see the webhook's delivery audit, then redeliver the webhook or comment again",
		}, payloadHash, srcIP)
		return
	}
	if pathRefusal.detail != "" {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, pathRefusal, payloadHash, srcIP)
		return
	}

	// The head is read through the SAME client the status is written with —
	// one credential resolution, one read.
	gc := path.gc
	pr, err := gc.GetPullRequest(ctx, p.ProjectPath, int(p.MRIID))
	if err != nil {
		s.gitlabApproveFailWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, approveRefusal{
			detail: "resolve MR head: " + err.Error(),
			reply:  "the forge did not return this merge request through the integration's credential — see the webhook's delivery audit, then redeliver the webhook or comment again",
		}, payloadHash, srcIP)
		return
	}
	if isSelfApprove(pr, p.AuthorUsername) {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, sameRefusal("this is your own merge request — a maintainer must run /revi approve here"), payloadHash, srcIP)
		return
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, sameRefusal("the forge returned no head sha for this MR"), payloadHash, srcIP)
		return
	}
	gateCtx := s.resolveGateContext(cfg, reviewer)
	if gateCtx == "" {
		s.gitlabApproveFilteredWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, sameRefusal("no merge-gate context is pinned on this repo (pin gate_context on the integration — see docs/merge-gate.md)"), payloadHash, srcIP)
		return
	}

	// Claim the approve under its stable key BEFORE the forge write: the
	// store's unique constraint on the key is what keeps two replicas
	// handling the same redelivery from both writing the status.
	claim := newWebhookDelivery(cfg, meta, webhooks.StatusAccepted, payloadHash, srcIP)
	claim.IdempotencyKey = idemKey
	claim.BotID = reviewer
	if s.webhookDeliveries != nil {
		if priorRow != nil {
			claim.ID = priorRow.ID
			if priorRow.Status == webhooks.StatusLaunchError {
				claim.ReceivedAt = priorRow.ReceivedAt
			}
			if err := s.webhookDeliveries.Update(ctx, claim); err != nil {
				s.gitlabApproveFailWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, approveRefusal{
					detail: "reset failed delivery: " + err.Error(), reply: approveAuditUnwritableReply,
				}, payloadHash, srcIP)
				return
			}
		} else if err := s.webhookDeliveries.Insert(ctx, claim); err != nil {
			if errors.Is(err, webhooks.ErrDuplicate) {
				resp := map[string]string{"status": webhooks.StatusDuplicate}
				if existing, gerr := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey); gerr == nil {
					resp["delivery_id"] = existing.ID
				}
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				writeJSONStatus(w, http.StatusOK, resp)
				return
			}
			s.gitlabApproveFailWithReply(ctx, w, cfg, meta, p, path.conn, path.connOK, approveRefusal{
				detail: "record delivery: " + err.Error(), reply: approveAuditUnwritableReply,
			}, payloadHash, srcIP)
			return
		}
	}

	desc := "approved by @" + p.AuthorUsername
	if reason != "" {
		desc += ": " + reason
	}
	if err := gc.SetCommitStatus(ctx, p.ProjectPath, pr.HeadSHA, forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     gateCtx,
		Description: desc,
		TargetURL:   p.NoteURL,
	}); err != nil {
		why := "set commit status: " + err.Error()
		s.warnApproveDidNotLand(cfg.Provider, p.ProjectPath, int(p.MRIID), p.AuthorUsername, why)
		claim.Status, claim.Error = webhooks.StatusLaunchError, why
		s.updateApproveDelivery(ctx, claim)
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunchError)
		s.postGitLabApproveReply(ctx, cfg, p, path.conn, path.connOK, "@"+p.AuthorUsername+" I can't approve: the forge refused the merge-gate status write — check the integration's statuses permission (docs/merge-gate.md), then redeliver the webhook or comment again")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": why})
		return
	}
	if s.logger != nil {
		s.logger.Info("webhooks: gitlab %s!%d %s force-greened by @%s (%q) via %s", p.ProjectPath, p.MRIID, gateCtx, p.AuthorUsername, reason, path.via)
	}
	now := time.Now().UTC()
	claim.Status, claim.Error, claim.LaunchedAt = webhooks.StatusLaunched, "", &now
	s.updateApproveDelivery(ctx, claim)
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunched)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved"})
}

// gitlabApproveWritePath is the client the approval status is written
// through, plus the connection identity the reply about it rides on —
// mirrors approveWritePath (the GitHub/Forgejo twin) exactly, minus the
// commenter field: GitLab's reply resolves separately, through
// gitlabPullCommenterFor, the same way GitHub's does through
// issueCommenterFor.
type gitlabApproveWritePath struct {
	gc     forgeGateClient
	conn   forge.Connection
	connOK bool
	via    string
}

// resolveGitLabApproveWritePath picks the client the approval status is
// written through — mirrors resolveApproveWritePath (the GitHub/Forgejo
// twin) structurally: the team connection's admin client first (via
// gitlabGateClientFor, which already tries connection-then-token), the
// webhook's own forge_token binding otherwise. A non-zero refusal is a
// configuration miss with nothing to write through; an error is a
// forge-side failure with no other credential to fall back on.
func (s *Server) resolveGitLabApproveWritePath(ctx context.Context, cfg webhooks.Config, reviewer, baseURL, host, projectPath string) (gitlabApproveWritePath, approveRefusal, error) {
	conn, connOK := s.forgeConnectionForPR(ctx, cfg.TenantID, "", host, projectPath)
	gc, writeRefusal := s.gitlabGateClientFor(ctx, cfg, baseURL, projectPath, reviewer, forgeNeedStatuses)
	if writeRefusal != "" {
		return gitlabApproveWritePath{conn: conn, connOK: connOK}, sameRefusal(writeRefusal), nil
	}
	via := "forge_token binding"
	if connOK {
		via = "connection " + conn.ID
	}
	return gitlabApproveWritePath{gc: gc, conn: conn, connOK: connOK, via: via}, approveRefusal{}, nil
}

// gitlabApproveFilteredWithReply is the configuration-refusal path, past the
// gate: audit `filtered` under its own key with the detail (no forge write
// was attempted, so a redelivery after the operator fixes the setup
// re-evaluates), tell the maintainer on the MR what to fix, answer 200.
func (s *Server) gitlabApproveFilteredWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, p gitlab.ParsedNote, conn forge.Connection, connOK bool, r approveRefusal, payloadHash, srcIP string) {
	if s.logger != nil {
		s.logger.Warn("webhooks: gitlab %s!%d /revi approve by @%s filtered: %s", p.ProjectPath, p.MRIID, p.AuthorUsername, r.detail)
	}
	s.recordNoteDelivery(ctx, cfg, webhooks.StatusFiltered, payloadHash, srcIP, p, r.detail)
	s.postGitLabApproveReply(ctx, cfg, p, conn, connOK, "@"+p.AuthorUsername+" I cannot approve here: "+r.reply)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
}

// gitlabApproveFailWithReply is the forge-failure path before the claim
// exists (MR head unresolvable, gate client unbuildable, audit store down):
// audit `launch_error` under its own key, tell the maintainer on the MR,
// answer 200 — never 502, which GitLab answers by disabling the hook.
func (s *Server) gitlabApproveFailWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, p gitlab.ParsedNote, conn forge.Connection, connOK bool, r approveRefusal, payloadHash, srcIP string) {
	s.warnApproveDidNotLand(cfg.Provider, p.ProjectPath, int(p.MRIID), p.AuthorUsername, r.detail)
	s.recordNoteDelivery(ctx, cfg, webhooks.StatusLaunchError, payloadHash, srcIP, p, r.detail)
	s.postGitLabApproveReply(ctx, cfg, p, conn, connOK, "@"+p.AuthorUsername+" I can't approve: "+r.reply)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": r.detail})
}

// postGitLabApproveReply best-effort tells the maintainer on the MR why the
// approve did not land: through the resolved connection when there is one,
// else the team connection covering the repo; when the connection posts
// nothing — none covers the repo, or its client cannot serve — through the
// webhook's forge_token binding, the other identity this lane holds.
// Mirrors postApproveReply (the GitHub/Forgejo twin) exactly. Silent on
// every miss: the refusal is already on the delivery audit and in the log,
// and a failed comment must not compound it.
func (s *Server) postGitLabApproveReply(ctx context.Context, cfg webhooks.Config, p gitlab.ParsedNote, conn forge.Connection, connOK bool, body string) {
	baseURL, refusal := resolveForgeBaseURL(cfg, p.MRURL)
	if !connOK && refusal == "" {
		if c, ok := s.forgeConnectionForPR(ctx, cfg.TenantID, "", hostOfURL(baseURL), p.ProjectPath); ok {
			conn, connOK = c, true
		}
	}
	if connOK {
		if pc, err := s.gitlabPullCommenterFor(ctx, conn); err == nil && pc != nil {
			if _, err := pc.CommentPullRequest(ctx, p.ProjectPath, int(p.MRIID), body); err == nil {
				return
			}
		}
	}
	if refusal != "" {
		return
	}
	token, err := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer)
	if err != nil || token == "" {
		return
	}
	c := forgegitlab.New(s.forgeHTTPClient(), baseURL, token)
	if _, err := c.CommentPullRequest(ctx, p.ProjectPath, int(p.MRIID), body); err != nil && s.logger != nil {
		s.logger.Debug("webhooks: gitlab approve reply to %s!%d not posted: %v", p.ProjectPath, p.MRIID, err)
	}
}
