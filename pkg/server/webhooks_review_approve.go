package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// reviewGateContext is the commit-status check the /revi approve override
// force-greens. It matches the context Revi posts (the bot pins "revi/review").
// Living in the /revi command handler, this is intrinsically Revi-scoped —
// consistent with the existing distinguished-bot coupling in this webhook layer
// (defaultWebhookBotReviewPR et al., see CLAUDE.md known-debt).
const reviewGateContext = "revi/review"

// reviewApproveReason detects a `/revi approve [reason]` command and returns
// the trailing reason. ok=false for any other command (so the normal /revi
// re-review path is untouched).
func reviewApproveReason(cmd, args string) (reason string, ok bool) {
	if !strings.EqualFold(strings.TrimSpace(cmd), "revi") {
		return "", false
	}
	fields := strings.Fields(args)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "approve") {
		return "", false
	}
	// Everything after the "approve" token is the free-text reason.
	rest := strings.TrimSpace(args)
	rest = strings.TrimSpace(rest[len(fields[0]):])
	return rest, true
}

// handlePRForgeReviewApprove handles `/revi approve [reason]` on a GitHub /
// Forgejo PR comment: a trusted maintainer force-greens the revi/review merge
// gate on the PR head (the human-arbitration escape hatch for a finding the
// author disputes, backstopping the admin merge-queue bypass). It is NOT a
// re-review — no bot launches. Every benign refusal is a 200/filtered so the
// forge keeps the hook enabled; the status write goes through the bot's own
// forge token (the same credential the command gate uses).
func (s *Server) handlePRForgeReviewApprove(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, reason, payloadHash, srcIP string) {
	meta := prforgeNoteMeta(p)
	filtered := func(why string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, why)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}
	if !p.IsPullRequest {
		filtered("/revi approve only applies on a pull request")
		return
	}
	// The review bot must be permitted on this webhook — same admission the
	// normal command path applies before authorizing a /command.
	if !cfg.AllowsBot(defaultWebhookBotReviewPR) {
		filtered("bot " + defaultWebhookBotReviewPR + " not permitted by this webhook")
		return
	}
	// Authorize EXACTLY like every other PR-comment command (not the
	// issue-author-trust gate): realWebhookPRForgeCommandGate checks the
	// commenter's live repo role against MinReplierRole (or AuthorizedRepliers),
	// AND rejects the review bot's own comment via a WhoAmI loop-guard so a
	// status can't self-approve. A synthetic review-pr route carries the token
	// binding + role threshold.
	route := webhooks.CommandRoute{BotID: defaultWebhookBotReviewPR}
	gate := s.webhookPRForgeCommandGate
	if gate == nil {
		gate = s.realWebhookPRForgeCommandGate
	}
	authorized, reason2, aerr := gate(ctx, cfg, provider, p, route)
	if aerr != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		filtered("@" + p.AuthorLogin + " not authorized to /revi approve (" + reason2 + ")")
		return
	}

	// Authorized: resolve the client to post the approval status.
	token, terr := s.resolveForgeToken(ctx, cfg, defaultWebhookBotReviewPR)
	if terr != nil || token == "" {
		filtered("no forge token to post the approval status (configure a forge_token binding)")
		return
	}
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		filtered(refusal)
		return
	}
	client := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
	gc, ok := client.(forgeGateClient)
	if !ok {
		filtered("provider " + string(provider) + " has no commit-status capability")
		return
	}
	pr, err := gc.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "resolve PR head: "+err.Error())
		httpError(w, http.StatusBadGateway, "could not resolve the PR head")
		return
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		filtered("forge returned no head sha for the PR")
		return
	}
	desc := "approved by @" + p.AuthorLogin
	if reason != "" {
		desc += ": " + reason
	}
	if err := gc.SetCommitStatus(ctx, p.ProjectPath, pr.HeadSHA, forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     reviewGateContext,
		Description: desc,
		TargetURL:   p.CommentURL,
	}); err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "set commit status: "+err.Error())
		httpError(w, http.StatusBadGateway, "could not post the approval status")
		return
	}
	if s.logger != nil {
		s.logger.Info("webhooks: %s %s#%d revi/review force-greened by @%s (%q)", provider, p.ProjectPath, p.IssueNumber, p.AuthorLogin, reason)
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunched, payloadHash, srcIP, "revi/review approved by @"+p.AuthorLogin)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved"})
}
