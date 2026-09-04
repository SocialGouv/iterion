package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	fforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	fgithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// prforgeReplierAPI is the minimal forge surface command handling needs:
// the bot's own identity (loop-guard), a commenter's repo permission
// (role-gate), and the PR a command comment sits on (head/base branch
// resolution). Both pkg/forge/{github,forgejo}.AdminClient satisfy it.
type prforgeReplierAPI interface {
	WhoAmI(ctx context.Context) (forge.Identity, error)
	CollaboratorPermission(ctx context.Context, repo, user string) (string, error)
	GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error)
}

// handlePRForgeComment routes a GitHub/Forgejo issue_comment (PR or issue) to
// its bot via the command registry — the GitHub/Forgejo twin of
// handleGitLabCommandNote. Every benign refusal is a 200/filtered so the
// forge does not auto-disable the hook.
func (s *Server) handlePRForgeComment(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, provider webhooks.Provider, body []byte, payloadHash, srcIP string) {
	p, err := prforge.ParseIssueComment(body)
	if err != nil {
		// Malformed body → filter (200), not 4xx: repeated 4xx make the forge
		// disable the webhook, and issue_comment shares the PR delivery URL.
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "issue_comment"}, webhooks.StatusFiltered, payloadHash, srcIP, "invalid issue_comment payload")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}
	meta := prforgeNoteMeta(p)
	filtered := func(reason string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	if p.Action != "created" || p.IssueState != "open" ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "issue_comment", "issue_comment") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
		filtered("out of scope (not a new open-issue comment / event / project)")
		return
	}
	cmd, cmdArgs := p.Command()
	if cmd == "" {
		filtered("no slash-command")
		return
	}
	// `/revi approve [reason]` is an OVERRIDE, not a re-review: a trusted
	// maintainer force-greens the merge gate. Intercept before the
	// command→bot routing so it never launches a run.
	if reason, isApprove := reviewApproveReason(cmd, cmdArgs); isApprove {
		s.handlePRForgeReviewApprove(ctx, w, cfg, provider, p, reason, payloadHash, srcIP)
		return
	}
	route, ok := webhooks.ResolveCommandRoute(cfg, cmd, cmdArgs, s.cmdDiscovery())
	if !ok {
		filtered("no command route for /" + cmd)
		return
	}
	if !route.AllowsScope(p.Surface()) {
		filtered("/" + cmd + " is not enabled on " + p.Surface() + " comments")
		return
	}
	if !cfg.AllowsBot(route.BotID) {
		filtered("bot " + route.BotID + " not permitted by this webhook")
		return
	}
	gate := s.webhookPRForgeCommandGate
	if gate == nil {
		gate = s.realWebhookPRForgeCommandGate
	}
	authorized, reason, aerr := gate(ctx, cfg, provider, p, route)
	if aerr != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		filtered(reason)
		return
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: %s comment %s#%d (/%s) by %s → %s (%s)", provider, p.ProjectPath, p.IssueNumber, cmd, p.AuthorLogin, route.BotID, reason)
	}
	// The issue_comment payload carries no PR head branch, so for a PR-surface
	// command the PR is resolved via the forge API: the run must check out the
	// PR head (repoRef) and know its base (base_ref/target_branch vars) — a
	// launch on the default branch sees an empty diff and no-ops, which reads
	// as "the bot did nothing". Resolution failure is a visible launch error,
	// never a silent fall-back to the default branch.
	var pr *forge.PullRef
	repoRef := ""
	if p.Surface() == "pr" {
		resolve := s.webhookPRForgePRResolver
		if resolve == nil {
			resolve = s.realWebhookPRForgePRResolver
		}
		resolved, rerr := resolve(ctx, cfg, provider, p, route)
		if rerr != nil {
			s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "PR resolution: "+rerr.Error())
			httpError(w, http.StatusBadGateway, "could not resolve the PR head branch")
			return
		}
		if resolved.State != "" && resolved.State != "open" {
			filtered("PR is " + resolved.State + " — command ignored")
			return
		}
		// Fork guard, fail-CLOSED: on a fork PR the launch pair (base repo's
		// CloneURL + PR head ref) does NOT name one repository — the head ref
		// lives in the head repo, so the checkout misses (or, worse, hits a
		// same-named branch on the base and the bot answers grounded in the
		// wrong code, under the bot's identity). An empty HeadRepoFullName
		// is equally unsafe: both GitHub and Forgejo emit `head.repo: null`
		// once the head repo is deleted or blocked, which only a fork can
		// be, and launching on it aims the bot at repoURL=<base>
		// repoRef=<head branch>. SameRepoAs is false on an empty head, so
		// the launch pair must be PROVEN same-repo before the bot runs.
		if !resolved.SameRepoAs(p.ProjectPath) {
			filtered("fork PR or unverifiable head repo — /" + cmd + " runs are same-repo only")
			return
		}
		pr = &resolved
		repoRef = resolved.SourceBranch
	}
	vars := applyWebhookVarLayers(buildPRForgeCommandVars(p, pr, route, cmdArgs, nil), cfg)
	if pr != nil {
		stampBranchImprovePushBack(vars, route.BotID, s.roleBots().Brancher, pr.SourceBranch, cfg.BranchImproveAsPR)
		// The revision the command is about, so the shared launch tail can tell a
		// consumer whether the review it is handed still matches the PR head.
		vars["head_sha"] = pr.HeadSHA
	}
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("cmd|%s|%s|%s|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.SubjectID())))
	s.dispatchInvocation(ctx, w, r, cfg, meta, idemKey, route, vars, p.CloneURL, repoRef, payloadHash, srcIP)
}

