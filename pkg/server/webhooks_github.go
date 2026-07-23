package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// registerGitHubWebhookRoute wires the inbound GitHub delivery endpoint
// behind webhookAuth. GitHub authenticates with HMAC over the body, so
// the middleware just admits the call — this handler MUST verify the
// signature itself before any side effect.
func (s *Server) registerGitHubWebhookRoute() {
	s.mux.Handle("POST /api/webhooks/github/{id}", s.webhookAuth(webhooks.ProviderGitHub, http.HandlerFunc(s.handleGitHubWebhook)))
}

// handleGitHubWebhook handles a verified-by-middleware inbound GitHub
// PR webhook. Auth, rate-limit, quota, suspend-check and tenant
// stamping are already done by webhookAuth; the config is on ctx.
//
// CRITICAL: under SignModeHMAC, the middleware deliberately skips the
// token check; the body signature is the ONLY auth proof. This handler
// MUST verify it BEFORE any side effect (no delivery row write, no
// launch).
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, ok := webhookConfigFromContext(ctx)
	if !ok {
		httpError(w, http.StatusInternalServerError, "webhook context missing")
		return
	}
	// Signature gate FIRST — never write an audit row or call gateLaunch
	// for an unauthenticated request (would leak quota signal to a
	// random poker on the open route).
	body, payloadHash, srcIP, ok := s.verifyWebhookHMACBody(w, r, cfg, "github", r.Header.Get("X-Hub-Signature-256"))
	if !ok {
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case prforge.EventHeaderIssueComment:
		// Universal slash-command path: /featurly, /seki… on a PR or issue
		// comment. Routes through the command registry to its bot.
		s.handlePRForgeComment(ctx, w, r, cfg, webhooks.ProviderGitHub, body, payloadHash, srcIP)
		return
	case prforge.EventHeaderIssues:
		// Issue lifecycle path: labeling an issue (e.g. "implement") — or, with
		// AutoImplementOnOpen, opening one — launches an implementer bot
		// (featurly) that opens a PR back-linked to the issue. Distinct from the
		// PR auto-review and slash-command paths.
		s.handleGitHubIssues(w, r, cfg, body, payloadHash, srcIP)
		return
	case prforge.EventHeaderPullRequest:
		// fall through to the PR auto-review path below.
	default:
		// GitHub sends ping/push/… on the same URL — silently filter (200)
		// instead of 4xx, otherwise GitHub disables the webhook after
		// repeated failures.
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: event}, webhooks.StatusFiltered, payloadHash, srcIP, "unsupported X-GitHub-Event")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// "gh|" prefix keeps the idempotency key space disjoint from any other
	// provider for the same tenant in case ids get reused.
	s.handlePRForgeReview(ctx, w, r, cfg, body, payloadHash, srcIP, "gh|")
}

