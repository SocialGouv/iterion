package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
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

	// A review request explicitly targeting iterion's own forge identity —
	// the "Re-request review" button (or first "Request review") on the bot
	// reviewer — is the button form of `/revi`: a deliberate on-demand
	// re-review. The forge itself gates who may edit a PR's reviewers (write
	// access), so no extra replier authz applies — but only on an OPEN PR
	// (reviewer edits arrive freely on closed/merged ones). Never when the
	// actor IS the bot: its own reviewer-write echoing back must not launch
	// a review of itself. The identity matched is iterionBotLogins — on
	// GitHub/Forgejo that is the App bot login only (a PAT/OAuth account may
	// be a HUMAN's), and a GitHub App cannot be a reviewer at all, so on
	// GitHub this lane stays inert and `/revi` is the on-demand path.
	reviewRequested := strings.EqualFold(p.State, "open") &&
		s.isIterionBotReviewRequest(ctx, cfg, p.ReviewRequestedFrom) &&
		!s.isIterionForgeBotAuthor(ctx, cfg, p.SenderLogin)

	// Hold-label gate (bot-agnostic, opt-in): a configured hold label on the PR
	// vetoes EVERY auto-launch this handler can do (auto-heal and review alike)
	// — the operator's escape hatch to pause automation on one PR. Placed before
	// any launch decision so it covers all of them. A human can still trigger a
	// bot manually via a `/command` — and a review re-request is the same
	// deliberate gesture, so it is exempt too.
	if !reviewRequested && s.suppressedByHoldLabel(ctx, w, cfg, meta, p.Labels, payloadHash, srcIP) {
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
		// ONE role snapshot for the whole lane: the allowlist gate and the
		// launch below must name the SAME bot — two roleBots() reads can
		// straddle a settings refresh and authorize A while launching B.
		brancher := s.roleBots().Brancher
		if !webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
			!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderLogin) ||
			!cfg.AllowsBot(brancher) {
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
		healVars := applyWebhookVarLayers(fixerPRVars(p.TargetBranch, p.SourceBranch, p.PRURL, mission, false, nil), cfg)
		s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, healIdem, brancher, healVars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
		return
	}

	// The merge gate opts synchronize (a push to the head) back into review so
	// the revi/review status re-evaluates on the new head SHA; otherwise only
	// opened/reopened/ready_for_review review (on-demand re-review on push).
	// Never on a closed/merged PR (a push to a dead PR's branch still
	// delivers synchronize); fail-open on a payload without `state` — a
	// filtered resync strands the required check.
	gateResync := cfg.ReviewOnSync && p.IsSynchronize() && p.StateOpenOrUnknown()
	reviewable := p.IsReviewable() || gateResync || reviewRequested
	if !reviewable ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "pull_request", "pull_request") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
		// The PR's AUTHOR, not the pusher: on a synchronize the sender is
		// whoever pushed, so filtering on it would drop a dependency bot's PR
		// the moment a human pushed a fix onto it.
		!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.Author()) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Iterion-bot guard: a PR opened by iterion's OWN forge bot (Doki/Willy/
	// Featurly… through the tenant's forge integration) already converged inside
	// its own loop — auto-reviewing it wastes budget and adds noise. Filter it
	// (a human can still run `/revi` on it manually).
	// Deliberately the SENDER, not the PR author: the point is to skip a PR
	// our own loop produced and already converged, but once a human pushes
	// onto it there is human work to review again.
	//
	// It does NOT apply to a merge-gate resync. There the pusher is by
	// construction our own forge bot — a fixer that just pushed onto someone
	// else's PR — and the re-review is the whole mechanism: it is what puts
	// the required check back on the new head, and it is the independent
	// verdict that supersedes the fixer's own (docs/merge-gate.md, "Two bots
	// on the SAME pull request"). Filtering here left the fixer's self-verdict
	// as the last word on a head no reviewer had read, and on a head where a
	// bot pushed without gating at all, it left the required check absent.
	// (A reviewRequested delivery is exempt: its own actor guard already
	// excluded a bot sender, and a human's re-request on a bot-authored PR
	// is deliberate, so it reviews.)
	if !gateResync && !reviewRequested && s.isIterionForgeBotAuthor(ctx, cfg, p.SenderLogin) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"PR authored by iterion's forge bot — auto-review skipped (self-produced; run /revi to force a review)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// PR-open auto-lane = REVIEW ONLY. Billy is NEVER auto-launched on a
	// PR-open — it runs on a deliberate `/billy` comment (or the narrow
	// merge-queue auto-heal above). With a per-bot routing table the lane fans
	// out to every bot claiming the pull_request event whose OWN author filter
	// admits this author, so a dependency guard and a reviewer share the repo
	// and each takes its own PRs.
	rules := s.resolveForgeEventBots(cfg, bundle.ForgeEventPullRequest, p.Author())
	if len(rules) == 0 {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "no enabled bot claims this PR (event/author routing)")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Idempotency base: one launch per (tenant, webhook, repo, PR#, head sha)
	// — per bot once the delivery fans out (see forgeIdemKey).
	idemBase := fmt.Sprintf("%s%s|%s|%s|%d|%s", idemPrefix, cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA)
	extra := map[string]string{"pr_author": p.Author(), "source_branch": p.SourceBranch, "head_sha": p.HeadSHA}
	// !gateResync mirrors the GitLab lane; here the two are already mutually
	// exclusive by action (review_requested vs synchronize) — the guard pins
	// the invariant against a forge overloading one action with both.
	if reviewRequested && !gateResync {
		// A deliberate re-request must relaunch even on a head the auto-review
		// already claimed — and again on a second click. The PR's updated_at
		// salts the key so each click is its own delivery; "rereq|" keeps it
		// disjoint from the open/resync space. re_review marks the posted
		// summary like the `/revi` comment path does.
		idemBase = fmt.Sprintf("%srereq|%s|%s|%s|%d|%s|%s", idemPrefix, cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA, p.UpdatedAt)
		extra["re_review"] = "true"
	}

	scopeNotes := strings.TrimSpace(p.Title + "\n\n" + p.Description)
	targets := forgePREventTargets(cfg, rules, idemBase, p.PRURL, p.TargetBranch, scopeNotes, p.CloneURL, p.SourceBranch, extra)

	s.insertAndLaunchWebhookMulti(ctx, w, r, cfg, meta, targets, payloadHash, srcIP)
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

	// Hold-label gate (bot-agnostic, opt-in): a configured hold label on the
	// issue vetoes the auto-launch (labeled + zero-touch alike). See the PR
	// handler for the rationale.
	if s.suppressedByHoldLabel(ctx, w, cfg, meta, p.IssueLabels, payloadHash, srcIP) {
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
	vars := applyWebhookVarLayers(issueLabeledVars(p, nil, route.ArgsVar), cfg)
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