// realWebhookPRForgePRResolver fetches the PR a command comment sits on, with
// the bot's own forge token — the same credential the command gate resolved.
// The head/base branches feed the launch (checkout ref + branch vars).
func (s *Server) realWebhookPRForgePRResolver(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, route webhooks.CommandRoute) (forge.PullRef, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, route.BotID)
	if terr != nil || token == "" {
		return forge.PullRef{}, fmt.Errorf("no forge token resolved: %v", terr)
	}
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		return forge.PullRef{}, fmt.Errorf("forge base URL: %s", refusal)
	}
	api := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
	return api.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
}

// buildPRForgeCommandVars composes the launch vars for a generic command on a
// GitHub/Forgejo comment: issue/PR context + the route's manifest ContextVars
// + operator LaunchVars, the command args landing in the route's args_var.
// On a PR-surface command pr carries the resolved PR: its head/base stamp the
// branch vars (base_ref is the dep-update-guard spelling, target_branch /
// source_branch the reviewer-loop one) so the bot diffs the right range.
// ContextVars/LaunchVars still win — an operator override stays authoritative.
func buildPRForgeCommandVars(p prforge.ParsedNote, pr *forge.PullRef, route webhooks.CommandRoute, args string, launchVars map[string]string) map[string]string {
	vars := map[string]string{
		"pr_url":      p.PRURL,
		"scope_notes": strings.TrimSpace(p.IssueTitle + "\n\n" + p.IssueBody),
	}
	if pr != nil {
		if pr.TargetBranch != "" {
			vars["base_ref"] = pr.TargetBranch
			vars["target_branch"] = pr.TargetBranch
		}
		if pr.SourceBranch != "" {
			vars["source_branch"] = pr.SourceBranch
		}
		if pr.Author != "" {
			vars["pr_author"] = pr.Author
		}
	}
	for k, v := range route.ContextVars {
		vars[k] = v
	}
	for k, v := range launchVars {
		vars[k] = v
	}
	if route.ArgsVar != "" && strings.TrimSpace(args) != "" {
		vars[route.ArgsVar] = args
	}
	return vars
}

func prforgeNoteMeta(p prforge.ParsedNote) webhookEventMeta {
	return webhookEventMeta{
		Kind:            "issue_comment",
		Action:          "comment",
		ProjectPath:     p.ProjectPath,
		SubjectID:       p.SubjectID(),
		ParentSubjectID: p.ParentSubjectID(),
		SenderHandle:    p.AuthorLogin,
		// IssueURL is the issue/PR the comment sits on — the back-link target a
		// command bot posts its opened MR/PR URL onto (via the ensureBoardCard
		// open_mr stamp). Works for both surfaces (Surface()=="pr"|"issue").
		SubjectURL: p.IssueURL,
	}
}

