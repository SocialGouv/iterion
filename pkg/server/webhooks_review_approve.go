package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// approveMinReplierRole is the DEFAULT commenter role floor for /revi
// approve when the webhook config pins no MinReplierRole. Set to
// maintainer because docs/merge-gate.md documents this override as a
// "maintainer" affordance — the same role a merge-queue admin bypass
// requires. An operator's explicit cfg.MinReplierRole always wins:
// never silently replace an explicit choice (CLAUDE.md philosophy §1).
const approveMinReplierRole = "maintainer"

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
	entries, err := s.effectiveEntriesWithSchema()
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
	reviewer := s.roleBots().Reviewer
	if !cfg.AllowsBot(reviewer) {
		filtered("bot " + reviewer + " not permitted by this webhook")
		return
	}
	// #662 A6 idempotency: a redelivered comment (forge "Redeliver", or a
	// retry after our own 5xx) must not run the whole approve flow again.
	// The key is deterministic on (approve, tenant, webhook, project,
	// subject) so both replicas of a delivery collide on the same row.
	idemKey := knowledge.ChecksumHex([]byte("approve|" + cfg.TenantID + "|" + cfg.ID + "|" + p.ProjectPath + "|" + p.SubjectID()))
	if s.webhookDeliveries != nil {
		if prior, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey); err == nil && prior.ID != "" {
			// A prior delivery already ran this approve — never re-write to
			// the forge (a duplicate approve is 100% wasted, and a duplicate
			// PR reply is noise).
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved-duplicate", "delivery_id": prior.ID})
			return
		}
	}
	// #662 A1: author cannot approve their own PR (docs/merge-gate.md
	// documents this as a "maintainer" affordance — a self-approve is a
	// merge-queue bypass in another shape). Refused BEFORE the command gate
	// so the PR reply names the reason a reader will act on.
	pr, prLoaded, prErr := s.loadPRHeadForApprove(ctx, cfg, p)
	if prErr != nil {
		// If we cannot even resolve the PR, treat it as a forge write error:
		// tell the maintainer, keep the hook alive.
		s.approveFailWithReply(ctx, w, cfg, meta, provider, p, "resolve PR head: "+prErr.Error(), idemKey, payloadHash, srcIP)
		return
	}
	if prLoaded && strings.TrimSpace(pr.Author) != "" && strings.EqualFold(pr.Author, p.AuthorLogin) {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" you cannot approve your own pull request — a maintainer must run /revi approve here",
			idemKey, payloadHash, srcIP)
		return
	}
	// Authorize EXACTLY like every other PR-comment command (not the
	// issue-author-trust gate): the gate checks the commenter's live repo
	// role against a floor, AND rejects the review bot's own comment via a
	// WhoAmI loop-guard so a status can't self-approve. Default the floor
	// to approveMinReplierRole ("maintainer") when the webhook pins none,
	// per A1; an operator's explicit cfg.MinReplierRole ALWAYS wins.
	route := webhooks.CommandRoute{BotID: reviewer}
	if strings.TrimSpace(cfg.MinReplierRole) == "" {
		route.MinReplierRole = approveMinReplierRole
	}
	gate := s.webhookPRForgeCommandGate
	if gate == nil {
		gate = s.realWebhookPRForgeCommandGate
	}
	authorized, reason2, aerr := gate(ctx, cfg, provider, p, route)
	if aerr != nil {
		s.recordTerminalWebhookDeliveryWithKey(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "authz check: "+aerr.Error(), idemKey)
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: "+reason2,
			idemKey, payloadHash, srcIP)
		return
	}

	// Authorized: pick the write path. Preferred: the SAME resolution
	// publish + the reconciler use — the team connection's admin client,
	// so a `github_app` integration mints its per-call installation token
	// (which HAS `statuses` scope). #662 A2 fallback: a webhook that has
	// only a `forge_token` binding (docs/webhooks.md documents this manual
	// path as supported) has no forge.Connection row. Fall back to the
	// old resolveForgeToken + prforgeReplierClient path there so the
	// hand-owned setup keeps working; log which path served.
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: "+refusal,
			idemKey, payloadHash, srcIP)
		return
	}
	host := hostOfURL(baseURL)
	if host == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: the payload URL has no usable forge host",
			idemKey, payloadHash, srcIP)
		return
	}
	gc, conn, connOK, resolveErr := s.resolveApproveGateClient(ctx, cfg, provider, reviewer, baseURL, host, p.ProjectPath)
	if resolveErr != nil {
		// #662 A3: reroute through the fail-with-reply path (was 502 before,
		// which is precisely the hook-disabling 5xx class this ticket asked
		// to remove). Pass the connection through when it resolved, so the
		// reply rides the App identity that failed rather than the token
		// binding's.
		s.approveFailWithReplyThroughConn(ctx, w, cfg, meta, provider, p, conn, connOK, "resolve forge client: "+resolveErr.Error(), idemKey, payloadHash, srcIP)
		return
	}
	if gc == nil {
		provStr := string(provider)
		if connOK {
			provStr = string(conn.Provider)
		}
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: provider "+provStr+" has no commit-status capability",
			idemKey, payloadHash, srcIP)
		return
	}

	// Re-resolve the PR head via the gate client if the pre-check couldn't.
	if !prLoaded {
		p2, err := gc.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
		if err != nil {
			s.approveFailWithReply(ctx, w, cfg, meta, provider, p, "resolve PR head: "+err.Error(), idemKey, payloadHash, srcIP)
			return
		}
		pr = p2
		if strings.TrimSpace(pr.Author) != "" && strings.EqualFold(pr.Author, p.AuthorLogin) {
			s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
				"@"+p.AuthorLogin+" you cannot approve your own pull request — a maintainer must run /revi approve here",
				idemKey, payloadHash, srcIP)
			return
		}
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: the forge returned no head sha for this PR",
			idemKey, payloadHash, srcIP)
		return
	}
	gateCtx := s.resolveGateContext(cfg, reviewer)
	if gateCtx == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p,
			"@"+p.AuthorLogin+" I cannot approve here: no merge-gate context is pinned on this repo (pin gate_context on the integration — see docs/merge-gate.md)",
			idemKey, payloadHash, srcIP)
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
		s.approveFailWithReplyThroughConn(ctx, w, cfg, meta, provider, p, conn, connOK, "set commit status: "+err.Error(), idemKey, payloadHash, srcIP)
		return
	}
	if s.logger != nil {
		via := "forge_token binding"
		if connOK {
			via = "connection " + conn.ID
		}
		s.logger.Info("webhooks: %s %s#%d %s force-greened by @%s (%q) via %s", provider, p.ProjectPath, p.IssueNumber, gateCtx, p.AuthorLogin, reason, via)
	}
	s.recordTerminalWebhookDeliveryWithKey(ctx, cfg, meta, webhooks.StatusLaunched, payloadHash, srcIP, gateCtx+" approved by @"+p.AuthorLogin, idemKey)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved"})
}

