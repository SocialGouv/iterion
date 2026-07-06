package server

import (
	"net/http"

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
	body, payloadHash, srcIP, ok := s.verifyWebhookHMACBody(w, r, cfg, "forgejo", forgejoSignatureHeader(r))
	if !ok {
		return
	}

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

	// "fj|" prefix keeps the idempotency key space disjoint from any other
	// provider for the same tenant in case ids get reused.
	s.handlePRForgeReview(ctx, w, r, cfg, body, payloadHash, srcIP, "fj|")
}