// realWebhookPRForgeCommandGate is the production replier gate for a GitHub /
// Forgejo command comment: resolve the bot's forge token, reject the bot's
// own comment (loop-guard), then authorize the commenter — allowlist OR a
// repo-permission >= the route's MinReplierRole (falling back to the webhook
// default). ok=false + reason for benign refusals; err only for infra failure.
func (s *Server) realWebhookPRForgeCommandGate(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, route webhooks.CommandRoute) (bool, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, route.BotID)
	if terr != nil || token == "" {
		return false, "no forge token resolved (configure a forge_token binding)", nil
	}
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		return false, refusal, nil
	}
	api := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
	if id, err := api.WhoAmI(ctx); err == nil && id.Login != "" && strings.EqualFold(id.Login, p.AuthorLogin) {
		return false, "self comment (loop-guard)", nil
	}
	// Allowlist short-circuit (no API call).
	if prforgeInAllowlist(cfg.AuthorizedRepliers, p.AuthorLogin) {
		return true, "allowlist", nil
	}
	perm, err := api.CollaboratorPermission(ctx, p.ProjectPath, p.AuthorLogin)
	if err != nil {
		return false, "", err
	}
	minRole := route.MinReplierRole
	if minRole == "" {
		minRole = cfg.MinReplierRole
	}
	if prforgePermRank(perm) >= replierMinRoleRank(minRole) {
		return true, "role", nil
	}
	return false, "replier not authorized: " + p.AuthorLogin, nil
}

// prforgeReplierClient builds the right minimal forge client for the gate.
func prforgeReplierClient(provider webhooks.Provider, httpClient *http.Client, baseURL, token string) prforgeReplierAPI {
	if provider == webhooks.ProviderForgejo {
		return fforgejo.New(httpClient, baseURL, token)
	}
	return fgithub.New(httpClient, baseURL, token)
}

// prforgeBaseURL decides the forge web base the bot's token may be sent to.
// A per-webhook ForgeBaseURL (set by the orchestrator from the connection) is
// authoritative; otherwise the host is derived from the comment/PR URL and
// gated by the optional ITERION_WEBHOOK_FORGE_HOSTS allowlist (same posture as
// the GitLab path). Returns a non-empty refusal when the token must not be
// sent.
func prforgeBaseURL(cfg webhooks.Config, p prforge.ParsedNote) (baseURL, refusal string) {
	if cfg.ForgeBaseURL != "" {
		return cfg.ForgeBaseURL, ""
	}
	ref := p.PRURL
	if ref == "" {
		ref = p.CommentURL
	}
	return prforgeBaseURLFromRef(ref)
}

// prforgeBaseURLFromRef derives (and host-gates) the forge web base from a
// payload URL — the shared tail of prforgeBaseURL, reused by the
// review-request replier gate whose subject is the PR itself.
func prforgeBaseURLFromRef(ref string) (baseURL, refusal string) {
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" || u.User != nil {
		return "", "payload URL has no usable forge host"
	}
	if !forgeHostAllowed(u.Host) {
		return "", "forge host not in ITERION_WEBHOOK_FORGE_HOSTS allowlist"
	}
	return u.Scheme + "://" + u.Host, ""
}

// realWebhookPRForgeReviewRequestGate authorizes a review re-request on the
// GitHub/Forgejo lane — the same replier controls as the `/command` gate
// (allowlist OR CollaboratorPermission ≥ the webhook's min_replier_role).
// R6a15fe: the GitLab twin was gated in R7e050f for exactly this reason
// ("the forge gates reviewer edits" is not an authorization story) — and
// the lane is LIVE here: cfg.ReviewRequestLogins arms it with a User
// identity (GitHub grants "request review" at the Triage role, below the
// write floor this gate enforces). Fail-closed on token resolution, like
// the GitLab twin.
func (s *Server) realWebhookPRForgeReviewRequestGate(ctx context.Context, cfg webhooks.Config, p prforge.Parsed, botID string) (bool, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil || token == "" {
		return false, "re-request refused: no forge token resolved (configure a forge_token binding)", nil
	}
	baseURL := cfg.ForgeBaseURL
	if baseURL == "" {
		var refusal string
		baseURL, refusal = prforgeBaseURLFromRef(p.PRURL)
		if refusal != "" {
			return false, refusal, nil
		}
	}
	api := prforgeReplierClient(cfg.Provider, s.forgeHTTPClient(), baseURL, token)
	if prforgeInAllowlist(cfg.AuthorizedRepliers, p.SenderLogin) {
		return true, "allowlist", nil
	}
	perm, err := api.CollaboratorPermission(ctx, p.ProjectPath, p.SenderLogin)
	if err != nil {
		return false, "", err
	}
	if prforgePermRank(perm) >= replierMinRoleRank(cfg.MinReplierRole) {
		return true, "role", nil
	}
	return false, "re-request review by unauthorized replier: " + p.SenderLogin, nil
}

