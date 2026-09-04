package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// approveMinReplierRole is the commenter role floor for /revi approve when
// the webhook pins no MinReplierRole: a force-green of a required check is a
// maintainer affordance (docs/merge-gate.md), not one every write-permission
// collaborator holds. An operator's explicit cfg.MinReplierRole always wins.
const approveMinReplierRole = "maintainer"

// approveClaimStaleAfter bounds how long an `accepted` approve claim is
// trusted to be in flight. A writer that dies between the claim and its
// outcome leaves the row accepted; past this age the claim is reused — the
// status write is idempotent on the forge — instead of answering duplicate
// forever.
const approveClaimStaleAfter = 10 * time.Minute

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
// Forgejo PR comment: a trusted maintainer force-greens the merge gate on the
// PR head (the human-arbitration escape hatch for a finding the author
// disputes, backstopping the admin merge-queue bypass). It is NOT a
// re-review — no bot launches.
//
// Response discipline: a configuration refusal answers 200/filtered and a
// forge failure 200/launch_error, both with a reply on the PR naming the
// reason — never a 5xx, which forges answer by disabling the hook and every
// future launch, re-review and override with it.
//
// The status is written through the team connection's admin client — the
// resolution publish and the gate reconciler use, so a GitHub App
// integration mints its per-call installation token, which carries the
// statuses scope — or through the webhook's forge_token binding, for a
// hand-owned webhook with no connection row and for a connection whose
// client cannot serve (resolveApproveWritePath).
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
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: the review bot "+reviewer+" is not enabled on this webhook", payloadHash, srcIP)
		return
	}
	// Replay check, before any forge I/O. A prior delivery of this comment
	// that already approved (launched) or is mid-write (accepted, younger
	// than approveClaimStaleAfter) answers `duplicate`: a second status
	// write is pure waste. A prior launch_error is NOT terminal — the
	// forge's "Redeliver" is the operator's retry after a transient failure
	// — and neither is a claim whose writer died before recording an
	// outcome; both rows are reused by the claim below. Refusals are audited
	// under their own keys (recordTerminalWebhookDelivery) so a redelivery
	// after the operator fixes the setup re-evaluates.
	idemKey := approveIdempotencyKey(cfg, p)
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
				s.logger.Warn("webhooks: %s %s#%d /revi approve: reusing claim %s left accepted %s ago — its writer never recorded an outcome", provider, p.ProjectPath, p.IssueNumber, existing.ID, time.Since(existing.ReceivedAt).Round(time.Minute))
			}
			ex := existing
			priorRow = &ex
		}
	}
	// The PR author cannot approve their own PR — a self-approve is a
	// merge-queue bypass in another shape. Checked before the command gate
	// so the reply names the reason a reader will act on; re-checked below
	// when the head could only be loaded through the write path's client.
	pr, prLoaded, prErr := s.loadPRHeadForApprove(ctx, cfg, p)
	if prErr != nil {
		s.approveFailWithReply(ctx, w, cfg, meta, provider, p, forge.Connection{}, false, "resolve PR head: "+prErr.Error(), payloadHash, srcIP)
		return
	}
	if prLoaded && isSelfApprove(pr, p) {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, selfApproveReply(p), payloadHash, srcIP)
		return
	}
	// Authorize through the same PR-comment command gate as every other
	// /command (not the issue-author-trust gate): the commenter's live repo
	// role against a floor, plus the WhoAmI loop-guard that rejects the
	// review bot's own comment. The floor defaults to approveMinReplierRole
	// when the webhook pins none; an operator's explicit cfg.MinReplierRole
	// always wins. ROLE-only: the webhook's AuthorizedRepliers allowlist is
	// "who may talk back to the bot", not "who may force-green a required
	// check", so the gate sees it empty here.
	route := webhooks.CommandRoute{BotID: reviewer}
	if strings.TrimSpace(cfg.MinReplierRole) == "" {
		route.MinReplierRole = approveMinReplierRole
	}
	gateCfg := cfg
	gateCfg.AuthorizedRepliers = nil
	gate := s.webhookPRForgeCommandGate
	if gate == nil {
		gate = s.realWebhookPRForgeCommandGate
	}
	authorized, gateReason, aerr := gate(ctx, gateCfg, provider, p, route)
	if aerr != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "authz check: "+aerr.Error())
		httpError(w, http.StatusBadGateway, "authorization check failed")
		return
	}
	if !authorized {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: "+gateReason, payloadHash, srcIP)
		return
	}

	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: "+refusal, payloadHash, srcIP)
		return
	}
	host := hostOfURL(baseURL)
	if host == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: the payload URL has no usable forge host", payloadHash, srcIP)
		return
	}
	path, pathRefusal, resolveErr := s.resolveApproveWritePath(ctx, cfg, provider, reviewer, baseURL, host, p.ProjectPath)
	if resolveErr != nil {
		// Infra (connection unreadable, token refresh failed): a forge-side
		// failure, told to the maintainer through whichever identity did
		// resolve — never a 5xx.
		s.approveFailWithReply(ctx, w, cfg, meta, provider, p, path.conn, path.connOK, "resolve forge client: "+resolveErr.Error(), payloadHash, srcIP)
		return
	}
	if pathRefusal != "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: "+pathRefusal, payloadHash, srcIP)
		return
	}
	gc := path.gc
	if !prLoaded {
		loaded, err := gc.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
		if err != nil {
			s.approveFailWithReply(ctx, w, cfg, meta, provider, p, path.conn, path.connOK, "resolve PR head: "+err.Error(), payloadHash, srcIP)
			return
		}
		pr = loaded
		if isSelfApprove(pr, p) {
			s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, selfApproveReply(p), payloadHash, srcIP)
			return
		}
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: the forge returned no head sha for this PR", payloadHash, srcIP)
		return
	}
	gateCtx := s.resolveGateContext(cfg, reviewer)
	if gateCtx == "" {
		s.approveFilteredWithReply(ctx, w, cfg, meta, provider, p, "@"+p.AuthorLogin+" I cannot approve here: no merge-gate context is pinned on this repo (pin gate_context on the integration — see docs/merge-gate.md)", payloadHash, srcIP)
		return
	}

	// Claim the approve under its stable key BEFORE the forge write: the
	// store's unique constraint on the key is what keeps two replicas
	// handling the same redelivery from both writing the status. A prior
	// failed or stale row is reused (an Insert would collide with its own
	// key); a failed row keeps its received-at, a stale claim gets a fresh
	// one so a twin arriving during this retry reads it as in flight.
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
				s.approveFailWithReply(ctx, w, cfg, meta, provider, p, path.conn, path.connOK, "reset failed delivery: "+err.Error(), payloadHash, srcIP)
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
			s.approveFailWithReply(ctx, w, cfg, meta, provider, p, path.conn, path.connOK, "record delivery: "+err.Error(), payloadHash, srcIP)
			return
		}
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
		// The claim turns launch_error under its stable key: the replay
		// check reads that as retryable, never as a duplicate.
		why := "set commit status: " + err.Error()
		s.warnApproveDidNotLand(provider, p, why)
		claim.Status, claim.Error = webhooks.StatusLaunchError, why
		s.updateApproveDelivery(ctx, claim)
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunchError)
		s.postApproveReply(ctx, cfg, provider, p, path.conn, path.connOK, "@"+p.AuthorLogin+" I can't approve: "+why)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": why})
		return
	}
	if s.logger != nil {
		s.logger.Info("webhooks: %s %s#%d %s force-greened by @%s (%q) via %s", provider, p.ProjectPath, p.IssueNumber, gateCtx, p.AuthorLogin, reason, path.via)
	}
	now := time.Now().UTC()
	claim.Status, claim.Error, claim.LaunchedAt = webhooks.StatusLaunched, "", &now
	s.updateApproveDelivery(ctx, claim)
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunched)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": "revi-approved"})
}

