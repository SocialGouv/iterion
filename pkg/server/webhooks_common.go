package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// maxWebhookBodyBytes caps the inbound payload every provider handler
// reads. The cap is generous (5 MiB) — forge events run smaller, but
// we'd rather a forge that mis-bundles fixtures see a 400 than have us
// OOM on a malformed gigabyte of JSON.
const maxWebhookBodyBytes = 5 << 20

// verifyWebhookHMACBody reads and size-limits the request body, then
// verifies its HMAC signature against cfg's sealed secret. providerLabel is
// used only in the bad-signature warn log ("webhooks: <label> bad HMAC …").
// Shared by every provider that authenticates deliveries with a body HMAC
// (GitHub, Forgejo/Gitea) — signature is the raw header value the caller
// extracted (providers vary in header name / fallback aliases).
//
// On failure it has already written the HTTP response (400 on a body read
// error, 401 on a bad signature); ok is false and the caller must return
// immediately without any further side effect (no delivery row, no launch).
func (s *Server) verifyWebhookHMACBody(w http.ResponseWriter, r *http.Request, cfg webhooks.Config, providerLabel, signature string) (body []byte, payloadHash, srcIP string, ok bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return nil, "", "", false
	}
	if !webhooks.VerifyHMACSignature(s.sealer, cfg.ID, cfg.HMACSecretSealed, body, signature) {
		if s.logger != nil {
			s.logger.Warn("webhooks: %s bad HMAC for %s from %s", providerLabel, cfg.ID, s.clientIP(r))
		}
		httpError(w, http.StatusUnauthorized, "invalid signature")
		return nil, "", "", false
	}
	return body, knowledge.ChecksumHex(body), s.clientIP(r), true
}

// forgeProjectionSem bounds concurrent best-effort forge→board projection
// goroutines. A burst of webhooks would otherwise spawn one 30s goroutine
// each without limit. Acquisition is non-blocking: when the cap is reached
// the fast-path refresh is skipped and the periodic forge→board sweep
// reconciles the board, so correctness is preserved and the request never
// blocks.
var forgeProjectionSem = make(chan struct{}, 16)

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

// branchImproveBotID is the branch-improvement bot (Billy). It is NEVER
// auto-launched on a PR-open (that lane is Revi's — a PR-open only ever
// auto-reviews); Billy runs on a PR only on the deliberate `/billy` slash-
// command (handlePRForgeComment / handleGitLabCommandNote, gated on the
// commenter's repo rights) or on the narrow merge-queue auto-heal path
// (NeedsAutoHeal in handlePRForgeReview).
const branchImproveBotID = "branch-improve-loop"

// featureDevBotID is the implementer bot (Featurly) the issue-labeled path
// routes to — a freshly-labeled issue has no diff to review, it needs to be
// TURNED INTO one. See selectIssueLabeledBot.
const featureDevBotID = "feature-dev"

// webhookEventMeta is the provider-agnostic carrier of "what happened
// upstream" the common helpers consume. Every field is optional: a
// provider that doesn't have e.g. a project path leaves it empty and
// the delivery row simply omits it.
type webhookEventMeta struct {
	Kind        string // "merge_request" | "pull_request" | "note" | "generic"
	Action      string // "open" | "reopen" | "comment" | …
	ProjectPath string // "owner/repo" or equivalent
	SubjectID   string // "mr:7" / "pr:42" / "note:99" — stable per-event id
	// ParentSubjectID is the subject this one hangs off when the two differ
	// — the PR a `/command` comment or a review-thread reply lives on. Set
	// by the comment lanes only; the pull_request lane's subject IS the PR.
	ParentSubjectID string
	SubjectURL      string // the subject's own web URL/ref (the issue/MR the comment is on) — back-linked as source_issue_ref for opens_mr commands
	SubjectSHA      string // head SHA, when known
	SenderHandle    string // username for audit (logged only, never in delivery audit row v1)
}

// applyWebhookVarLayers puts the two webhook-level var layers onto a
// handler-built base, in the only order that keeps them meaning what they say:
//
//   - cfg.LaunchVars is the UNION of every co-enabled bot's manifest vars, so
//     it fills in only where nothing more specific is pinned;
//   - cfg.OperatorLaunchVars is the repo's own choice and always wins.
//
// Every launch lane goes through here. The operator layer is what pins a
// repo's shared gate_context, and a lane that skipped it would post its status
// under the bot's default context — leaving the required one stale, which is
// exactly what a manual /revi is supposed to repair.
func applyWebhookVarLayers(vars map[string]string, cfg webhooks.Config) map[string]string {
	mergeVarsInto(vars, cfg.LaunchVars)
	mergeVarsInto(vars, cfg.OperatorLaunchVars)
	return vars
}

// suppressedByHoldLabel applies the bot-agnostic hold-label veto shared by
// every AUTO-launch lane (GitHub/Forgejo/GitLab, PR/MR + issue): if the
// triggering entity carries a configured hold label, it records a filtered
// delivery, writes a 200, and returns true so the caller returns without
// launching — whatever bot would have run. Opt-in (empty HoldLabels = off) and
// fail-open (a payload without the label set is never suppressed). The human
// `/command` lanes deliberately do NOT call this — the label pauses automation,
// not a deliberate manual trigger.
func (s *Server) suppressedByHoldLabel(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, meta webhookEventMeta, labels []string, payloadHash, srcIP string) bool {
	held := webhooks.HeldByLabel(cfg.HoldLabels, labels)
	if held == "" {
		return false
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
		`held: carries hold label "`+held+`" — automation suppressed (remove the label, or trigger a bot manually via a command)`)
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	return true
}