// handlePRForgeReview handles the shared PR auto-review path for GitHub
// and Forgejo/Gitea (identical prforge.Parsed wire shape): parse → filter
// → bot select → idempotent launch. idemPrefix keeps each provider's
// idempotency-key space disjoint for the same tenant.
func (s *Server) handlePRForgeReview(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP, idemPrefix string) {
	p, err := prforge.ParsePullRequest(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "pull_request"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid pull_request payload")
		return
	}
	meta := prforgePRMeta(p)

	// Fork guard (UNCONDITIONAL on the auto path): a fork PR (head repo != base
	// repo) is untrusted — an adversary can open one to run code in our runner
	// with the forge token and to exhaust the tenant's budget. So an inbound PR
	// event NEVER auto-launches a bot on a fork, regardless of block_fork_prs.
	// A repo-authorized collaborator can still run one DELIBERATELY by issuing a
	// `/command` on the PR: that path (handlePRForgeComment) gates on the
	// commenter's CollaboratorPermission, so only a trusted user, manually,
	// triggers a run against fork code. Filtered as a clean 200 so the forge
	// keeps the hook enabled.
	if p.IsCrossRepo() {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "fork PR — auto-launch blocked (untrusted; a repo collaborator can trigger a bot manually via a command)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Merge-queue auto-heal: a PR ejected from the queue for a conflict or a
	// combined-build failure (`dequeued` with a healable reason) is
	// dispatched to the branch-improvement bot (Billy) to rebase on the base,
	// resolve the conflict / fix the combined break, and push so it re-enters
	// the queue — closing the loop the queue opens (it DETECTS the break; the
	// bot REPAIRS it, no human). Same-repo + allowlist + bot-permitted only;
	// the per-(PR,head-sha) idempotency bounds a heal to one attempt per head
	// (Billy's push advances the head, so a re-eject re-heals the NEW state,
	// and Billy's own convergence + the author allowlist bound the loop).
	if p.NeedsAutoHeal() {
		if !webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
			!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderLogin) ||
			!cfg.AllowsBot(branchImproveBotID) {
			s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "auto-heal not permitted (project/author/bot)")
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
			return
		}
		healIdem := knowledge.ChecksumHex([]byte(fmt.Sprintf("heal|%s|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA)))
		mission := fmt.Sprintf(
			"This PR was ejected from the merge queue (reason: %s). Rebase the branch on `%s`, "+
				"resolve any conflicts, and fix whatever breaks the build when the branch is combined "+
				"with the current `%s` (a compile break, a stale generated file, a test broken by an "+
				"interleaved merge). Keep the PR's own change intact; only reconcile it with the new base. "+
				"Push so the PR can re-enter the merge queue.\n\n%s",
			p.DequeueReason, p.TargetBranch, p.TargetBranch, strings.TrimSpace(p.Title+"\n\n"+p.Description))
		healVars := branchImproveVars(p.TargetBranch, p.SourceBranch, p.PRURL, mission, false, cfg.LaunchVars)
		s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, healIdem, branchImproveBotID, healVars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
		return
	}

	if !p.IsReviewable() ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "pull_request", "pull_request") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
		!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderLogin) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Iterion-bot guard: a PR opened by iterion's OWN forge bot (Doki/Willy/
	// Featurly… through the tenant's forge integration) already converged inside
	// its own loop — auto-reviewing it wastes budget and adds noise. Filter it
	// (a human can still run `/revi` on it manually).
	if s.isIterionForgeBotAuthor(ctx, cfg, p.SenderLogin) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"PR authored by iterion's forge bot — auto-review skipped (self-produced; run /revi to force a review)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// PR-open auto-lane = REVIEW ONLY (Revi). Billy is NEVER auto-launched on a
	// PR-open — it runs on a deliberate `/billy` comment (or the narrow
	// merge-queue auto-heal above). resolveReviewBot gates AllowsBot.
	botID, ok := s.resolveReviewBot(ctx, w, cfg, meta, payloadHash, srcIP)
	if !ok {
		return
	}

	// Idempotency: one launch per (tenant, webhook, repo, PR#, head sha).
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("%s%s|%s|%s|%d|%s", idemPrefix, cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA)))

	scopeNotes := strings.TrimSpace(p.Title + "\n\n" + p.Description)
	vars := reviewPRVars(p.PRURL, p.TargetBranch, scopeNotes, cfg.LaunchVars, map[string]string{"pr_author": p.SenderLogin})

	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, botID, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
}