// approveIdempotencyKey is the per-comment dedupe key of an approve: the same
// shape the command lane keys its launches on, so both replicas of one
// forge delivery collide on one row.
func approveIdempotencyKey(cfg webhooks.Config, p prforge.ParsedNote) string {
	return knowledge.ChecksumHex([]byte("approve|" + cfg.TenantID + "|" + cfg.ID + "|" + p.ProjectPath + "|" + p.SubjectID()))
}

// isSelfApprove reports whether the commenter is the PR's own author. An
// empty author (a provider that omits it) proves nothing and passes.
func isSelfApprove(pr forge.PullRef, p prforge.ParsedNote) bool {
	return strings.TrimSpace(pr.Author) != "" && strings.EqualFold(pr.Author, p.AuthorLogin)
}

func selfApproveReply(p prforge.ParsedNote) string {
	return "@" + p.AuthorLogin + " you cannot approve your own pull request — a maintainer must run /revi approve here"
}

// loadPRHeadForApprove resolves the PR head through the webhook's
// forge_token binding, when there is one, so the self-approve check can run
// before the command gate. loaded=false with no error means no token is
// bound; the write path's client resolves the head later in that case.
func (s *Server) loadPRHeadForApprove(ctx context.Context, cfg webhooks.Config, p prforge.ParsedNote) (pr forge.PullRef, loaded bool, err error) {
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		return forge.PullRef{}, false, nil
	}
	token, terr := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer)
	if terr != nil || token == "" {
		return forge.PullRef{}, false, nil
	}
	gc, ok := prforgeReplierClient(cfg.Provider, s.forgeHTTPClient(), baseURL, token).(forgeGateClient)
	if !ok {
		return forge.PullRef{}, false, nil
	}
	pr, err = gc.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
	if err != nil {
		return forge.PullRef{}, false, err
	}
	return pr, true, nil
}