// loadPRHeadForApprove tries to resolve the PR head early so the self-approve
// check runs BEFORE the command gate. Uses whatever path is easiest to try —
// the forge_token binding via the replier client — and returns loaded=false
// with no error when nothing is set up (the connection path will retry it).
func (s *Server) loadPRHeadForApprove(ctx context.Context, cfg webhooks.Config, p prforge.ParsedNote) (pr forge.PullRef, loaded bool, err error) {
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		return forge.PullRef{}, false, nil
	}
	token, terr := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer)
	if terr != nil || token == "" {
		return forge.PullRef{}, false, nil
	}
	client := prforgeReplierClient(cfg.Provider, s.forgeHTTPClient(), baseURL, token)
	gc, ok := client.(forgeGateClient)
	if !ok {
		return forge.PullRef{}, false, nil
	}
	pr, err = gc.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
	if err != nil {
		return forge.PullRef{}, false, err
	}
	return pr, true, nil
}

// resolveApproveGateClient picks the write path for the approval status.
// Preferred: the team connection's admin client (App integration → per-call
// installation token). Fallback (#662 A2): a webhook with a forge_token
// binding but no connection row (docs/webhooks.md manual setup). Returns
// (gc, conn, connOK, err); gc==nil AND no err means "no client capability".
func (s *Server) resolveApproveGateClient(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, reviewer, baseURL, host, projectPath string) (forgeGateClient, forge.Connection, bool, error) {
	conn, connOK := s.forgeConnectionForPR(ctx, cfg.TenantID, "", host, projectPath)
	if connOK {
		gc, err := s.gateClientFor(ctx, conn)
		if err != nil {
			return nil, conn, true, err
		}
		return gc, conn, true, nil
	}
	// No connection row → try the forge_token binding path (the pre-fix
	// working setup for hand-owned webhooks — #662 A2).
	token, terr := s.resolveForgeToken(ctx, cfg, reviewer)
	if terr != nil {
		return nil, forge.Connection{}, false, terr
	}
	if token == "" {
		return nil, forge.Connection{}, false, nil
	}
	client := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
	gc, ok := client.(forgeGateClient)
	if !ok {
		return nil, forge.Connection{}, false, nil
	}
	if s.logger != nil {
		s.logger.Info("webhooks: /revi approve on %s/%s: no team connection covers this repo, using the webhook's forge_token binding (hand-owned setup)", host, projectPath)
	}
	return gc, forge.Connection{}, false, nil
}

