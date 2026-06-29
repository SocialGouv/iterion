package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// maxWebhookBodyBytes caps the inbound payload every provider handler
// reads. The cap is generous (5 MiB) — forge events run smaller, but
// we'd rather a forge that mis-bundles fixtures see a 400 than have us
// OOM on a malformed gigabyte of JSON.
const maxWebhookBodyBytes = 5 << 20

// defaultWebhookBotReviewPR is the bot iterion auto-selects when a
// review-PR-shaped delivery (GitLab MR open/reopen, GitLab Note /revi,
// GitHub PR open, Forgejo PR open) lands on a wildcard webhook with no
// explicit DefaultBotID. Pinning it lets us ship those routes with
// zero-config webhooks. The generic webhook deliberately does NOT use
// this default — it's bot-agnostic by design.
const defaultWebhookBotReviewPR = "review-pr"

// defaultWebhookBotReviConverse is the conversational sibling iterion
// routes to when a `/revi <question>` note carries non-empty args (a
// follow-up question, not a re-review request). See
// docs/forge-conversations.md §A5. When this bot isn't permitted by
// the webhook scope OR isn't resolvable on disk (older deploy without
// the bundle), the handler gracefully falls back to the re-review
// path with the args ignored — matching today's behaviour.
const defaultWebhookBotReviConverse = "revi-converse"

// webhookEventMeta is the provider-agnostic carrier of "what happened
// upstream" the common helpers consume. Every field is optional: a
// provider that doesn't have e.g. a project path leaves it empty and
// the delivery row simply omits it.
type webhookEventMeta struct {
	Kind         string // "merge_request" | "pull_request" | "note" | "generic"
	Action       string // "open" | "reopen" | "comment" | …
	ProjectPath  string // "owner/repo" or equivalent
	SubjectID    string // "mr:7" / "pr:42" / "note:99" — stable per-event id
	SubjectURL   string // the subject's own web URL/ref (the issue/MR the comment is on) — back-linked as source_issue_ref for opens_mr commands
	SubjectSHA   string // head SHA, when known
	SenderHandle string // username for audit (logged only, never in delivery audit row v1)
}

// reviewPRVars composes the launch-vars map every forge-specific
// review-PR path produces: the canonical {pr_url, base_ref, scope_notes,
// post_to_board:"false", pr_review_mode:"summary"} base, an optional
// per-handler `extra` overlay (the Note handler injects "re_review":
// "true"), then the operator's `launchVars` LAST so the per-webhook
// pin always wins. `extra` may be nil; `launchVars` may be nil.
func reviewPRVars(prURL, baseRef, scopeNotes string, launchVars map[string]string, extra map[string]string) map[string]string {
	vars := map[string]string{
		"pr_url":         prURL,
		"base_ref":       baseRef,
		"scope_notes":    scopeNotes,
		"post_to_board":  "false",
		"pr_review_mode": "summary",
	}
	for k, v := range extra {
		vars[k] = v
	}
	for k, v := range launchVars {
		vars[k] = v
	}
	return vars
}

// resolveReviewBot picks the bot id for a forge-specific review-PR
// delivery: the webhook's SelectBot() result, falling back to the
// defaultWebhookBotReviewPR constant when the operator didn't pin one.
// The chosen bot is then validated against AllowsBot; a denied bot
// writes a terminal "invalid" delivery + 403 and ok=false (the caller
// must return immediately).
//
// Returned ok=false means the response was already written; the caller
// must not write a second response.
func (s *Server) resolveReviewBot(
	ctx context.Context,
	w http.ResponseWriter,
	cfg webhooks.Config,
	meta webhookEventMeta,
	payloadHash, srcIP string,
) (string, bool) {
	botID := cfg.SelectBot()
	if botID == "" {
		botID = defaultWebhookBotReviewPR
	}
	if !cfg.AllowsBot(botID) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusInvalid, payloadHash, srcIP, "bot not permitted by webhook scope")
		httpError(w, http.StatusForbidden, "bot %q not permitted by this webhook", botID)
		return "", false
	}
	return botID, true
}

// newWebhookDelivery builds the common fields of a delivery audit row.
// Provider handlers layer the idempotency key + outcome-specific fields
// (BotID, RunID, Error) on top.
//
// `status` is the initial status: terminal handlers pass StatusInvalid /
// StatusFiltered; the happy path passes StatusAccepted and updates the
// row to StatusLaunched once the launch returns.
func newWebhookDelivery(cfg webhooks.Config, meta webhookEventMeta, status, payloadHash, srcIP string) webhooks.Delivery {
	return webhooks.Delivery{
		ID:          uuid.NewString(),
		TenantID:    cfg.TenantID,
		WebhookID:   cfg.ID,
		Provider:    cfg.Provider,
		EventKind:   meta.Kind,
		EventAction: meta.Action,
		ProjectPath: meta.ProjectPath,
		SubjectID:   meta.SubjectID,
		SubjectSHA:  meta.SubjectSHA,
		PayloadHash: payloadHash,
		Status:      status,
		SourceIP:    srcIP,
		ReceivedAt:  time.Now().UTC(),
	}
}

// markWebhookOutcome bumps the per-provider delivery counter. The
// status label space is the small fixed Delivery status enum — no
// tenant label (cardinality discipline; Mongo counters are billing).
func (s *Server) markWebhookOutcome(provider webhooks.Provider, status string) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.WebhookDeliveriesTotal.WithLabelValues(string(provider), status).Inc()
	}
}