func prforgeInAllowlist(allow []string, login string) bool {
	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a != "" && strings.EqualFold(strings.TrimPrefix(a, "@"), login) {
			return true
		}
	}
	return false
}

// prforgePermRank maps a GitHub/Forgejo collaborator permission to a rank on
// the same scale as replierMinRoleRank so a gitlab-vocab MinReplierRole gates
// cross-forge commenters sensibly.
func prforgePermRank(perm string) int {
	switch strings.ToLower(strings.TrimSpace(perm)) {
	case "owner", "admin":
		// "owner" is Forgejo/Gitea's answer for the repository owner; GitHub
		// reports the same account as admin.
		return 5
	case "maintain":
		return 4
	case "write":
		return 3
	case "triage":
		return 2
	case "read":
		return 1
	}
	return 0 // none
}

// replierMinRoleRank maps a MinReplierRole (gitlab vocabulary) to a rank.
// Empty defaults to "developer" (matching the GitLab gate default), which
// equals a GitHub "write" collaborator. Thin delegate: the table lives in
// pkg/webhooks so the provision carry's stricter-of merge reads the same
// ordering as the gates.
func replierMinRoleRank(role string) int {
	return webhooks.ReplierRoleRank(role)
}

// ---------------------------------------------------------------------------
// Review-thread conversations (GitHub) — reply to a Revi suggestion, get an
// in-thread answer. The GitHub twin of the GitLab note reply-in-thread lane
// (docs/forge-conversations.md); Forgejo is NOT wired yet (its dispatch never
// routes the event here).
// ---------------------------------------------------------------------------

// prforgeReviewThreadAPI extends the replier surface with the thread fetch
// the reply gate needs. github.AdminClient satisfies it; the Forgejo client
// deliberately does not (the lane is GitHub-only until Forgejo's
// review-comment API is validated).
type prforgeReviewThreadAPI interface {
	prforgeReplierAPI
	ListPRReviewComments(ctx context.Context, repo string, number int) ([]forge.PRReviewComment, error)
}

// handlePRForgeReviewThreadReply routes a pull_request_review_comment event
// (a reply inside a PR review thread) to the converse bot when the thread is
// one of iterion's own: replying to a Revi suggestion IS the question, no
// slash-command needed. Mirrors the GitLab note lane's reply-in-thread half.
// Every benign refusal is a 200/filtered so the forge never auto-disables
// the hook.
func (s *Server) handlePRForgeReviewThreadReply(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, provider webhooks.Provider, body []byte, payloadHash, srcIP string) {
	p, err := prforge.ParseReviewComment(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "review_comment"}, webhooks.StatusFiltered, payloadHash, srcIP, "invalid pull_request_review_comment payload")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}
	meta := prforgeReviewCommentMeta(p)
	filtered := func(reason string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	// The allowlist must name the event (or be empty = zero-config): configs
	// provisioned before this lane carry ["issue_comment","pull_request"]
	// and stay inert until re-provisioned WITH the converse bot —
	// pull_request_review_comment is its own normalized manifest event,
	// declared by the converse bot alone (not folded into
	// pull_request_comment), so only its presence in bot_ids makes the
	// re-provision subscribe the hook and regenerate the allowlist.
	if p.Action != "created" || p.PRState != "open" ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "pull_request_review_comment", "pull_request_review_comment") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) {
		filtered("out of scope (not a new open-PR review comment / event / project)")
		return
	}
	// A thread-opening comment roots its own thread: nobody — the bot
	// included — is in that thread yet, so it can never be a reply to the
	// bot. Decidable from the payload alone, before any I/O; every inline
	// comment of a bot review echoes back as one of these.
	if p.ThreadRootID == p.CommentID {
		filtered("thread-opening comment (not a reply)")
		return
	}
	// Fork guard, payload-only: on a fork PR the launch pair (base repo's
	// CloneURL + head-repo SourceBranch) does not name one repository — the
	// checkout would miss, or silently hit a same-named BASE branch and the
	// bot would answer grounded in the wrong code. Same posture as the PR
	// auto lane: filtered.
	if p.IsCrossRepo() {
		filtered("fork PR — review-thread replies are same-repo only")
		return
	}
	// Loop-guard next, still without forge I/O: the converse bot answers
	// with the same PAT identity, so its own reply echoes back as this
	// event.
	if s.isIterionForgeBotAuthor(ctx, cfg, p.AuthorLogin) {
		filtered("self reply (loop-guard)")
		return
	}
	// ONE role snapshot: the enable-gate and the launch below must name the
	// same converse bot.
	converseBot := s.roleBots().ReviConverse
	if !s.canRouteToConverseBot(cfg, converseBot) {
		filtered("converse bot not enabled on this webhook")
		return
	}
	gate := s.webhookPRForgeReviewReplyGate
	if gate == nil {
		gate = s.realWebhookPRForgeReviewReplyGate
	}
	authorized, threadContext, reason, aerr := gate(ctx, cfg, provider, p, converseBot)
	if aerr != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		filtered(reason)
		return
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: %s review-thread reply %s#%d by %s authorized (%s)", provider, p.ProjectPath, p.PRNumber, p.AuthorLogin, reason)
	}
	vars := applyWebhookVarLayers(buildPRForgeReviewReplyVars(p, threadContext, nil), cfg)
	// Idempotency: one launch per reply comment; "rc|" keeps the key space
	// disjoint from the pr|/cmd| paths.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("rc|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.RepoID, p.SubjectID())))
	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, converseBot, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
}