// handleGitHubIssues handles a verified inbound GitHub `issues` delivery. Two
// triggers launch the implementer bot: a "labeled" action whose label passes
// the webhook's LabelAllowlist (the deliberate opt-in), and — when the webhook
// enables AutoImplementOnOpen — an "opened" action (the zero-touch lane that
// turns every new issue into a PR). Everything else is filtered (200) so GitHub
// keeps the hook enabled. The launched bot (e.g. featurly) gets
// feature_prompt/open_mr/source_issue_ref so it implements the issue and opens
// a PR back-linked to it.
func (s *Server) handleGitHubIssues(w http.ResponseWriter, r *http.Request, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	ctx := r.Context()
	p, err := prforge.ParseIssues(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "issues"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid issues payload")
		return
	}
	meta := prforgeIssueMeta(p)

	// Common gates (event + project) mirror the PR path.
	labeled := p.IsLabeled() && webhooks.MatchLabel(cfg.LabelAllowlist, p.LabelName)
	openedZeroTouch := p.IsOpened() && cfg.AutoImplementOnOpen
	if (!labeled && !openedZeroTouch) ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "issues", "issues") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	botID, ok := s.selectIssueLabeledBot(ctx, w, cfg, meta, payloadHash, srcIP)
	if !ok {
		return
	}

	// Author-trust gate on the zero-touch lane ONLY: an opened issue launches
	// a bot with no human in the loop, so the author must hold real repo
	// rights (the budget boundary against drive-by issues). The labeled lane
	// needs no gate — applying the trigger label already requires triage+
	// rights on the forge, which IS the approval gesture.
	if openedZeroTouch && !s.issueAuthorTrusted(ctx, cfg, webhooks.ProviderGitHub, botID, p) {
		author := p.IssueAuthorLogin
		if author == "" {
			author = p.SenderLogin
		}
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"untrusted issue author "+author+" — parked for operator approval")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Idempotency: one launch per (tenant, webhook, repo, issue#, trigger).
	// The trigger is the label for the labeled path (re-applying a DIFFERENT
	// label still launches; the SAME label replays no-op) and a stable "opened"
	// marker for the zero-touch path (so a later label on the same issue is a
	// distinct trigger, not a replay of the open).
	trigger := p.LabelName
	if !labeled {
		trigger = "opened"
	}
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("gh|issue|%s|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.IssueNumber, trigger)))

	// Route through dispatchInvocation so a one-way tracking card is
	// materialised on the tenant's board (idempotent, linked to the issue via
	// source_issue_ref) — exactly like the slash-command path — while the run
	// still launches (or a board coordinator owns it). repoRef empty → the
	// runner clones the repo's default branch; featurly's worktree: auto
	// branches from there.
	route := s.boardRouteForLabel(botID)
	vars := issueLabeledVars(p, cfg.LaunchVars, route.ArgsVar)
	s.dispatchInvocation(ctx, w, r, cfg, meta, idemKey, route, vars, p.CloneURL, "", payloadHash, srcIP)
}

// issueLabeledVars composes the launch vars an implementer bot (featurly)
// needs to turn a labeled issue into a back-linked PR: the issue
// title+body as the feature prompt, open_mr to push+open the PR, and
// source_issue_ref (the issue URL) so finalize_mr comments the PR URL back
// onto the issue. Operator-pinned LaunchVars win last.
func issueLabeledVars(p prforge.ParsedIssue, launchVars map[string]string, argsVar string) map[string]string {
	if argsVar == "" {
		argsVar = "feature_prompt"
	}
	vars := map[string]string{
		argsVar:            strings.TrimSpace(p.IssueTitle + "\n\n" + p.IssueBody),
		"open_mr":          "true",
		"source_issue_ref": p.IssueURL,
	}
	mergeVarsInto(vars, launchVars)
	return vars
}

// prforgeIssueMeta flattens a parsed issues event into webhookEventMeta.
// SubjectURL carries the issue's own URL (the back-link target); the
// delivery row records the issue subject + the label that triggered it.
func prforgeIssueMeta(p prforge.ParsedIssue) webhookEventMeta {
	subject := ""
	if p.IssueNumber != 0 {
		subject = p.SubjectID()
	}
	return webhookEventMeta{
		Kind:         "issues",
		Action:       p.Action,
		ProjectPath:  p.ProjectPath,
		SubjectID:    subject,
		SubjectURL:   p.IssueURL,
		SenderHandle: p.SenderLogin,
	}
}

// prforgePRMeta flattens a parsed PR-over-forge event (GitHub or
// Forgejo/Gitea) into webhookEventMeta — the wire shape is identical
// between the two providers, so the helper is shared by both handlers.
func prforgePRMeta(p prforge.Parsed) webhookEventMeta {
	subject := ""
	if p.PRNumber != 0 {
		subject = p.SubjectID()
	}
	return webhookEventMeta{
		Kind:         "pull_request",
		Action:       p.Action,
		ProjectPath:  p.ProjectPath,
		SubjectID:    subject,
		SubjectSHA:   p.HeadSHA,
		SenderHandle: p.SenderLogin,
	}
}