// mergeVarsInto copies every key from src into dst (overwriting on
// collision) and returns dst. A nil src is a no-op. Used by the
// webhook launch-vars builders to layer overlays (context vars,
// operator launch vars) onto a handler-specific base map, each later
// layer winning on key collision.
func mergeVarsInto(dst, src map[string]string) map[string]string {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// fillVarGaps copies the keys of src that dst does not already carry a VALUE
// for: the "layer UNDER" counterpart to mergeVarsInto's "layer OVER". Naming
// the direction is the point — a var whose precedence is ambiguous is how a
// gate_context ends up resolving differently per lane. A nil src is a no-op.
//
// A key present but blank counts as absent. A cleared studio field or an empty
// bot-arg row is not a decision to suppress the repo's policy, and honouring
// it as one would silently disarm the gate — exactly the failure this layering
// exists to fix.
func fillVarGaps(dst, src map[string]string) map[string]string {
	for k, v := range src {
		if strings.TrimSpace(dst[k]) == "" {
			dst[k] = v
		}
	}
	return dst
}

// reviewPRVars composes the launch-vars map every forge-specific
// review-PR path produces: the canonical {pr_url, base_ref, scope_notes,
// post_to_board:"false", pr_review_mode:"summary"} base, an optional
// per-handler `extra` overlay (the Note handler injects "re_review":
// "true"), then the operator's `launchVars` LAST so the per-webhook
// pin always wins. `extra` may be nil; `launchVars` may be nil.
//
// GENERIC PR-context vars (bot-agnostic): the MR/PR-open lanes pass
// "source_branch" (the PR head) via `extra` alongside "base_ref" (the PR
// base). Any bot may read them — a review bot to scope its diff, a doc bot
// to amend the PR head. They name PR facts, never a specific bot.
func reviewPRVars(prURL, baseRef, scopeNotes string, launchVars map[string]string, extra map[string]string) map[string]string {
	vars := map[string]string{
		"pr_url":        prURL,
		"base_ref":      baseRef,
		"scope_notes":   scopeNotes,
		"post_to_board": "false",
		// Inline: the publish path is deterministic and server-anchored
		// (the /api/v1/forge/publish-review endpoint falls back to a
		// summary-only review when a forge rejects the inline anchors),
		// so the historical "summary for a lower failure surface" default
		// no longer applies.
		"pr_review_mode": "inline",
		// head_sha is the revision the run is about to review. It is what
		// lets anything downstream prove it is speaking about the commit it
		// read rather than whatever the branch holds later — the gate
		// reconciler refuses to post without it.
		"head_sha": "",
	}
	mergeVarsInto(vars, extra)
	mergeVarsInto(vars, launchVars)
	return vars
}

// fixerPRVars builds the launch vars for a bot asked to FIX a pull request
// rather than review it: it reviews + hardens the PR's branch diff over
// its base. baseRef is the PR's target branch; scopeNotes carries the PR
// title+body (which includes the "Fixes #N" ticket link). open_mr=false — the
// PR already exists, so Billy commits onto the checked-out PR branch rather
// than opening a second MR; push_branch (the PR's source branch) routes those
// commits through the bot's deterministic push-back so they land ON the PR
// instead of stranding in the cloud runner's ephemeral worktree. The webhook's
// LaunchVars win last so an operator can override per repo (e.g. pin
// max_passes or a scratch path).
func fixerPRVars(baseRef, sourceBranch, prURL, scopeNotes string, asPR bool, launchVars map[string]string) map[string]string {
	vars := map[string]string{
		"base_ref":    baseRef,
		"scope_notes": scopeNotes,
		// The PR Billy is hardening — post_pr_feedback comments its review
		// verdict on it so the author reads the conclusion in the forge.
		"pr_url": prURL,
	}
	if asPR {
		// Open a separate PR targeting the contributor's source branch — the
		// author reviews the bot's hardening as an isolated diff. Billy derives
		// its own mr_branch (iterion/improve/<run>) and opens base=source.
		vars["open_mr"] = "true"
		vars["mr_base"] = sourceBranch
	} else {
		// Commit + push directly onto the PR's own source branch (in-place).
		vars["open_mr"] = "false"
		vars["push_branch"] = sourceBranch
	}
	mergeVarsInto(vars, launchVars)
	return vars
}

// stampBranchImprovePushBack gives a branch-improvement command launch
// (/billy on a PR/MR comment) the same push-back semantics as the
// pull_request-event path above: without open_mr/push_branch the bot's
// mr_gate takes neither tail and its commits strand on the cloud runner's
// storage branch — the PR never receives them. Vars already present
// (operator LaunchVars / route ContextVars) win.
func stampBranchImprovePushBack(vars map[string]string, botID, brancherID, sourceBranch string, asPR bool) {
	if botID != brancherID || sourceBranch == "" {
		return
	}
	if _, ok := vars["open_mr"]; ok {
		return
	}
	if _, ok := vars["push_branch"]; ok {
		return
	}
	if asPR {
		vars["open_mr"] = "true"
		vars["mr_base"] = sourceBranch
	} else {
		vars["open_mr"] = "false"
		vars["push_branch"] = sourceBranch
	}
}

// isIterionForgeBotAuthor reports whether the PR/MR author `login` is iterion's
// OWN forge bot — i.e. this PR was opened by another iterion bot (Doki, Willy,
// Featurly…) through the tenant's forge integration. Such PRs already converge
// to near-perfection inside their own loop, so the PR-open auto-review lane must
// NOT launch Revi on them (a manual `/revi` still can). The detection is
// derived from the tenant's provisioned forge connection, NOT a generic `[bot]`
// suffix — Dependabot/Renovate/etc. must stay reviewable:
//   - GitHub / Forgejo App: the App's bot login is exactly `<app_slug>[bot]`
//     (e.g. "iterion-forge-61934180[bot]"), and app_slug is pinned on the
//     Connection the orchestrator provisioned this webhook from.
//   - GitLab / other non-App: iterion authors MRs as the connected bot account,
//     so a match on that connection's AccountLogin is the equivalent signal
//     (gated to GitLab so a GitHub OAuth connection to a HUMAN account can't
//     make that human's own PRs unreviewable).
//
// Fail-safe: any resolution miss (no provisioning marker, no connection store,
// no app slug) returns false — the PR stays on the normal auto-review path.
// Routed through the webhookIterionBotAuthor seam so handler tests need no live
// connection store.
func (s *Server) isIterionForgeBotAuthor(ctx context.Context, cfg webhooks.Config, login string) bool {
	fn := s.webhookIterionBotAuthor
	if fn == nil {
		fn = s.realIterionBotAuthor
	}
	return fn(ctx, cfg, login)
}

// realIterionBotAuthor is the production isIterionForgeBotAuthor: it resolves the
// forge Connection the webhook was provisioned from and compares `login` against
// that connection's bot identity. See isIterionForgeBotAuthor for the contract.
func (s *Server) realIterionBotAuthor(ctx context.Context, cfg webhooks.Config, login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	// Configured identities first, and WITHOUT a connection lookup — the other
	// half of the pair reads them the same way. An identity only the
	// review-request half recognised would launch on the bot's own reviewer
	// write echoing back, which is the loop this guard exists to close.
	for _, l := range iterionBotLogins(cfg, forge.Connection{}) {
		if strings.EqualFold(login, l) {
			return true
		}
	}
	conn, ok := s.webhookForgeConnection(ctx, cfg)
	if !ok {
		return false
	}
	for _, botLogin := range iterionBotLogins(cfg, conn) {
		if strings.EqualFold(login, botLogin) {
			return true
		}
	}
	return false
}

// iterionBotAuthorPredicate gives the same verdict as isIterionForgeBotAuthor
// as a cheap per-login comparator, for call sites that classify MANY logins in
// one delivery (a review-thread walk): the connection store is read once, not
// once per login. The second return reports whether ANY identity resolved —
// on a GitHub PAT/OAuth connection with no configured logins the set is
// legitimately empty (iterionBotLogins gates the account fallback to GitLab),
// and a caller whose safety rests on the classification must fail closed
// rather than read "always false" as "no bot here". Honours the
// webhookIterionBotAuthor test seam.
func (s *Server) iterionBotAuthorPredicate(ctx context.Context, cfg webhooks.Config) (func(string) bool, bool) {
	if s.webhookIterionBotAuthor != nil {
		return func(login string) bool { return s.webhookIterionBotAuthor(ctx, cfg, login) }, true
	}
	logins := iterionBotLogins(cfg, forge.Connection{})
	if conn, ok := s.webhookForgeConnection(ctx, cfg); ok {
		// The full set: iterionBotLogins starts from the configured
		// identities, so this supersedes the zero-connection list.
		logins = iterionBotLogins(cfg, conn)
	}
	return func(login string) bool {
		login = strings.TrimSpace(login)
		if login == "" {
			return false
		}
		for _, l := range logins {
			if strings.EqualFold(login, l) {
				return true
			}
		}
		return false
	}, len(logins) > 0
}

// iterionBotLogins is THE definition of "iterion's own identity on this
// webhook's forge" — the single set both consumers read: the bot-author
// skip (is this event's actor the bot?) and the re-request-review trigger
// (is this reviewer the bot?). One set by construction, because the two
// checks are the two halves of the same loop guard: an identity the
// trigger recognises but the actor check doesn't would let the bot's own
// reviewer-write echo launch a review of itself.
//
//   - GitHub/Forgejo App: the bot login is the app slug suffixed with
//     [bot].
//   - GitLab (non-App): iterion acts as the connection's own account.
//     Gated to GitLab: on GitHub/Forgejo a PAT/OAuth connection may be a
//     HUMAN's personal account — treating it as the bot would make that
//     human's PRs unreviewable on one side and turn an ordinary
//     human-to-human review request into an LLM launch on the other.
func iterionBotLogins(cfg webhooks.Config, conn forge.Connection) []string {
	// Operator-configured identities first: they are the only ones that can
	// name a USER account, which on GitHub is the only thing that can be a
	// requested reviewer. See Config.ReviewRequestLogins for why this is never
	// derived from the connection.
	var logins []string
	for _, l := range cfg.ReviewRequestLogins {
		if l = strings.TrimPrefix(strings.TrimSpace(l), "@"); l != "" {
			logins = append(logins, l)
		}
	}
	if conn.AppSlug != "" {
		logins = append(logins, conn.AppSlug+"[bot]")
	}
	if cfg.Provider == webhooks.ProviderGitLab && conn.AccountLogin != "" {
		logins = append(logins, conn.AccountLogin)
	}
	return logins
}

// webhookForgeConnection resolves the forge Connection an
// orchestrator-provisioned webhook rides — the identity iterion acts as on
// that forge. false for hand-created webhooks (no provisioning marker), a
// missing store, or a resolution miss.
func (s *Server) webhookForgeConnection(ctx context.Context, cfg webhooks.Config) (forge.Connection, bool) {
	if s.forgeConnections == nil {
		return forge.Connection{}, false
	}
	connID := strings.TrimPrefix(cfg.ProvisionedBy, "forge:")
	if connID == "" || connID == cfg.ProvisionedBy {
		return forge.Connection{}, false
	}
	conn, err := s.forgeConnections.Get(store.WithTenant(ctx, cfg.TenantID), connID)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("webhooks: forge connection %s for webhook %s: %v", connID, cfg.ID, err)
		}
		return forge.Connection{}, false
	}
	return conn, true
}