// prforgeReviewCommentMeta builds the delivery-audit meta for a review-thread
// reply.
func prforgeReviewCommentMeta(p prforge.ParsedReviewComment) webhookEventMeta {
	return webhookEventMeta{
		Kind:            "review_comment",
		Action:          "comment",
		ProjectPath:     p.ProjectPath,
		SubjectID:       p.SubjectID(),
		ParentSubjectID: p.ParentSubjectID(),
		SubjectURL:      p.PRURL,
		SubjectSHA:      p.HeadSHA,
		SenderHandle:    p.AuthorLogin,
	}
}

// buildPRForgeReviewReplyVars composes the converse launch vars for a
// review-thread reply: the PR context (reviewPRVars) + the conversation vars
// the converse bot declares. discussion_id is the THREAD ROOT comment id —
// exactly what GitHub's /pulls/{n}/comments/{id}/replies endpoint wants (see
// bots/revi-converse/skills/forge-reply.md §4). No re_review flag: a reply
// is a question, never a fresh review.
func buildPRForgeReviewReplyVars(p prforge.ParsedReviewComment, threadContext string, launchVars map[string]string) map[string]string {
	question := strings.TrimSpace(p.CommentBody)
	vars := reviewPRVars(p.PRURL, p.TargetBranch, strings.TrimSpace(p.PRTitle+"\n\n"+p.PRBody), launchVars, map[string]string{
		"source_branch":     p.SourceBranch,
		"head_sha":          p.HeadSHA,
		"conversation_mode": "reply",
		"discussion_id":     fmt.Sprintf("%d", p.ThreadRootID),
		"trigger_note":      p.CommentBody,
		"replier":           p.AuthorLogin,
		"converse_question": question,
	})
	if threadContext != "" {
		vars["thread_context"] = threadContext
	}
	return vars
}

// realWebhookPRForgeReviewReplyGate is the production reply gate: resolve
// the bot's forge token and hand the thread work to reviewReplyGateWithAPI.
// ok=false + reason for benign refusals; err only for infra failure.
func (s *Server) realWebhookPRForgeReviewReplyGate(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedReviewComment, botID string) (bool, string, string, error) {
	token, terr := s.resolveForgeToken(ctx, cfg, botID)
	if terr != nil || token == "" {
		return false, "", "no forge token resolved (configure a forge_token binding)", nil
	}
	baseURL := strings.TrimSpace(cfg.ForgeBaseURL)
	if baseURL == "" {
		return false, "", "no forge base url on this webhook", nil
	}
	client := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
	api, ok := client.(prforgeReviewThreadAPI)
	if !ok {
		return false, "", "review-thread conversations are not supported on this provider yet", nil
	}
	return s.reviewReplyGateWithAPI(ctx, cfg, p, api)
}