// approveWritePath is the client the approval status is written through,
// plus the identity a reply about it rides on.
type approveWritePath struct {
	gc     forgeGateClient
	conn   forge.Connection
	connOK bool
	via    string
}

// resolveApproveWritePath picks the client the approval status is written
// through. Preferred: the team connection's admin client — the same
// resolution publish and the gate reconciler use, so an App integration
// mints its per-call installation token (which carries the statuses scope).
// The webhook's own forge_token binding serves when no connection row
// covers the repo (the hand-owned setup docs/webhooks.md describes) AND
// when the covering connection's client cannot serve — an App client mints
// lazily, so it is preflighted here rather than discovered by the status
// write. A non-empty refusal is a configuration miss with nothing to write
// through; an error is a forge-side failure with no other credential to
// fall back on.
func (s *Server) resolveApproveWritePath(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, reviewer, baseURL, host, projectPath string) (approveWritePath, string, error) {
	conn, connOK := s.forgeConnectionForPR(ctx, cfg.TenantID, "", host, projectPath)
	var connErr error
	if connOK {
		gc, err := s.gateClientFor(ctx, conn)
		if err == nil && gc != nil {
			err = preflightForgeClient(ctx, gc)
		}
		switch {
		case err != nil:
			connErr = err
			if s.logger != nil {
				s.logger.Warn("webhooks: /revi approve on %s/%s: connection %s covers the repo but its client cannot serve (%v) — writing through the webhook's forge_token binding instead", host, projectPath, conn.ID, err)
			}
		case gc == nil:
			return approveWritePath{conn: conn, connOK: true}, "provider " + string(conn.Provider) + " has no commit-status capability", nil
		default:
			return approveWritePath{gc: gc, conn: conn, connOK: true, via: "connection " + conn.ID}, "", nil
		}
	}
	token, err := s.resolveForgeToken(ctx, cfg, reviewer)
	if err != nil {
		return approveWritePath{conn: conn, connOK: connOK}, "", err
	}
	if token == "" {
		if connErr != nil {
			// The connection is the only credential this lane holds and it
			// cannot serve: a forge-side failure, told to the maintainer
			// through whichever identity still resolves.
			return approveWritePath{conn: conn, connOK: true}, "", connErr
		}
		return approveWritePath{}, "no team connection covers " + host + "/" + projectPath + " and this webhook has no forge_token binding — connect a forge integration for this repo, or bind forge_token on the webhook", nil
	}
	gc, ok := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token).(forgeGateClient)
	if !ok {
		return approveWritePath{}, "provider " + string(provider) + " has no commit-status capability", nil
	}
	via := "forge_token binding"
	if connErr != nil {
		via += " (connection " + conn.ID + " cannot serve: " + connErr.Error() + ")"
	} else if s.logger != nil {
		s.logger.Info("webhooks: /revi approve on %s/%s: no team connection covers this repo, writing through the webhook's forge_token binding", host, projectPath)
	}
	return approveWritePath{gc: gc, via: via}, "", nil
}

// approveFilteredWithReply is the configuration-refusal path: audit
// `filtered` under its own key (no forge write was attempted, and a
// redelivery after the operator fixes the setup must re-evaluate), tell the
// maintainer why on the PR, answer 200.
func (s *Server) approveFilteredWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, provider webhooks.Provider, p prforge.ParsedNote, replyBody, payloadHash, srcIP string) {
	if s.logger != nil {
		s.logger.Warn("webhooks: %s %s#%d /revi approve filtered: %s", provider, p.ProjectPath, p.IssueNumber, replyBody)
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, replyBody)
	s.postApproveReply(ctx, cfg, provider, p, forge.Connection{}, false, replyBody)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
}