// isIterionBotReviewRequest reports whether a PR/MR event explicitly asks
// iterion's OWN forge identity for a review — the forge-native "Re-request
// review" button (or adding the bot to the reviewer set): the button form of
// `/revi`, a deliberate on-demand re-review. `requested` is the parser's
// per-provider "does this event request a review from <login>?" predicate,
// probed with the SAME identity set the actor guard reads (iterionBotLogins
// — see its comment for which login counts on which forge).
// Fail-safe like the author check: any resolution miss returns false and the
// delivery stays on the normal filtered path. Routed through the
// webhookIterionBotReviewRequest seam so handler tests need no live
// connection store.
func (s *Server) isIterionBotReviewRequest(ctx context.Context, cfg webhooks.Config, requested func(login string) bool) bool {
	fn := s.webhookIterionBotReviewRequest
	if fn == nil {
		fn = s.realIterionBotReviewRequest
	}
	return fn(ctx, cfg, requested)
}

// realIterionBotReviewRequest is the production isIterionBotReviewRequest:
// it resolves the webhook's connection and probes the parser predicate with
// the SAME identity set the bot-author actor guard reads (iterionBotLogins)
// — never a wider one: an identity only this half recognised would launch
// on the bot's own reviewer-write echo (the actor guard couldn't name it)
// and would treat a human account's review requests as bot triggers.
func (s *Server) realIterionBotReviewRequest(ctx context.Context, cfg webhooks.Config, requested func(login string) bool) bool {
	// A configured identity needs no connection lookup: it is the operator's
	// explicit statement of who the button addresses, and it must arm the lane
	// even when the provisioning marker cannot be resolved.
	for _, l := range iterionBotLogins(cfg, forge.Connection{}) {
		if requested(l) {
			return true
		}
	}
	conn, ok := s.webhookForgeConnection(ctx, cfg)
	if !ok {
		return false
	}
	for _, botLogin := range iterionBotLogins(cfg, conn) {
		if requested(botLogin) {
			return true
		}
	}
	return false
}