// reviewReplyGateWithAPI is the token-free core of the reply gate — split
// from the wrapper so tests drive it with a fake thread API. It fetches the
// PR's review comments, keeps only the reply's thread, and authorizes BOTH
// halves: the thread must be one of iterion's own (a comment by the bot
// identity in it; a human↔human thread never triggers), and the replier must
// clear the allowlist or the webhook's MinReplierRole. Returns the thread
// transcript for the bot's grounding.
func (s *Server) reviewReplyGateWithAPI(ctx context.Context, cfg webhooks.Config, p prforge.ParsedReviewComment, api prforgeReviewThreadAPI) (bool, string, string, error) {
	isBot, haveIdentity := s.iterionBotAuthorPredicate(ctx, cfg)
	// The connection-derived set can be legitimately empty (a GitHub
	// PAT/OAuth connection names no [bot] identity). The identity that
	// actually POSTS our reviews is the token's own — resolve it like the
	// GitLab twin (CurrentUser) and union it in, so the lane lives on those
	// connections. WhoAmI failing on an App installation token is fine: the
	// [bot] slug already covers that shape.
	if id, werr := api.WhoAmI(ctx); werr == nil && strings.TrimSpace(id.Login) != "" {
		tokenLogin, base := id.Login, isBot
		isBot = func(login string) bool { return base(login) || strings.EqualFold(login, tokenLogin) }
		haveIdentity = true
	}
	if !haveIdentity {
		// Fail closed with an honest reason — "not a bot review thread"
		// would misdiagnose a dead identity resolution as a human thread.
		return false, "", "bot identity unresolved; cannot classify reply", nil
	}
	// The completing half of the handler's loop-guard: that one reads the
	// connection-derived set only, which on a PAT/OAuth connection is empty
	// — yet the token identity resolved above is exactly who the bot's own
	// in-thread answer posts as. Without this check that answer walks the
	// whole gate, authorizes itself (the posting account has write), and
	// the conversation answers itself forever.
	if isBot(p.AuthorLogin) {
		return false, "", "self reply (loop-guard, token identity)", nil
	}
	comments, err := api.ListPRReviewComments(ctx, p.ProjectPath, int(p.PRNumber))
	if err != nil {
		return false, "", "", err
	}
	botInThread, transcript := classifyReviewThread(comments, p, isBot)
	if !botInThread {
		return false, "", "not a bot review thread (no iterion comment in it)", nil
	}
	if prforgeInAllowlist(cfg.AuthorizedRepliers, p.AuthorLogin) {
		return true, transcript, "allowlist", nil
	}
	perm, err := api.CollaboratorPermission(ctx, p.ProjectPath, p.AuthorLogin)
	if err != nil {
		return false, "", "", err
	}
	if prforgePermRank(perm) >= replierMinRoleRank(cfg.MinReplierRole) {
		return true, transcript, "role", nil
	}
	return false, "", "replier not authorized: " + p.AuthorLogin, nil
}

// classifyReviewThread keeps only the reply's thread out of the PR's review
// comments and decides the gate's thread half: whether the bot identity
// participates in it — the trigger comment itself never counts, so a thread
// where only the human's reply exists stays untriggerable — plus the
// transcript, bot entries labelled and the whole capped by the same
// anchor+newest budget as the GitLab twin (maxThreadContextChars).
func classifyReviewThread(comments []forge.PRReviewComment, p prforge.ParsedReviewComment, isBot func(string) bool) (bool, string) {
	botInThread := false
	rendered := make([]string, 0, len(comments))
	for _, c := range comments {
		if c.ID != p.ThreadRootID && c.InReplyTo != p.ThreadRootID {
			continue
		}
		who := c.Author
		if isBot(c.Author) {
			if c.ID != p.CommentID {
				botInThread = true
			}
			who += " (you, the bot)"
		}
		rendered = append(rendered, fmt.Sprintf("%s (%s):\n%s", who, c.CreatedAt, strings.TrimSpace(c.Body)))
	}
	return botInThread, webhooks.CapTranscript(rendered, maxThreadContextChars)
}