// approveFailWithReply is the forge-failure path before the claim exists
// (PR head unresolvable, gate client unbuildable, audit store down): audit
// `launch_error` under its own key, tell the maintainer, answer 200 — never
// 502, which forges answer by disabling the hook.
func (s *Server) approveFailWithReply(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, provider webhooks.Provider, p prforge.ParsedNote, conn forge.Connection, connOK bool, why, payloadHash, srcIP string) {
	s.warnApproveDidNotLand(provider, p, why)
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, why)
	s.postApproveReply(ctx, cfg, provider, p, conn, connOK, "@"+p.AuthorLogin+" I can't approve: "+why)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError, "reason": why})
}

func (s *Server) warnApproveDidNotLand(provider webhooks.Provider, p prforge.ParsedNote, why string) {
	if s.logger != nil {
		s.logger.Warn("webhooks: %s %s#%d /revi approve by @%s did not land: %s", provider, p.ProjectPath, p.IssueNumber, p.AuthorLogin, why)
	}
}

// updateApproveDelivery flips the claim row to its terminal status. A failed
// write leaves the row `accepted`, which the replay check reads as a
// duplicate — so it is logged at Warn: that comment can no longer be
// redelivered without an operator noticing why.
func (s *Server) updateApproveDelivery(ctx context.Context, d webhooks.Delivery) {
	if s.webhookDeliveries == nil {
		return
	}
	if err := s.webhookDeliveries.Update(ctx, d); err != nil && s.logger != nil {
		s.logger.Warn("webhooks: approve delivery %s not updated to %s (row stays accepted, redeliveries read as duplicate): %v", d.ID, d.Status, err)
	}
}

// postApproveReply best-effort tells the maintainer on the PR why the
// approve did not land: through the resolved connection when there is one,
// else the team connection covering the repo; when the connection posts
// nothing — none covers the repo, or its client cannot serve — through the
// webhook's forge_token binding, the other identity this lane holds. The
// token is never sent to a payload host outside the allowlist. Silent on
// every miss: the refusal is already on the delivery audit and in the log,
// and a failed comment must not compound it.
func (s *Server) postApproveReply(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, conn forge.Connection, connOK bool, body string) {
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if !connOK && refusal == "" {
		if c, ok := s.forgeConnectionForPR(ctx, cfg.TenantID, "", hostOfURL(baseURL), p.ProjectPath); ok {
			conn, connOK = c, true
		}
	}
	if connOK && s.postApproveRejection(ctx, conn, p.ProjectPath, int(p.IssueNumber), body) == nil {
		return
	}
	if refusal != "" {
		return
	}
	token, err := s.resolveForgeToken(ctx, cfg, s.roleBots().Reviewer)
	if err != nil || token == "" {
		return
	}
	c, ok := prforgeReplierClient(provider, s.forgeHTTPClient(), baseURL, token).(forgeIssueCommenter)
	if !ok {
		return
	}
	if _, err := c.CommentIssue(ctx, p.ProjectPath, int(p.IssueNumber), body); err != nil && s.logger != nil {
		s.logger.Debug("webhooks: approve reply to %s#%d not posted: %v", p.ProjectPath, p.IssueNumber, err)
	}
}

// postApproveRejection posts why a /revi approve did not land on the PR the
// command sat on, through the connection the approve wrote (or tried to
// write) through. The error is for the caller's fallback only — the failure
// is already on the delivery audit; a comment failure on top of it must not
// compound the confusion, so it is logged at Debug and never surfaced.
func (s *Server) postApproveRejection(ctx context.Context, conn forge.Connection, repo string, number int, body string) error {
	commenter, err := s.issueCommenterFor(ctx, conn)
	if err == nil && commenter == nil {
		err = fmt.Errorf("provider %s has no comment capability", conn.Provider)
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("webhooks: approve-rejection reply to %s#%d not posted (no comment client for %s: %v)", repo, number, conn.Provider, err)
		}
		return err
	}
	if _, err := commenter.CommentIssue(ctx, repo, number, body); err != nil {
		if s.logger != nil {
			s.logger.Debug("webhooks: approve-rejection reply to %s#%d not posted: %v", repo, number, err)
		}
		return err
	}
	return nil
}
