package server

import (
	"fmt"
	"io"
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}

	// Signature gate FIRST — never write an audit row or call gateLaunch
	// for an unauthenticated request (would leak quota signal to a
	// random poker on the open route).
	if !webhooks.VerifyHMACSignature(s.sealer, cfg.ID, cfg.HMACSecretSealed, body, r.Header.Get("X-Hub-Signature-256")) {
		if s.logger != nil {
			s.logger.Warn("webhooks: github bad HMAC for %s from %s", cfg.ID, s.clientIP(r))
		}
		httpError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	payloadHash := knowledge.ChecksumHex(body)
	srcIP := s.clientIP(r)

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case prforge.EventHeaderIssueComment:
		// Universal slash-command path: /featurly, /seki… on a PR or issue
		// comment. Routes through the command registry to its bot.
		s.handlePRForgeComment(ctx, w, r, cfg, webhooks.ProviderGitHub, body, payloadHash, srcIP)
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

	p, err := prforge.ParsePullRequest(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: "pull_request"}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid pull_request payload")
		return
	}
	meta := prforgePRMeta(p)

	if !p.IsReviewable() ||
		!webhooks.MatchEvent(cfg.EventAllowlist, "pull_request", "pull_request") ||
		!webhooks.MatchProject(cfg.ProjectAllowlist, p.ProjectPath) ||
		!webhooks.MatchAuthor(cfg.AuthorAllowlist, p.SenderLogin) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	botID, ok := s.resolveReviewBot(ctx, w, cfg, meta, payloadHash, srcIP)
	if !ok {
		return
	}

	// Idempotency: one launch per (tenant, webhook, repo, PR#, head sha).
	// "gh|" prefix keeps the key space disjoint from any other provider
	// for the same tenant in case ids get reused.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("gh|%s|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA)))

	vars := reviewPRVars(p.PRURL, p.TargetBranch, strings.TrimSpace(p.Title+"\n\n"+p.Description), cfg.LaunchVars, map[string]string{"pr_author": p.SenderLogin})

	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, botID, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
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