// isDependencyBotAuthor reports whether login is an automated dependency-update
// bot (Dependabot / Renovate, including the "renovate[bot]" GitHub-App form and
// common self-hosted names). Case-insensitive.
func isDependencyBotAuthor(login string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	switch l {
	case "dependabot[bot]", "dependabot", "renovate[bot]", "renovate", "renovate-bot":
		return true
	}
	return strings.HasPrefix(l, "renovate[") || strings.HasPrefix(l, "dependabot[")
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
		botID = s.roleBots().Reviewer
	}
	return s.checkBotPermitted(ctx, w, cfg, meta, botID, payloadHash, srcIP)
}

// resolveForgeEventBots returns every bot to launch for a forge EVENT
// delivery (never a command — those route through CommandMap). Rule-driven
// when the config carries a per-bot routing table; otherwise the legacy
// single-bot fallback, so a pre-BotRules config behaves EXACTLY as before.
//
// It writes no HTTP response. An empty result means nothing matched: the
// caller records a `filtered` delivery + 200 (never a 4xx — a 403 on a forge
// hook is what makes GitHub disable it).
func (s *Server) resolveForgeEventBots(cfg webhooks.Config, event, author string) []webhooks.BotRule {
	if cfg.HasBotRules() {
		return cfg.RulesForEvent(event, author)
	}
	botID := cfg.SelectBot()
	if botID == "" {
		botID = s.roleBots().Reviewer
		if s.logger != nil {
			s.logger.Warn("webhooks: config %s carries no bot_rules — falling back to the pinned review default %q; re-provision the integration to route per bot", cfg.ID, botID)
		}
	}
	if !cfg.AllowsBot(botID) {
		return nil
	}
	return []webhooks.BotRule{{BotID: botID}}
}

// forgeIdemKey derives a delivery's idempotency key for one bot. A fan-out
// delivery produces N runs, so the key MUST carry the bot: otherwise the
// second bot reads the first one's claim as a replay and never launches. A
// legacy (no BotRules) delivery keeps the historical bot-less key byte for
// byte, so an in-flight redelivery across the upgrade still dedupes.
func forgeIdemKey(base, botID string, multi bool) string {
	if !multi {
		return knowledge.ChecksumHex([]byte(base))
	}
	return knowledge.ChecksumHex([]byte(base + "|" + botID))
}

// forgePREventTargets turns the resolved rules into launch targets for a
// PR-shaped delivery. Each target gets a FRESH vars map layered
// base < rule.LaunchVars < cfg.LaunchVars (the operator pin always wins) —
// sharing one map would hand every bot the same publish grant, since
// injectForgePublishVars mutates in place.
func forgePREventTargets(
	cfg webhooks.Config,
	rules []webhooks.BotRule,
	idemBase, prURL, baseRef, scopeNotes, cloneURL, sourceBranch string,
	extra map[string]string,
) []forgeLaunchTarget {
	targets := make([]forgeLaunchTarget, 0, len(rules))
	for _, rule := range rules {
		vars := reviewPRVars(prURL, baseRef, scopeNotes, nil, extra)
		// cfg.LaunchVars is the UNION of every co-enabled bot's manifest vars,
		// so it only applies where a rule carries none of its own — layering it
		// over the rule would let one bot's value win for the other, which is
		// exactly what the per-bot table exists to prevent. The operator's own
		// overrides are a separate layer and always win.
		for k, v := range cfg.LaunchVars {
			if _, pinned := rule.LaunchVars[k]; !pinned {
				vars[k] = v
			}
		}
		mergeVarsInto(vars, rule.LaunchVars)
		mergeVarsInto(vars, cfg.OperatorLaunchVars)
		targets = append(targets, forgeLaunchTarget{
			BotID:   rule.BotID,
			IdemKey: forgeIdemKey(idemBase, rule.BotID, cfg.HasBotRules()),
			Vars:    vars,
			RepoURL: cloneURL,
			RepoRef: sourceBranch,
		})
	}
	return targets
}

// selectIssueLabeledBot picks the bot for a freshly-labeled issue. Unlike a
// PR (which carries a diff to REVIEW), an issue must be TURNED INTO one, so
// the reviewer default is wrong here. Precedence: an operator-pinned
// DefaultBotID wins (explicit intent); else the canonical implementer
// (Featurly) when the webhook permits it; else fall back to SelectBot /
// review-pr so a reviewer-only webhook keeps its prior behaviour.
func (s *Server) selectIssueLabeledBot(
	ctx context.Context,
	w http.ResponseWriter,
	cfg webhooks.Config,
	meta webhookEventMeta,
	payloadHash, srcIP string,
) (string, bool) {
	botID := cfg.DefaultBotID
	if implementer := s.roleBots().Implementer; botID == "" && cfg.AllowsBot(implementer) {
		botID = implementer
	}
	if botID == "" {
		if botID = cfg.SelectBot(); botID == "" {
			botID = s.roleBots().Reviewer
		}
	}
	return s.checkBotPermitted(ctx, w, cfg, meta, botID, payloadHash, srcIP)
}

// checkBotPermitted enforces the webhook's bot allowlist, recording an
// Invalid delivery + writing a 403 (ok=false) when botID is out of scope.
func (s *Server) checkBotPermitted(
	ctx context.Context,
	w http.ResponseWriter,
	cfg webhooks.Config,
	meta webhookEventMeta,
	botID, payloadHash, srcIP string,
) (string, bool) {
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
		// The single stamping point for every DELIVERY-CREATING lane, so a
		// handler cannot forget it. Not universal: a board-mode command with
		// the cloud coordinator is carded and returns before any delivery
		// exists (dispatchInvocation), so its runs are found through the
		// card, not here — see stopRunsForDeadPR.
		ParentSubjectID: meta.ParentSubjectID,
		SubjectSHA:      meta.SubjectSHA,
		PayloadHash:     payloadHash,
		Status:          status,
		SourceIP:        srcIP,
		ReceivedAt:      time.Now().UTC(),
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
	res := s.launchWebhookTarget(ctx, r, cfg, meta, forgeLaunchTarget{
		BotID: botID, IdemKey: idemKey, Vars: vars, RepoURL: repoURL, RepoRef: repoRef,
	}, payloadHash, srcIP)
	if res.Status == webhooks.StatusLaunched {
		s.scheduleForgeBoardProjection(meta.ProjectPath)
	}
	s.writeSingleLaunchResult(w, r, res)
}

