package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// gateContextVar is the var every gating bot exposes to name the commit-status
// context it posts under, and that a repo pins — to ONE shared value — so a
// single required check can span several bots (docs/merge-gate.md).
const gateContextVar = "gate_context"

// resolveGateContext resolves the commit-status context an override must
// force-green, in the same precedence the launch lanes apply to the var itself:
// the repo's pin first, then the manifest union, then the gating bot's own
// declared default.
//
// Reading it — rather than assuming a literal — is what makes the override
// green the check the repo actually requires. A repo pinning `iterion/review`
// (the documented setup, and the only one where a required check can span two
// bots) was getting a green `revi/review` instead: a status nothing required,
// leaving the real gate untouched while reporting success.
func (s *Server) resolveGateContext(cfg webhooks.Config, botID string) string {
	if v := strings.TrimSpace(cfg.OperatorLaunchVars[gateContextVar]); v != "" {
		return v
	}
	if v := strings.TrimSpace(cfg.LaunchVars[gateContextVar]); v != "" {
		return v
	}
	return strings.TrimSpace(s.botVarDefault(botID, gateContextVar))
}

// botVarDefault reads a bot's declared default for one workflow var.
func (s *Server) botVarDefault(botID, name string) string {
	entries, err := botregistry.ListWithSchema(s.botListOptions())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Name != botID || e.Vars == nil {
			continue
		}
		for _, f := range e.Vars.Fields {
			if f.Name == name && f.Default != nil {
				return f.Default.StrVal
			}
		}
	}
	return ""
}

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
	gateCtx := s.resolveGateContext(cfg, defaultWebhookBotReviewPR)
	if gateCtx == "" {
		filtered("no merge-gate context is pinned on this repo, so there is nothing to approve (pin gate_context on the integration — see docs/merge-gate.md)")
		return
	}
	desc := "approved by @" + p.AuthorLogin
	if reason != "" {
		desc += ": " + reason
	}
	if err := gc.SetCommitStatus(ctx, p.ProjectPath, pr.HeadSHA, forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     gateCtx,
		Description: desc,
		TargetURL:   p.CommentURL,
	}); err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "set commit status: "+err.Error())
		httpError(w, http.StatusBadGateway, "could not post the approval status")
		return
	}
	if s.logger != nil {
		s.logger.Info("webhooks: %s %s#%d %s force-greened by @%s (%q)", provider, p.ProjectPath, p.IssueNumber, gateCtx, p.AuthorLogin, reason)
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunched, payloadHash, srcIP, gateCtx+" approved by @"+p.AuthorLogin)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved"})
}