// recordTerminalWebhookDelivery inserts a non-launched audit row with a
// uuid idempotency key — terminal rows must NEVER collide with the
// dedup key (otherwise a real subsequent event under that key would
// look like a replay). Best-effort: an audit-store error doesn't fail
// the inbound request.
func (s *Server) recordTerminalWebhookDelivery(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta, status, payloadHash, srcIP, errMsg string) {
	s.markWebhookOutcome(cfg.Provider, status)
	if s.webhookDeliveries == nil {
		return
	}
	d := newWebhookDelivery(cfg, meta, status, payloadHash, srcIP)
	d.IdempotencyKey = uuid.NewString()
	d.Error = errMsg
	_ = s.webhookDeliveries.Insert(ctx, d)
}

// insertAndLaunchWebhook is the shared idempotency + launch + delivery
// update + response-writing tail every provider handler runs once it
// has resolved (cfg, meta, idemKey, botID, vars, repoURL, repoRef).
//
// Flow:
//  1. gateLaunch (per-org run quota / cost cap / concurrency) — denial
//     records a launch_error delivery and writes the standard denial
//     response, returning early.
//  2. Insert the delivery row keyed by idemKey. A duplicate idempotency
//     key writes a 200 replay response keyed on the existing row's
//     run_id and returns early.
//  3. Hand off to s.webhookLaunchBot (test seam) or its real
//     counterpart. A launch failure records launch_error and writes
//     502; success updates the row to StatusLaunched and writes 202.
//
// Provider handlers stay thin and DRY by funnelling everything through
// this single function. Behaviour is exactly the same as the GitLab
// handler shipped on main before this refactor — see the original
// commit for the contract.
func (s *Server) insertAndLaunchWebhook(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	cfg webhooks.Config,
	meta webhookEventMeta,
	idemKey string,
	botID string,
	vars map[string]string,
	repoURL string,
	repoRef string,
	payloadHash string,
	srcIP string,
) {
	// 1. Run-launch admission. Checked BEFORE the idempotency insert so
	// a denied event still writes a terminal row (under a random key)
	// and a later forge retry can launch once the quota resets.
	if d := s.gateLaunch(ctx); d != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, d.reason)
		s.writeLaunchDenial(w, r, d)
		return
	}

	// 2. Idempotency insert.
	delivery := newWebhookDelivery(cfg, meta, webhooks.StatusAccepted, payloadHash, srcIP)
	delivery.IdempotencyKey = idemKey
	delivery.BotID = botID
	if s.webhookDeliveries != nil {
		if err := s.webhookDeliveries.Insert(ctx, delivery); err != nil {
			if errors.Is(err, webhooks.ErrDuplicate) {
				// Read back the prior delivery so the duplicate 200
				// echoes its run_id/delivery_id. A failed read would
				// otherwise emit a misleading 200 with empty IDs —
				// surface it as a 500 instead.
				existing, gerr := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey)
				if gerr != nil {
					httpError(w, http.StatusInternalServerError, "lookup duplicate delivery: %v", gerr)
					return
				}
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				writeJSONStatus(w, http.StatusOK, map[string]string{
					"status": webhooks.StatusDuplicate, "run_id": existing.RunID, "delivery_id": existing.ID,
				})
				return
			}
			httpError(w, http.StatusInternalServerError, "record delivery: %v", err)
			return
		}
	}

	// 3. Launch.
	launch := s.webhookLaunchBot
	if launch == nil {
		launch = s.realWebhookLaunchBot
	}
	// meta.ProjectPath is the forge slug already parsed by the provider
	// handler — thread it onto the launch so the run is filterable by
	// repository in the studio.
	runID, lerr := launch(ctx, botID, vars, repoURL, repoRef, meta.ProjectPath, cfg.KeyOverrides, cfg.SecretOverrides)
	if lerr != nil {
		delivery.Status = webhooks.StatusLaunchError
		delivery.Error = lerr.Error()
		s.updateWebhookDelivery(ctx, delivery)
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunchError)
		httpError(w, http.StatusBadGateway, "launch failed: %v", lerr)
		return
	}
	launchedAt := time.Now().UTC()
	delivery.Status = webhooks.StatusLaunched
	delivery.RunID = runID
	delivery.LaunchedAt = &launchedAt
	s.updateWebhookDelivery(ctx, delivery)
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunched)

	// Mirror the launch onto the trigger spine (observational; carries
	// launched_run_id so the evaluator never re-launches). Unifies forge with
	// board/run/schedule sources; no-op without the spine wired.
	s.emitForgeTriggerEvent(ctx, cfg, meta, botID, vars, repoURL, repoRef, runID)

	// Near-real-time forge→board projection: refresh the affected repo's cards
	// now instead of at the next periodic sweep. Detached context + goroutine so
	// it never delays the webhook ack; best-effort, no-op without the cloud board.
	if s.cfg.CloudBoardFor != nil && s.forgeIntegrations != nil {
		repo := meta.ProjectPath
		go func() {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			s.projectForgeWebhookToBoard(pctx, repo)
		}()
	}

	if s.logger != nil {
		s.logger.Info("webhooks: %s/%s %s launched %s run=%s", cfg.Provider, meta.ProjectPath, meta.SubjectID, botID, runID)
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{
		"status": webhooks.StatusLaunched, "run_id": runID, "delivery_id": delivery.ID,
	})
}