// writeSingleLaunchResult reproduces, byte for byte, the response the
// single-bot tail has always written — the shape provider handlers and the
// studio still parse.
func (s *Server) writeSingleLaunchResult(w http.ResponseWriter, r *http.Request, res webhookLaunchResult) {
	switch {
	case res.denial != nil:
		s.writeLaunchDenial(w, r, res.denial)
	case res.Status == webhooks.StatusDuplicate:
		writeJSONStatus(w, http.StatusOK, map[string]string{
			"status": webhooks.StatusDuplicate, "run_id": res.RunID, "delivery_id": res.DeliveryID,
		})
	case res.Status == webhooks.StatusLaunched:
		writeJSONStatus(w, http.StatusAccepted, map[string]string{
			"status": webhooks.StatusLaunched, "run_id": res.RunID, "delivery_id": res.DeliveryID,
		})
	default:
		httpError(w, res.httpStatus, "%s", res.Error)
	}
}

// forgeLaunchTarget is one resolved (bot, idempotency key, vars) triple of a
// delivery. Vars MUST be a per-bot map: injectForgePublishVars mutates it in
// place, so a map shared across bots would hand every run the same publish
// grant.
type forgeLaunchTarget struct {
	BotID   string
	IdemKey string
	Vars    map[string]string
	RepoURL string
	RepoRef string
}

