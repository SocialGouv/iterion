package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/generic"
)

// registerGenericWebhookRoute wires the bot-agnostic JSON webhook
// endpoint behind webhookAuth. Token-mode by default — operators who
// run iterion behind something that already signs the body (a custom
// CI runner, an inter-service hook) can switch to hmac via the CRUD.
func (s *Server) registerGenericWebhookRoute() {
	s.mux.Handle("POST /api/webhooks/generic/{id}", s.webhookAuth(webhooks.ProviderGeneric, http.HandlerFunc(s.handleGenericWebhook)))
}

// handleGenericWebhook is the inbound handler for the generic webhook.
//
// Unlike forge-specific handlers, this one does NOT default a bot to
// review-pr — the generic endpoint is bot-agnostic, so a request that
// can't resolve a bot from (request body | config default | single-bot
// scope) is a 400 rather than a silent miss.
//
// Var precedence: request body → config LaunchVars override. The
// operator wins; a malicious caller can't escalate by renaming a var
// the operator has pinned (matches the forge handlers' contract).
func (s *Server) handleGenericWebhook(w http.ResponseWriter, r *http.Request) {
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

	// hmac-mode: middleware skipped the token check; we verify here
	// over the raw body before any side effect. Token-mode webhooks
	// were already verified by the middleware.
	if cfg.SignMode == webhooks.SignModeHMAC {
		if !webhooks.VerifyHMACSignature(s.sealer, cfg.ID, cfg.HMACSecretSealed, body, r.Header.Get("X-Iterion-Webhook-Signature")) {
			if s.logger != nil {
				s.logger.Warn("webhooks: generic bad HMAC for %s from %s", cfg.ID, s.clientIP(r))
			}
			httpError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
	}

	payloadHash := knowledge.ChecksumHex(body)
	srcIP := s.clientIP(r)

	req, err := generic.ParseRequest(body)
	if err != nil {
		meta := webhookEventMeta{Kind: "generic"}
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	meta := genericRequestMeta(req)

	if !genericMatchProject(cfg.ProjectAllowlist, req.ProjectPath) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, "")
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}

	// Bot resolution: caller-supplied → config default/single-bot →
	// 400. Generic is bot-agnostic by design.
	botID := req.Bot
	if botID == "" {
		botID = cfg.SelectBot()
	}
	if botID == "" {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "bot required (set request.bot or webhook default)")
		httpError(w, http.StatusBadRequest, "bot required (set request.bot or webhook default)")
		return
	}
	if !cfg.AllowsBot(botID) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "bot not permitted by webhook scope")
		httpError(w, http.StatusForbidden, "bot %q not permitted by this webhook", botID)
		return
	}

	// Idempotency: req.IdempotencyKey when present (operator-supplied
	// dedup), else sha256(body) so an exact retransmission still dedupes.
	dedup := req.IdempotencyKey
	if dedup == "" {
		sum := sha256.Sum256(body)
		dedup = hex.EncodeToString(sum[:])
	}
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("generic|%s|%s|%s", cfg.TenantID, cfg.ID, dedup)))

	// Var merge: body vars first, then the webhook's own layers (config wins,
	// operator overrides last — see applyWebhookVarLayers).
	vars := make(map[string]string, len(req.Vars)+len(cfg.LaunchVars))
	for k, v := range req.Vars {
		vars[k] = v
	}
	applyWebhookVarLayers(vars, cfg)

	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, botID, vars, req.RepoURL, req.RepoRef, payloadHash, srcIP)
}

// genericRequestMeta flattens a generic.Request into webhookEventMeta.
// The "subject id" embeds the operator-supplied idempotency key so an
// audit listing can correlate the request to the upstream event.
func genericRequestMeta(req generic.Request) webhookEventMeta {
	subject := req.IdempotencyKey
	if subject != "" {
		subject = "generic:" + subject
	}
	return webhookEventMeta{
		Kind:        "generic",
		Action:      "",
		ProjectPath: req.ProjectPath,
		SubjectID:   subject,
	}
}

// genericMatchProject mirrors the gitlab/github project matcher but is
// permissive when the request omits a project path (the operator who
// wired the allowlist accepts that contract — the generic endpoint is
// the only way to launch a project-less workflow this way). All other
// pattern semantics (trailing /*, bare *, exact) come from the canonical
// webhooks.MatchProject.
func genericMatchProject(allowlist []string, path string) bool {
	return path == "" || webhooks.MatchProject(allowlist, path)
}