// approveFilteredWithReply is the config-shaped refusal path: record
// `filtered` (not launch_error — the setup is wrong, no forge write was
// attempted), best-effort tell the maintainer why on the PR, answer 200.
// #662 A4: every previously-silent refusal now names its reason on the PR.
func (s *Server) approveFilteredWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, provider webhooks.Provider, p prforge.ParsedNote, replyBody, idemKey, payloadHash, srcIP string) {
	if s.logger != nil {
		s.logger.Warn("webhooks: %s %s#%d /revi approve filtered: %s", provider, p.ProjectPath, p.IssueNumber, replyBody)
	}
	s.recordTerminalWebhookDeliveryWithKey(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, replyBody, idemKey)
	// Best-effort reply. Try connection first; fall through to the
	// forge_token binding for the A2 hand-owned setup.
	baseURL, _ := prforgeBaseURL(cfg, p)
	host := hostOfURL(baseURL)
	if conn, ok := s.forgeConnectionForPR(ctx, cfg.TenantID, "", host, p.ProjectPath); ok {
		s.postApproveRejection(ctx, conn, p.ProjectPath, int(p.IssueNumber), replyBody)
	} else if token, err := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer); err == nil && token != "" {
		client := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
		if c, ok := client.(forgeIssueCommenter); ok {
			if _, err := c.CommentIssue(ctx, p.ProjectPath, int(p.IssueNumber), replyBody); err != nil && s.logger != nil {
				s.logger.Debug("webhooks: approve-filtered reply to %s#%d not posted: %v", p.ProjectPath, p.IssueNumber, err)
			}
		}
	}
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
}

// approveFailWithReply is the forge-error path: record `launch_error`, tell
// the maintainer, answer 200 (never 502 — forges auto-disable on 5xx). Used
// when the forge write itself failed (scope refusal, transient outage) or
// when the gate client couldn't be built.
func (s *Server) approveFailWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, provider webhooks.Provider, p prforge.ParsedNote, why, idemKey, payloadHash, srcIP string) {
	// Same as WithReplyThroughConn but we haven't picked a conn yet.
	s.approveFailWithReplyThroughConn(ctx, w, cfg, meta, provider, p, forge.Connection{}, false, why, idemKey, payloadHash, srcIP)
}

// approveFailWithReplyThroughConn is the connection-aware variant: uses the
// already-resolved connection for the reply when available, else falls back
// to the forge_token binding.
func (s *Server) approveFailWithReplyThroughConn(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, provider webhooks.Provider, p prforge.ParsedNote, conn forge.Connection, connOK bool, why, idemKey, payloadHash, srcIP string) {
	if s.logger != nil {
		s.logger.Warn("webhooks: %s %s#%d /revi approve by @%s did not land: %s", provider, p.ProjectPath, p.IssueNumber, p.AuthorLogin, why)
	}
	s.recordTerminalWebhookDeliveryWithKey(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, why, idemKey)
	body := "@" + p.AuthorLogin + " I can't approve: " + why
	if connOK {
		s.postApproveRejection(ctx, conn, p.ProjectPath, int(p.IssueNumber), body)
	} else if token, err := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer); err == nil && token != "" {
		baseURL, _ := prforgeBaseURL(cfg, p)
		client := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token)
		if c, ok := client.(forgeIssueCommenter); ok {
			if _, err := c.CommentIssue(ctx, p.ProjectPath, int(p.IssueNumber), body); err != nil && s.logger != nil {
				s.logger.Debug("webhooks: approve-fail reply to %s#%d not posted: %v", p.ProjectPath, p.IssueNumber, err)
			}
		}
	}
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": why})
}

// recordTerminalWebhookDeliveryWithKey stamps a stable idempotency key
// (rather than a fresh uuid per call) so a retry lands on the SAME row.
// The delivery-store Insert honours the unique constraint on
// (tenant_id, idempotency_key). Behaves like the current recorder when the
// deliveries store lacks that hook.
func (s *Server) recordTerminalWebhookDeliveryWithKey(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta, status, payloadHash, srcIP, reason, idemKey string) {
	// The default recorder mints a uuid; we shadow it by pre-inserting the
	// row with our stable key. If webhookDeliveries is nil, fall back to
	// the recorder so nothing regresses.
	if s.webhookDeliveries != nil && idemKey != "" {
		_ = s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID:             idemKey,
			TenantID:       cfg.TenantID,
			WebhookID:      cfg.ID,
			IdempotencyKey: idemKey,
			Status:         status,
			EventKind:      meta.Kind,
			ProjectPath:    meta.ProjectPath,
			SubjectID:      meta.SubjectID,
			PayloadHash:    payloadHash,
			SourceIP:       srcIP,
			Error:          reason,
			ReceivedAt:     time.Now().UTC(),
		})
		return
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, status, payloadHash, srcIP, reason)
}

// postApproveRejection best-effort posts why a /revi approve did not land on
// the PR the command sat on. Uses the SAME connection the approve tried to
// write through (already resolved by the caller). Silent on every miss — the
// approve failure is already recorded on the delivery audit; a comment
// failure on top of it must not compound the confusion.
func (s *Server) postApproveRejection(ctx context.Context, conn forge.Connection, repo string, number int, body string) {
	commenter, err := s.issueCommenterFor(ctx, conn)
	if err != nil || commenter == nil {
		if s.logger != nil {
			s.logger.Debug("webhooks: approve-rejection reply to %s#%d not posted (no comment client for %s: %v)", repo, number, conn.Provider, err)
		}
		return
	}
	if _, err := commenter.CommentIssue(ctx, repo, number, body); err != nil && s.logger != nil {
		s.logger.Debug("webhooks: approve-rejection reply to %s#%d not posted: %v", repo, number, err)
	}
}
