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

// registerForgejoWebhookRoute wires the inbound Forgejo/Gitea delivery
// endpoint behind webhookAuth. Forgejo + Gitea sign the body with HMAC,
// so the middleware admits the call and this handler is responsible for
// the signature gate.
func (s *Server) registerForgejoWebhookRoute() {
	s.mux.Handle("POST /api/webhooks/forgejo/{id}", s.webhookAuth(webhooks.ProviderForgejo, http.HandlerFunc(s.handleForgejoWebhook)))
}

// forgejoSignatureHeader returns the presented HMAC value, preferring
// X-Forgejo-Signature (current spelling) but falling back to
// X-Gitea-Signature (older / Gitea-compatible deployments). Both are
// raw hex digests (NO "sha256=" prefix); webhooks.VerifyHMACSignature
// tolerates the prefix anyway so we don't have to special-case it.
func forgejoSignatureHeader(r *http.Request) string {
	if v := r.Header.Get("X-Forgejo-Signature"); v != "" {
		return v
	}
	return r.Header.Get("X-Gitea-Signature")
}

// forgejoEventHeader returns the event-kind value, accepting either
// X-Forgejo-Event or X-Gitea-Event (Forgejo's compatibility header).
func forgejoEventHeader(r *http.Request) string {
	if v := r.Header.Get("X-Forgejo-Event"); v != "" {
		return v
	}
	return r.Header.Get("X-Gitea-Event")
}

// handleForgejoWebhook is the inbound handler for both Forgejo and
// Gitea (one wire shape, two header names). Mirrors the GitHub flow:
// signature gate FIRST, then event-kind filter, then parse → filter →
// bot select → admission → launch.
func (s *Server) handleForgejoWebhook(w http.ResponseWriter, r *http.Request) {
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

	if !webhooks.VerifyHMACSignature(s.sealer, cfg.ID, cfg.HMACSecretSealed, body, forgejoSignatureHeader(r)) {
		if s.logger != nil {
			s.logger.Warn("webhooks: forgejo bad HMAC for %s from %s", cfg.ID, s.clientIP(r))
		}
		httpError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	payloadHash := knowledge.ChecksumHex(body)
	srcIP := s.clientIP(r)

	event := forgejoEventHeader(r)
	switch event {
	case prforge.EventHeaderIssueComment:
		// Universal slash-command path: /featurly, /seki… on a PR or issue
		// comment. Routes through the command registry to its bot.
		s.handlePRForgeComment(ctx, w, r, cfg, webhooks.ProviderForgejo, body, payloadHash, srcIP)
		return
	case prforge.EventHeaderPullRequest:
		// fall through to the PR auto-review path below.
	default:
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: event}, webhooks.StatusFiltered, payloadHash, srcIP, "unsupported event")
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

	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("fj|%s|%s|%s|%d|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.PRNumber, p.HeadSHA)))

	vars := reviewPRVars(p.PRURL, p.TargetBranch, strings.TrimSpace(p.Title+"\n\n"+p.Description), cfg.LaunchVars, map[string]string{"pr_author": p.SenderLogin})

	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, botID, vars, p.CloneURL, p.SourceBranch, payloadHash, srcIP)
}