// webhookLaunchResult is one bot's outcome inside a (possibly multi-bot)
// delivery. denial/httpStatus carry what the single-bot shell needs to write
// the historical response verbatim.
type webhookLaunchResult struct {
	BotID      string `json:"bot"`
	Status     string `json:"status"`
	RunID      string `json:"run_id,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Error      string `json:"error,omitempty"`

	denial     *launchDenial
	httpStatus int
}

// supersedeLiveRuns cancels the runs a fresh delivery has made obsolete, when
// the webhook opts into overlap=supersede.
//
// The key is (webhook, subject, bot) — ONE pull request's runs of ONE bot, not
// the repo's. Two PRs must review concurrently, and two different bots on the
// same PR are doing different jobs; neither supersedes the other.
//
// Best-effort by construction: a cancel that fails must not stop the new run
// from launching, because the new run is the one carrying the current truth.
// Every outcome is logged — a superseded run is a real event an operator will
// see in the run list and needs to be able to explain.
func (s *Server) supersedeLiveRuns(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta, botID string) {
	cancel := s.webhookCancelRun
	if cancel == nil && s.runs != nil {
		// Named reason: this cancel is nobody's click. It lands in run.Error,
		// which the merge-gate synthetic status quotes — "cancelled by user"
		// there sent operators hunting for a human who did nothing.
		cancel = func(runID string) error {
			return s.runs.CancelWithReason(runID, supersededRunReason)
		}
	}
	if s.webhookDeliveries == nil || cancel == nil || !overlapSupersedes(cfg) {
		return
	}
	if meta.SubjectID == "" {
		return // no subject to scope the supersede to; never cancel repo-wide
	}
	recent, err := s.webhookDeliveries.ListByWebhook(ctx, cfg.TenantID, cfg.ID, supersedeLookback)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("webhooks: supersede lookup failed for %s %s: %v", cfg.ID, meta.SubjectID, err)
		}
		return
	}
	for _, d := range recent {
		if d.RunID == "" || d.BotID != botID || d.SubjectID != meta.SubjectID {
			continue
		}
		if d.Status != webhooks.StatusLaunched {
			continue
		}
		// Cancel is a no-op on a run that already finished, so the delivery
		// row's status is enough of a filter — no run lookup needed.
		if cerr := cancel(d.RunID); cerr != nil {
			if s.logger != nil {
				s.logger.Debug("webhooks: supersede could not cancel run %s (likely already finished): %v", d.RunID, cerr)
			}
			continue
		}
		if s.logger != nil {
			s.logger.Info("webhooks: superseded run %s (%s on %s) — a newer delivery for the same subject arrived", d.RunID, botID, meta.SubjectID)
		}
	}
}

// supersedeLookback bounds the delivery scan behind a supersede check. A PR's
// live runs are necessarily among the most recent deliveries on its webhook,
// and an unbounded scan would put the whole delivery history on the hot path.
const supersedeLookback = 50

// headReviewClaim reports whether the ordinary per-head key space of this
// delivery's head is already claimed by an earlier review launch, and whether
// EVERY fanned-out rule's claim is still in flight — the only state in which
// the click has nothing left to serve. Rb9e7c9: with several bots on the
// event, one live run must not swallow the re-review of a bot whose own run
// finished, nor a retryable launch_error's relaunch — so a single not-live
// rule declines the collapse and the delivery salts. It drives the
// re-request lane's three-way idempotency choice (collapse / salt / claim) —
// see the caller in handlePRForgeReview.
//
// Failure posture: THE BUTTON KEEPS WORKING. Absence of the deliveries store
// reads as unclaimed, a failed RUN lookup as not-live, and a failed STORE
// read (≠ not-found) as claimed-but-not-live — claimed is what routes the
// delivery onto the salted key, so a store hiccup costs at most a duplicate
// review, never a silently deduped no-op (R1545ff; a not-found miss keeps
// the per-head key, whose row the launch tail retries or dedupes exactly).
// A StatusLaunchError row is NOT a claim (mirrors the launch tail: a failed
// launch is retryable via its own key); a StatusAccepted row is a launch in
// progress — in flight by definition, but only within acceptedLaunchWindow
// of its receipt: a process dying between the insert and the post-launch
// update strands the row at accepted forever, and reading that as live would
// permanently disarm the button for the head (Rf96744).
func (s *Server) headReviewClaim(ctx context.Context, cfg webhooks.Config, rules []webhooks.BotRule, headBase string) (claimed, live bool) {
	if s.webhookDeliveries == nil {
		return false, false
	}
	live = true
	for _, rule := range rules {
		d, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, forgeIdemKey(headBase, rule.BotID, cfg.HasBotRules()))
		switch {
		case errors.Is(err, webhooks.ErrNotFound):
			live = false
			continue
		case err != nil:
			claimed, live = true, false
			continue
		case d.Status == webhooks.StatusLaunchError:
			live = false
			continue
		}
		claimed = true
		switch {
		case d.Status == webhooks.StatusAccepted && time.Since(d.ReceivedAt) < acceptedLaunchWindow:
			// launch in progress — in flight.
		case d.RunID != "" && s.webhookRunLive(ctx, d.RunID):
			// run still expected to produce its review — in flight.
		default:
			live = false
		}
	}
	return claimed, claimed && live
}

// overlapSupersedes reports whether this webhook's overlap policy resolves to
// supersede — the single predicate shared by the launch tail's cancel pass
// (supersedeLiveRuns) and the re-request collapse, which DEFERS to it: an
// explicit supersede is the operator saying "newest request wins".
func overlapSupersedes(cfg webhooks.Config) bool {
	if cfg.Overlap == "" {
		return false
	}
	decision, _ := schedgate.EvaluateOverlap([]string{"probe"}, cfg.OverlapPolicy())
	return decision == schedgate.DecisionSupersede
}

// acceptedLaunchWindow bounds how long a StatusAccepted delivery row reads as
// "launch in progress" to the re-request collapse. A live launch resolves to
// launched/launch_error within seconds; a row older than this was stranded by
// a crash and must not keep collapsing re-requests.
const acceptedLaunchWindow = 10 * time.Minute

// webhookRunLive resolves the seam: is this run still expected to produce its
// review (queued, running, or paused)? Terminal statuses — and a run the
// store cannot load — read as not-live, so a re-request on them relaunches.
func (s *Server) webhookRunLive(ctx context.Context, runID string) bool {
	if s.webhookRunIsLive != nil {
		return s.webhookRunIsLive(ctx, runID)
	}
	if s.runs == nil {
		return false
	}
	run, err := s.runs.LoadRunCtx(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return false
	}
	switch run.Status {
	case store.RunStatusQueued, store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		return true
	}
	return false
}

// supersededRunReason is recorded as the run error of a run cancelled by the
// overlap=supersede lane. Kept short: the merge-gate synthetic description
// quotes it within a 60-rune budget.
const supersededRunReason = "superseded by a newer delivery for the same subject"

// scheduleForgeBoardProjection kicks the near-real-time forge→board refresh
// for a repo. Once per DELIVERY, never once per bot: a fan-out would otherwise
// queue N identical projections against the 16-slot semaphore.
func (s *Server) scheduleForgeBoardProjection(repo string) {
	if s.cfg.CloudBoardFor == nil || s.forgeIntegrations == nil {
		return
	}
	select {
	case forgeProjectionSem <- struct{}{}:
		go func() {
			defer func() { <-forgeProjectionSem }()
			// Fresh background context (not derived from the request ctx) so the
			// goroutine neither is cancelled by the response nor keeps the
			// request's scoped values alive for its 30s lifetime.
			pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.projectForgeWebhookToBoard(pctx, repo)
		}()
	default:
		// Concurrency cap reached — skip the fast path; the periodic
		// forge→board sweep will reconcile this repo's cards.
		if s.logger != nil {
			s.logger.Debug("webhooks: forge→board fast-path projection skipped for %s (concurrency cap); periodic sweep will reconcile", repo)
		}
	}
}

// insertAndLaunchWebhookMulti runs one target per bot and writes ONE
// aggregated response. Every target gets an entry — a fan-out never drops a
// bot silently. On an org-scoped denial it stops (the next bot would be denied
// identically) and marks the untried bots "skipped".
func (s *Server) insertAndLaunchWebhookMulti(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	cfg webhooks.Config,
	meta webhookEventMeta,
	targets []forgeLaunchTarget,
	payloadHash string,
	srcIP string,
) {
	if len(targets) == 1 {
		t := targets[0]
		s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, t.IdemKey, t.BotID, t.Vars, t.RepoURL, t.RepoRef, payloadHash, srcIP)
		return
	}

	results := make([]webhookLaunchResult, 0, len(targets))
	var firstDenial *launchDenial
	for i, t := range targets {
		res := s.launchWebhookTarget(ctx, r, cfg, meta, t, payloadHash, srcIP)
		results = append(results, res)
		if res.denial == nil {
			continue
		}
		// Denials are org-scoped (concurrency, launch rate, monthly quota,
		// cost cap) — retrying the next bot would deny identically and
		// re-record the same terminal row.
		firstDenial = res.denial
		for _, skipped := range targets[i+1:] {
			results = append(results, webhookLaunchResult{BotID: skipped.BotID, Status: "skipped"})
		}
		break
	}

	launched := 0
	var firstRunID, firstDeliveryID string
	for _, res := range results {
		if res.Status != webhooks.StatusLaunched {
			continue
		}
		launched++
		if firstRunID == "" {
			firstRunID, firstDeliveryID = res.RunID, res.DeliveryID
		}
	}
	if launched > 0 {
		s.scheduleForgeBoardProjection(meta.ProjectPath)
	}

	switch {
	case launched > 0:
		// A sibling failure is not fatal: its row is StatusLaunchError, which
		// the replay check treats as retryable, so a redelivery relaunches
		// exactly the bots that failed.
		writeJSONStatus(w, http.StatusAccepted, map[string]any{
			"status": webhooks.StatusLaunched, "run_id": firstRunID,
			"delivery_id": firstDeliveryID, "launches": results,
		})
	case firstDenial != nil:
		s.writeLaunchDenial(w, r, firstDenial)
	default:
		status, code := webhooks.StatusDuplicate, http.StatusOK
		for _, res := range results {
			if res.Status == webhooks.StatusLaunchError || res.httpStatus >= 500 {
				status, code = webhooks.StatusLaunchError, http.StatusBadGateway
				break
			}
		}
		writeJSONStatus(w, code, map[string]any{
			"status": status, "run_id": firstRunID, "delivery_id": firstDeliveryID, "launches": results,
		})
	}
}

// launchWebhookTarget is the per-bot core of the tail: replay check →
// admission → delivery row → publish grant → launch → trigger-spine emit. It
// writes no HTTP response and schedules no board projection, so it composes
// for one bot or N.
func (s *Server) launchWebhookTarget(
	ctx context.Context,
	r *http.Request,
	cfg webhooks.Config,
	meta webhookEventMeta,
	t forgeLaunchTarget,
	payloadHash string,
	srcIP string,
) webhookLaunchResult {
	idemKey, botID, vars := t.IdemKey, t.BotID, t.Vars
	out := webhookLaunchResult{BotID: botID}

	// 1. Idempotency replay check — BEFORE metering. gateLaunch performs the
	// per-org quota CAS *increment* (the increment IS the metering), so a
	// forge redelivery of an already-processed event (lost ack, operator
	// "Redeliver") must be caught here or it re-charges the org's monthly
	// run/cost budget and then fails on the insert with nothing launched.
	// Denied events are recorded under a random key (see step 3), never
	// under idemKey, so this lookup misses them and a retry-after-reset
	// still launches.
	//
	// EXCEPTION — a prior LAUNCH FAILURE (StatusLaunchError: no run was ever
	// created, RunID empty) is RETRYABLE, not a terminal duplicate. A
	// transient failure (a temporarily-broken bot, an LLM 5xx, a deploy
	// window) must be relaunchable by a redelivery of the SAME event; else
	// the failure poisons re-review for that exact (repo, PR#, head sha)
	// until a new commit changes the key. We reuse that row (below) instead
	// of short-circuiting.
	var reusePriorFailure *webhooks.Delivery
	if s.webhookDeliveries != nil {
		if existing, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey); err == nil {
			if existing.Status != webhooks.StatusLaunchError {
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				out.Status = webhooks.StatusDuplicate
				out.RunID, out.DeliveryID = existing.RunID, existing.ID
				return out
			}
			ex := existing
			reusePriorFailure = &ex
		}
	}

	// 1b. Supersede: this delivery's input makes the live run for the same
	// subject obsolete (a newer commit on the same PR). Cancel before
	// metering, so the superseded run's slot is freed for the fresh one.
	s.supersedeLiveRuns(ctx, cfg, meta, botID)

	// 2. Run-launch admission. Only reached for a genuinely new delivery
	// (replays were filtered in step 1), so the quota CAS fires once per
	// distinct event. A denied event writes a terminal row under a random
	// key so a later forge retry can launch once the quota resets.
	adm, d := s.gateLaunch(ctx)
	if d != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, d.reason)
		out.Status = webhooks.StatusLaunchError
		out.Error = d.reason
		out.denial = d
		return out
	}

	// 3. Idempotency insert (durable dedupe backstop for concurrent
	// deliveries of the same event that both passed step 1) — OR reuse a
	// prior failed row (retry of a StatusLaunchError delivery, above).
	delivery := newWebhookDelivery(cfg, meta, webhooks.StatusAccepted, payloadHash, srcIP)
	delivery.IdempotencyKey = idemKey
	delivery.BotID = botID
	if reusePriorFailure != nil {
		// Retry: keep the prior row's identity + received-at, clear the
		// error, and UPDATE it (Insert would ErrDuplicate on the idemKey).
		delivery.ID = reusePriorFailure.ID
		delivery.ReceivedAt = reusePriorFailure.ReceivedAt
		if s.webhookDeliveries != nil {
			if err := s.webhookDeliveries.Update(ctx, delivery); err != nil {
				adm.rollback(s.logger)
				out.Status = webhooks.StatusLaunchError
				out.Error = fmt.Sprintf("reset failed delivery: %v", err)
				out.httpStatus = http.StatusInternalServerError
				return out
			}
		}
	} else if s.webhookDeliveries != nil {
		if err := s.webhookDeliveries.Insert(ctx, delivery); err != nil {
			if errors.Is(err, webhooks.ErrDuplicate) {
				// Concurrent-duplicate loser: both deliveries passed the
				// step-1 replay check and both metered a quota unit in
				// step 2, but only the Insert winner launches. Release
				// this delivery's unit or every concurrent forge
				// redelivery over-counts the monthly run quota.
				adm.rollback(s.logger)
				// Read back the prior delivery so the duplicate 200
				// echoes its run_id/delivery_id. A failed read would
				// otherwise emit a misleading 200 with empty IDs —
				// surface it as a 500 instead.
				existing, gerr := s.webhookDeliveries.GetByIdempotencyKey(ctx, idemKey)
				if gerr != nil {
					out.Status = webhooks.StatusLaunchError
					out.Error = fmt.Sprintf("lookup duplicate delivery: %v", gerr)
					out.httpStatus = http.StatusInternalServerError
					return out
				}
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				out.Status = webhooks.StatusDuplicate
				out.RunID, out.DeliveryID = existing.RunID, existing.ID
				return out
			}
			adm.rollback(s.logger)
			out.Status = webhooks.StatusLaunchError
			out.Error = fmt.Sprintf("record delivery: %v", err)
			out.httpStatus = http.StatusInternalServerError
			return out
		}
	}

	// 3. Launch.
	launch := s.webhookLaunchBot
	if launch == nil {
		// A closure over cfg rather than a bare method reference: the real
		// launcher needs the webhook's own retry policy, and threading it
		// as a ninth positional parameter would churn every test fake of
		// this seam for one field.
		launch = s.webhookLauncherFor(cfg)
	}
	// Hand the run any prior review of the same PR, if it asked for one. Done
	// HERE, in the tail every lane funnels through, rather than per provider
	// handler: the two comment lanes had it and nothing else did, so the
	// merge-queue auto-heal — which launches a fixer on a PR a reviewer has
	// almost always already read — started from nothing. The PR context comes
	// from the vars the lane already built, so a lane with no pr_url no-ops.
	s.stampHandoffs(ctx, cfg, botID, vars, handoffQuery{
		PRURL:   vars["pr_url"],
		HeadSHA: vars["head_sha"],
	})
	// Deterministic forge review publishing: a review-shaped delivery
	// carries a pr_url var — mint a per-run publish grant scoped to the
	// webhook's tenant so the bot's deterministic publish node posts
	// through the server's live forge client (never a workspace token).
	vars = s.injectForgePublishVars(ctx, cfg.TenantID, "", botID, vars, r)
	// meta.ProjectPath is the forge slug already parsed by the provider
	// handler — thread it onto the launch so the run is filterable by
	// repository in the studio.
	runID, lerr := launch(ctx, botID, vars, t.RepoURL, t.RepoRef, meta.ProjectPath, cfg.KeyOverrides, cfg.SecretOverrides)
	if lerr != nil {
		delivery.Status = webhooks.StatusLaunchError
		delivery.Error = lerr.Error()
		s.updateWebhookDelivery(ctx, delivery)
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunchError)
		out.Status = webhooks.StatusLaunchError
		out.Error = fmt.Sprintf("launch failed: %v", lerr)
		out.DeliveryID = delivery.ID
		out.httpStatus = http.StatusBadGateway
		return out
	}
	launchedAt := time.Now().UTC()
	delivery.Status = webhooks.StatusLaunched
	delivery.RunID = runID
	delivery.LaunchedAt = &launchedAt
	s.updateWebhookDelivery(ctx, delivery)
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusLaunched)

	// Claim the repo's gate context on the revision this run is about to
	// review, so the minutes between the push and the verdict read as
	// "running" instead of as the absence they are indistinguishable from.
	// After the launch, because the marker carries the run's URL.
	s.markGateInFlight(ctx, cfg.TenantID, botID, vars, runID)

	// Mirror the launch onto the trigger spine (observational; carries
	// launched_run_id so the evaluator never re-launches). Unifies forge with
	// board/run/schedule sources; no-op without the spine wired.
	s.emitForgeTriggerEvent(ctx, cfg, meta, botID, vars, t.RepoURL, t.RepoRef, runID)

	if s.logger != nil {
		s.logger.Info("webhooks: %s/%s %s launched %s run=%s", cfg.Provider, meta.ProjectPath, meta.SubjectID, botID, runID)
	}
	out.Status = webhooks.StatusLaunched
	out.RunID, out.DeliveryID = runID, delivery.ID
	return out
}

// prClosedRunReason names the cancel so the run list, and the merge-gate
// synthetic status that quotes run.Error, say WHY. "cancelled by user"
// there once sent operators hunting for a human who did nothing.
const prClosedRunReason = "pull request closed or merged — nothing left to review"

// stopRunsForDeadPR ends the runs a pull request that just closed or merged
// left behind, and disarms any usage-window retry armed for one.
//
// Two distinct leaks, both observed: a run in FLIGHT keeps spending
// provider quota on a diff nobody will merge, and a run PARKED on a
// provider window is a promise to come back — hours later, to review and
// comment on a dead pull request. Cancelling covers the first; the retry
// disarm covers the second, and it is the one nothing else would do (the
// retry lives in the store, not in the run's process).
//
// REACH — what this does and does not see, stated because it is not "every
// run bound to the PR". Discovery is the webhook DELIVERY audit, so it
// covers every run a delivery recorded: the pull_request lane itself, plus
// the `/command` and review-thread lanes, whose per-comment subjects are
// matched through Delivery.ParentSubjectID. It does NOT cover a board-mode
// command running under the cloud coordinator: dispatchInvocation cards it
// and returns before any delivery exists, so those runs have no row here at
// all (they are reachable only through the card, via the SourceRef.IssueID
// reverse edge). Nor rows written before ParentSubjectID shipped. Closing
// the board lane needs a per-bot answer to "does a command REQUESTED on a
// pull request die with it?" — true for /billy, which pushes to the PR
// branch, and false for a repo-wide audit someone happened to ask for in a
// PR comment — so it is deliberately not decided here.
//
// Scoped to (project, subject) across EVERY bot, unlike supersedeLiveRuns
// which is per-bot: supersede replaces one bot's work with newer work of
// the same bot, while a closed PR ends everyone's. The PROJECT half is
// load-bearing — a subject id ("pr:7") carries no repo and one webhook
// config can serve several, so matching the subject alone would cancel a
// same-numbered pull request of another repo.
//
// The scan is the EXACT by-subject query, never the recency-bounded one
// supersede uses: the run this exists to reach is the one parked hours
// ago, i.e. precisely the delivery a 50-row window has already dropped.
//
// Only runs still LIVE are touched. A merged PR's history is mostly
// finished reviews, and disarming a retry that was never armed writes a
// stop reason onto a run that succeeded — a lie the next person debugging
// retries would read as fact.
//
// Best-effort throughout — the close event must answer 200 regardless, or
// the forge starts disabling the hook. Returns how many runs it actually
// stopped, for the delivery audit reason.
func (s *Server) stopRunsForDeadPR(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta) int {
	if s.webhookDeliveries == nil || meta.SubjectID == "" || meta.ProjectPath == "" {
		return 0
	}
	cancel := s.webhookCancelRun
	if cancel == nil && s.runs != nil {
		cancel = func(runID string) error {
			return s.runs.CancelWithReason(runID, prClosedRunReason)
		}
	}
	retries := store.AsRunRetryStore(s.cfg.Store)
	if cancel == nil && retries == nil {
		return 0
	}
	launched, err := s.webhookDeliveries.ListLaunchedBySubject(ctx, cfg.TenantID, cfg.ID, meta.ProjectPath, meta.SubjectID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("webhooks: closed-PR stop lookup failed for %s %s %s: %v", cfg.ID, meta.ProjectPath, meta.SubjectID, err)
		}
		return 0
	}
	stopped := 0
	seen := make(map[string]bool, len(launched))
	for _, d := range launched {
		if d.RunID == "" || seen[d.RunID] {
			continue // one PR has several deliveries per run's lifetime
		}
		seen[d.RunID] = true
		if !s.runIsStoppable(ctx, d.RunID) {
			continue
		}
		// Disarm FIRST: a cancel that lands while a retry is still armed
		// leaves the promise standing, and the sweeper would resume the
		// run we just cancelled.
		if retries != nil {
			if aerr := retries.AbandonRunRetry(ctx, d.RunID, prClosedRunReason); aerr != nil && s.logger != nil {
				s.logger.Debug("webhooks: could not disarm the retry of run %s on a closed PR: %v", d.RunID, aerr)
			}
		}
		if cancel == nil {
			continue
		}
		if cerr := cancel(d.RunID); cerr != nil {
			if s.logger != nil {
				s.logger.Debug("webhooks: closed-PR stop could not cancel run %s (it may have just settled): %v", d.RunID, cerr)
			}
			continue
		}
		stopped++
		if s.logger != nil {
			s.logger.Info("webhooks: stopped run %s (%s on %s %s) — its pull request closed or merged", d.RunID, d.BotID, meta.ProjectPath, meta.SubjectID)
		}
	}
	return stopped
}

// runIsStoppable reports whether a run is still live enough for the
// closed-PR stop to touch it: running/queued/paused (cancel it) or parked
// with an armed retry (disarm the promise). A settled run is left alone —
// writing a stop reason onto a review that finished hours ago tells the
// next reader something false about it.
//
// Fails OPEN (true) when the run cannot be read: a store blip must not
// strand a live run on a dead pull request, and both actions are no-ops
// on a settled run anyway.
func (s *Server) runIsStoppable(ctx context.Context, runID string) bool {
	if s.cfg.Store == nil {
		return true
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil {
		return true
	}
	switch run.Status {
	case store.RunStatusRunning, store.RunStatusQueued,
		store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		return true
	case store.RunStatusFailedResumable:
		// Only the ones something will actually come back for.
		return run.RetryState != nil && run.RetryState.RetryAfter != nil
	default:
		return false
	}
}
