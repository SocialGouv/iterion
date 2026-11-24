package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The spine router (ADR-172, plan forge_spine_route.go): a forge delivery
// becomes a trigger.Event, the handler matches it — synchronously, before it
// answers — against the subscriptions bound to its integration, and every
// subscription that claims it becomes one target of the existing webhook
// tail. The spine is the forge ROUTER; the webhook tail stays the forge
// effect. Steps, in order:
//
//  1. match against the binding (MatchForgeBound); a store error is a
//     laneUnavailable 503 — nothing claimed, nothing metered;
//  2. a binding with no row at all is an unprojected integration: 503 on an
//     integration binding (never a silent legacy fallback), 200 filtered on
//     a hand-created webhook;
//  3. the staleness gate: the projection digest recomputed from the live
//     catalogue, with the arguments the projection was given, must match
//     every bound row's — stale rows route nothing and never route legacy;
//  4. per bot, at most one target: ambiguity, webhook scope, catalogue
//     visibility and unresolvable modes are PER-BOT filtered rows beside the
//     bots that do answer — never a 4xx for the delivery;
//  5. the read phase: every remaining target's lane is walked in BOTH
//     stores before anything is claimed; a read error refuses the whole
//     delivery; a record that answers THIS event for a bot (its claim row,
//     or the card carded for it) makes that bot — and only that bot — the
//     duplicate it is, so the redelivery a partial answer asks for still
//     starts the bots it left unclaimed;
//  6. the claim phase, in bot order: a board target with a coordinator
//     claims its card, a direct (or coordinator-less board) target claims
//     its Delivery row through the tail; a claim that fails is that bot's
//     failure, never its siblings'; the answer follows the claims.

// forgeRoutingFor resolves the routing mode of one lane for cfg: the mode
// stored on the config's ForgeRouting, else the server default
// (ITERION_FORGE_ROUTING_DEFAULT, parsed when the server was built — only
// legacy is accepted there). "" routes nowhere by spine — the lane keeps its
// legacy handler.
func (s *Server) forgeRoutingFor(cfg webhooks.Config, lane string) string {
	if mode := cfg.ForgeRouting.For(lane); mode != "" {
		return mode
	}
	return s.forgeRoutingDefault[lane]
}

// forgeBindingOf names the forge binding the delivery's config projects its
// subscriptions under: the integration binding when the config is managed by
// one, else the hand-created webhook's own binding — so a webhook-bound row
// keeps a path after the legacy router is retired.
func forgeBindingOf(cfg webhooks.Config) string {
	if cfg.ProvisionIntegrationID != "" {
		return trigger.ForgeBindingForIntegration(cfg.ProvisionIntegrationID)
	}
	return trigger.ForgeBindingForWebhook(cfg.ID)
}

// spineTarget is one matched subscription resolved for the claim phase: the
// lane it claims, its bot-scoped chain base, and the launch it would carry.
type spineTarget struct {
	sub     trigger.Subscription
	base    string
	lane    triggerLane
	route   webhooks.CommandRoute
	vars    map[string]string
	verdict laneVerdict
}

// routeIssueThroughSpine runs the router steps 1-6 above for one issue-label
// delivery and writes the answer.
func (s *Server) routeIssueThroughSpine(ctx context.Context, w http.ResponseWriter, r *http.Request,
	cfg webhooks.Config, meta webhookEventMeta, d forgeIssueDelivery, payloadHash, srcIP string) {
	if s.cfg.TriggerStore == nil {
		// No store: no rows exist anywhere — the unprojected-integration
		// answer, never a lane error and never a legacy fallback.
		if cfg.ProvisionIntegrationID == "" {
			reason := fmt.Sprintf("no subscription is bound to webhook %s: nothing claims this delivery", cfg.ID)
			s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
			return
		}
		s.spineRefuseRetryable(ctx, w, cfg, meta, payloadHash, srcIP, webhooks.StatusLaunchError,
			fmt.Sprintf("integration %s has no projected subscriptions: POST /api/teams/%s/forge/repo-bots/%s/subscriptions/sync",
				cfg.ProvisionIntegrationID, cfg.TenantID, cfg.ProvisionIntegrationID))
		return
	}
	ev := d.event(cfg, meta)
	binding := forgeBindingOf(cfg)
	bound, err := s.cfg.TriggerStore.ListByForgeBinding(ctx, cfg.TenantID, binding)
	if err != nil {
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, fmt.Errorf("list the subscriptions bound to %s: %w", binding, err))
		return
	}
	match, err := trigger.MatchForgeBound(ctx, s.cfg.TriggerStore, cfg.TenantID, binding, ev)
	if err != nil {
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, fmt.Errorf("match the subscriptions bound to %s: %w", binding, err))
		return
	}
	// An integration whose rows were never synced is a sync problem, not a
	// legacy fallback: retryable 503, the audit row names the sync route.
	if match.Bound == 0 {
		if cfg.ProvisionIntegrationID == "" {
			reason := fmt.Sprintf("no subscription is bound to webhook %s (%s): nothing claims this delivery", cfg.ID, binding)
			s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
			return
		}
		s.spineRefuseRetryable(ctx, w, cfg, meta, payloadHash, srcIP, webhooks.StatusLaunchError,
			fmt.Sprintf("integration %s has no projected subscriptions: POST /api/teams/%s/forge/repo-bots/%s/subscriptions/sync",
				cfg.ProvisionIntegrationID, cfg.TenantID, cfg.ProvisionIntegrationID))
		return
	}
	// Staleness: the rows are a view of the manifests, checked every time
	// they are used. Stale rows launch nothing and never fall back to legacy.
	stale, err := s.spineDigestStale(ctx, cfg, bound)
	if err != nil {
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, err)
		return
	}
	if stale {
		s.spineRefuseRetryable(ctx, w, cfg, meta, payloadHash, srcIP, "subscriptions_stale",
			fmt.Sprintf("subscriptions of integration %s are stale — the catalogue moved since the last projection: POST /api/teams/%s/forge/repo-bots/%s/subscriptions/sync",
				cfg.ProvisionIntegrationID, cfg.TenantID, cfg.ProvisionIntegrationID))
		return
	}

	// Step 4: per bot, at most one target. Matched rows are grouped by bot;
	// a bot with two matched rows is ambiguous and starts nothing — the
	// projection refuses the case, so reaching it means a hand-edited row or
	// an older planner. Every other refusal is a per-bot filtered row beside
	// the bots that do answer.
	if len(match.Matched) == 0 {
		// No row claims the event: 200 filtered naming every bound candidate
		// and the dimension that failed it — the forge keeps the hook.
		reason := spineNoClaimReason(bound, d)
		s.warnf("webhooks: %s/%s %s: filtered on the spine: %s", cfg.Provider, meta.ProjectPath, meta.SubjectID, reason)
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
		return
	}
	matchedByBot := map[string][]trigger.Subscription{}
	var bots []string
	for _, sub := range match.Matched {
		if _, seen := matchedByBot[sub.BotID]; !seen {
			bots = append(bots, sub.BotID)
		}
		matchedByBot[sub.BotID] = append(matchedByBot[sub.BotID], sub)
	}
	slices.Sort(bots)
	var reasons []string
	for _, bot := range bots {
		rows := matchedByBot[bot]
		if len(rows) > 1 {
			ids := make([]string, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			reason := fmt.Sprintf("ambiguous: subscriptions %s of %s both claim %s",
				strings.Join(ids, ", "), bot, trigger.MatchedAddedLabel(rows[0].Match, ev))
			s.recordSpineBotFiltered(ctx, cfg, meta, payloadHash, srcIP, bot, rows[0].ID, reason)
			reasons = append(reasons, reason)
			delete(matchedByBot, bot)
			continue
		}
		if reason := s.spineBotRefusal(ctx, cfg, rows[0]); reason != "" {
			s.recordSpineBotFiltered(ctx, cfg, meta, payloadHash, srcIP, bot, rows[0].ID, reason)
			reasons = append(reasons, reason)
			delete(matchedByBot, bot)
		}
	}

	// Step 5: the read phase. Every remaining target's lane, in both stores,
	// before any claim; a read error refuses the whole delivery (503,
	// nothing claimed, nothing metered).
	laneEv := laneEvent{PayloadHash: payloadHash, DeliveryID: meta.DeliveryID}
	var (
		targets []spineTarget
		// answered are the bots a record of THIS event already answers — its
		// claim row, or the card carded for it. Each is that bot's
		// duplicate; the bots no record answers still claim, so a
		// redelivery completes the fan-out a partial answer left open. The
		// claims are per-bot unique keys (a deterministic card id, a claim
		// row's idempotency key), so a twin replica racing on the same
		// delivery still starts each bot once.
		answered []webhookLaunchResult
		// eraAnswer is a record of THIS event a pre-spine router made (a
		// legacy claim row or bot-less lane card): that router decided the
		// whole delivery, so it answers the whole delivery and no bot it
		// did not start may start now.
		eraAnswer *webhookLaunchResult
	)
	for _, bot := range bots {
		rows := matchedByBot[bot]
		if len(rows) == 0 {
			continue // filtered in step 4
		}
		sub := rows[0]
		mode, merr := bundle.ResolveMode(bundle.InvocationKindForge, sub.Mode, "forge router")
		if merr != nil {
			// Unreachable through the step-4 filter; an explicit guard keeps
			// an unresolved mode from ever building a target.
			reason := merr.Error()
			s.recordSpineBotFiltered(ctx, cfg, meta, payloadHash, srcIP, bot, sub.ID, reason)
			reasons = append(reasons, reason)
			continue
		}
		trig := trigger.MatchedAddedLabel(sub.Match, ev)
		t := spineTarget{
			sub:  sub,
			lane: triggerLane{Project: d.ProjectPath, Subject: meta.SubjectID, Trigger: trig, Bot: bot},
			route: webhooks.CommandRoute{
				BotID:       bot,
				Mode:        string(mode),
				ArgsVar:     sub.ArgsVar,
				ContextVars: sub.Vars,
				OpensMR:     sub.OpensMR,
			},
			vars: spineIssueVars(d, sub, cfg),
		}
		t.base = botLaneBase(d.legacyBaseString(cfg, trig), bot)
		rd := s.spineLaneRead(cfg, d, t, mode)
		verdict, err := s.readTriggerLane(ctx, cfg, rd, laneEv)
		if err != nil {
			s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, err)
			return
		}
		var answer *webhookLaunchResult
		switch {
		case verdict.answered != nil && laneEv.claimedBy(*verdict.answered):
			answer = &webhookLaunchResult{BotID: bot, Status: webhooks.StatusDuplicate,
				RunID: verdict.answered.RunID, DeliveryID: verdict.answered.ID, SubscriptionID: sub.ID}
		case verdict.card != nil && laneEv.cardedFor(verdict.card):
			answer = &webhookLaunchResult{BotID: bot, Status: webhooks.StatusDuplicate,
				CardID: verdict.card.ID, SubscriptionID: sub.ID}
		}
		if answer != nil {
			if verdict.legacyEra && eraAnswer == nil {
				eraAnswer = answer
			}
			answered = append(answered, *answer)
			continue
		}
		t.verdict = verdict
		targets = append(targets, t)
	}
	// A redelivery of a handled event: answered by the record of the era
	// that decided the whole delivery, else — every matched bot answered —
	// by the first bot's.
	if eraAnswer != nil {
		s.writeSpineAnswered(w, cfg, *eraAnswer)
		return
	}
	if len(targets) == 0 && len(answered) > 0 {
		s.writeSpineAnswered(w, cfg, answered[0])
		return
	}

	// Step 6: the claim phase, in bot order. pre carries the claims decided
	// here (the carded board targets); targets carries the tail's own.
	var (
		pre      []webhookLaunchResult
		launches []forgeLaunchTarget
		dupes    = answered
		// unclaimed are the board targets whose claim could not be decided
		// (a board or lane-store failure): per-bot claim failures, named in
		// the partial answer beside the bots that did claim.
		unclaimed []webhookLaunchResult
		failures  []string
	)
	for _, t := range targets {
		ev := laneEvent{PayloadHash: payloadHash, DeliveryID: meta.DeliveryID}
		absorb := t.verdict.answered
		card := t.verdict.card
		switch {
		case absorb != nil && t.verdict.absorb:
			// A serving claim of ANOTHER event absorbs this bot: recorded
			// under the bot's own base, so a redelivery stays a duplicate.
			s.recordLaneAbsorbed(ctx, cfg, meta, t.base, t.sub.BotID, ev, srcIP,
				fmt.Sprintf("claim %s (run %q)", absorb.ID, absorb.RunID))
			dupes = append(dupes, webhookLaunchResult{BotID: t.sub.BotID,
				Status: webhooks.StatusDuplicate, RunID: absorb.RunID, DeliveryID: absorb.ID})
		case card != nil && t.verdict.absorb:
			s.recordLaneAbsorbed(ctx, cfg, meta, t.base, t.sub.BotID, ev, srcIP, "card "+card.ID)
			dupes = append(dupes, webhookLaunchResult{BotID: t.sub.BotID,
				Status: webhooks.StatusDuplicate, DeliveryID: card.ID})
		case t.route.Mode == string(bundle.ExecutionBoard) && s.cfg.CloudBoardFor != nil && s.cfg.CloudBoardCoordinator != nil:
			// The card IS the claim: decide, then leave the carded audit row
			// beside the lane, under a key no lane walk reads.
			res, err := s.cardTriggerLaneDecide(ctx, cfg, meta, t.base, t.route, t.vars,
				d.CloneURL, d.RepoRef, payloadHash, srcIP, launchProvenance{}, t.lane, nil,
				laneSpineHalf{}, boardCardExtras{ContextVars: t.route.ContextVars, OwnVars: true})
			if err != nil {
				unclaimed = append(unclaimed, webhookLaunchResult{BotID: t.sub.BotID, Status: webhooks.StatusLaunchError,
					Error: err.Error(), SubscriptionID: t.sub.ID, httpStatus: http.StatusServiceUnavailable})
				failures = append(failures, fmt.Sprintf("%s: %v", t.sub.BotID, err))
				continue
			}
			switch {
			case res.denial != nil:
				pre = append(pre, res.webhookLaunchResult)
			case res.created:
				s.recordSpineCardedRow(ctx, cfg, meta, payloadHash, srcIP, t.base, res.cardID, t.sub)
				out := res.webhookLaunchResult
				out.CardID, out.SubscriptionID = res.cardID, t.sub.ID
				pre = append(pre, out)
			case res.Status == webhooks.StatusDuplicate && !res.absorb && (res.cardID != "" || res.row != nil):
				// A record carded FOR THIS EVENT answers the decide — the race
				// a twin replica won on this very delivery, or the card the
				// legacy era left on it. This bot is that record's duplicate;
				// the targets still queued claim on their own keys.
				out := webhookLaunchResult{BotID: t.sub.BotID, Status: webhooks.StatusDuplicate, CardID: res.cardID, SubscriptionID: t.sub.ID}
				if res.row != nil {
					out.RunID, out.DeliveryID = res.row.RunID, res.row.ID
				}
				dupes = append(dupes, out)
			default:
				// A serving claim of another event decided the lane between
				// the read and the claim: absorbed, beside the bots that
				// claimed.
				dupes = append(dupes, res.webhookLaunchResult)
			}
		default:
			// Direct, or board without a coordinator: the tail claims a
			// Delivery row. A board route also materialises its tracking
			// card — a record, never a claim.
			if t.route.Mode == string(bundle.ExecutionBoard) && s.cfg.CloudBoardFor != nil {
				s.ensureBoardCard(ctx, cfg, t.route, t.vars, meta, "", d.CloneURL, d.RepoRef, &t.lane)
			}
			base, displaced := t.base, t.verdict.displaced
			launches = append(launches, forgeLaunchTarget{
				BotID: t.sub.BotID, IdemKey: t.verdict.nextKey, Vars: t.vars,
				RepoURL: d.CloneURL, RepoRef: d.RepoRef,
				subscriptionID: t.sub.ID, routedBy: webhooks.RoutingSpine,
				onClaimDecided: func(claimed bool, existing *webhooks.Delivery) {
					switch {
					case claimed && displaced != nil:
						s.recordLaneAbsorbed(ctx, cfg, meta, base, t.sub.BotID, *displaced, srcIP,
							"the delivery that retries its failed claim")
					case !claimed && existing != nil && !ev.claimedBy(*existing):
						s.recordLaneAbsorbed(ctx, cfg, meta, base, t.sub.BotID, ev, srcIP,
							fmt.Sprintf("claim %s (run %q)", existing.ID, existing.RunID))
					}
				},
			})
		}
	}

	// The answer follows the claims.
	switch {
	case len(pre)+len(launches) > 0:
		// A board claim that could not be decided rides beside the claims
		// as a per-bot failure: the tail names it in the partial answer, and
		// the redelivery that answer asks for claims it.
		s.acceptWebhookTargetsWith(ctx, w, r, cfg, meta, launches, append(pre, unclaimed...), payloadHash, srcIP)
	case len(unclaimed) > 0:
		// Nothing claimed and a claim undecided: the whole delivery is
		// retryable, every other bot already a duplicate or filtered.
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, errors.New(strings.Join(failures, "; ")))
	case len(dupes) > 0:
		// Every target a duplicate: 200, answered by the first.
		s.writeSpineAnswered(w, cfg, dupes[0])
	default:
		// Nothing claims: 200 filtered naming every refusal — the forge
		// keeps the hook enabled.
		reason := "no subscription claims this delivery"
		if len(reasons) > 0 {
			reason = strings.Join(reasons, "; ")
		}
		s.warnf("webhooks: %s/%s %s: filtered on the spine: %s", cfg.Provider, meta.ProjectPath, meta.SubjectID, reason)
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
	}
}

// writeSpineAnswered answers a delivery every matched bot of which is a
// duplicate, from the first answering record: the card carded for the event
// ("carded", the card id), else its claim row (duplicate, its run and
// delivery ids).
func (s *Server) writeSpineAnswered(w http.ResponseWriter, cfg webhooks.Config, first webhookLaunchResult) {
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
	if first.CardID != "" && first.DeliveryID == "" {
		writeJSONStatus(w, http.StatusOK, map[string]string{
			"status": "carded", "card_id": first.CardID,
		})
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]string{
		"status": webhooks.StatusDuplicate, "run_id": first.RunID, "delivery_id": first.DeliveryID,
	})
}

// spineLaneRead builds the lane read of one spine target: its own bot-scoped
// chain as primary, the same trigger's legacy chain read-only (serving and
// absorption), the key legacy used for THIS event (GitLab payload order) for
// its identity alone, and the board half only where a wired coordinator makes
// a card a claim.
func (s *Server) spineLaneRead(cfg webhooks.Config, d forgeIssueDelivery, t spineTarget, mode bundle.ExecutionMode) laneRead {
	rd := laneRead{
		Lane:      &t.lane,
		Bases:     []string{t.base},
		Legacy:    []string{d.legacyString(cfg, t.lane.Trigger)},
		EventOnly: []string{},
		Bot:       t.sub.BotID,
		Board:     mode == bundle.ExecutionBoard && s.cfg.CloudBoardFor != nil && s.cfg.CloudBoardCoordinator != nil,
	}
	if legacyTrig := d.legacyTrigger(cfg); legacyTrig != "" && legacyTrig != t.lane.Trigger {
		rd.EventOnly = append(rd.EventOnly, d.legacyString(cfg, legacyTrig))
	}
	return rd
}

// spineNoClaimReason names, per bound subscription, why it did not claim the
// delivery: the labels it wanted, or its disabled reason.
func spineNoClaimReason(bound []trigger.Subscription, d forgeIssueDelivery) string {
	parts := make([]string, 0, len(bound))
	for _, row := range bound {
		if !row.Enabled {
			parts = append(parts, fmt.Sprintf("%s disabled: %s", row.BotID, row.DisabledReason))
			continue
		}
		wants := "any label"
		if len(row.Match.AddedLabels) > 0 {
			wants = fmt.Sprint(row.Match.AddedLabels)
		}
		parts = append(parts, fmt.Sprintf("%s wants %s", row.BotID, wants))
	}
	return fmt.Sprintf("no subscription claims the added labels %v: %s", d.Added, strings.Join(parts, "; "))
}

// spineIssueVars layers a spine issue launch's vars, lowest first: the issue
// context, the subscription's own vars (its invocation's context_vars over
// its manifest launch vars, merged at provision time) without the server's
// reserved launch context, the live operator layer. cfg.LaunchVars is never applied: on a spine-routed config it is the
// manifest union, the cross-bot leak the per-bot rows exist to prevent
// (ADR-172 §6). Every issue-lane target is trusted, so no trust parameter is
// threaded through.
func spineIssueVars(d forgeIssueDelivery, sub trigger.Subscription, cfg webhooks.Config) map[string]string {
	argsVar := sub.ArgsVar
	if argsVar == "" {
		argsVar = "feature_prompt"
	}
	vars := map[string]string{
		argsVar:            strings.TrimSpace(d.Title + "\n\n" + d.Body),
		"open_mr":          "true",
		"source_issue_ref": d.URL,
	}
	mergeVarsInto(vars, sub.Vars)
	// The row's vars come from a manifest: the server's launch context and
	// minted grants are never theirs to write (native.ReservedLaunchArgs).
	vars = native.DropServerMintedLaunchVars(vars)
	mergeVarsInto(vars, cfg.OperatorLaunchVars)
	return vars
}

// spineBotRefusal names why one matched subscription starts nothing on this
// webhook: out of the webhook's bot scope, disabled in the catalogue (or
// unreadable — fail closed), or a mode no lane honours. "" routes.
func (s *Server) spineBotRefusal(ctx context.Context, cfg webhooks.Config, sub trigger.Subscription) string {
	if !cfg.AllowsBot(sub.BotID) {
		return fmt.Sprintf("bot %q is not permitted by the webhook's bot scope", sub.BotID)
	}
	if reason := s.labelBotVisibilityRefusal(ctx, cfg.TenantID, sub.BotID); reason != "" {
		return reason
	}
	if _, err := bundle.ResolveMode(bundle.InvocationKindForge, sub.Mode, "forge router"); err != nil {
		return err.Error()
	}
	return ""
}

// recordSpineBotFiltered writes one bot's filtered audit row — BotID,
// SubscriptionID and the reason on it — beside the bots that do answer.
// Never a 4xx for the delivery.
func (s *Server) recordSpineBotFiltered(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta,
	payloadHash, srcIP, botID, subscriptionID, reason string) {
	s.warnf("webhooks: %s/%s %s: %s filtered on the spine: %s", cfg.Provider, meta.ProjectPath, meta.SubjectID, botID, reason)
	if s.webhookDeliveries == nil {
		return
	}
	row := newWebhookDelivery(cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP)
	row.IdempotencyKey = uuid.NewString()
	row.BotID = botID
	row.SubscriptionID = subscriptionID
	row.Error = reason
	_ = s.webhookDeliveries.Insert(ctx, row)
}

// recordSpineCardedRow leaves the audit row of a board target the spine
// claimed: status carded, the card and the subscription that claimed the
// event, under a key no lane walk reads. The card is the claim — an
// audit-row failure is logged, never undoes it.
func (s *Server) recordSpineCardedRow(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta,
	payloadHash, srcIP, base, cardID string, sub trigger.Subscription) {
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusAccepted)
	if s.webhookDeliveries == nil {
		return
	}
	row := newWebhookDelivery(cfg, meta, webhooks.StatusCarded, payloadHash, srcIP)
	row.IdempotencyKey = knowledge.ChecksumHex([]byte(base + "|carded|" + cardID))
	row.BotID = sub.BotID
	row.SubscriptionID = sub.ID
	row.CardID = cardID
	row.RoutedBy = webhooks.RoutingSpine
	if err := s.webhookDeliveries.Insert(ctx, row); err != nil && !errors.Is(err, webhooks.ErrDuplicate) {
		s.warnf("webhooks: %s/%s %s: the carded audit row of %s (card %s) could not be recorded (%v)",
			cfg.Provider, meta.ProjectPath, meta.SubjectID, sub.BotID, cardID, err)
	}
}

// spineRefuseRetryable answers a delivery the spine refuses as a sync
// problem: a launch_error audit row naming the sync route, an
// errtrack-visible warning, and a 503 the forge redelivers against. It never
// falls back to legacy.
func (s *Server) spineRefuseRetryable(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config,
	meta webhookEventMeta, payloadHash, srcIP, status, reason string) {
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, reason)
	s.warnf("webhooks: %s/%s %s: refused on the spine: %s", cfg.Provider, meta.ProjectPath, meta.SubjectID, reason)
	writeJSONStatusRaw(w, http.StatusServiceUnavailable, map[string]string{"status": status, "reason": reason})
}

// writeJSONStatusRaw writes the payload verbatim, whatever the status: the
// shared writeJSONStatus funnels every 5xx through the errtrack sanitizer,
// which replaces the body with an opaque internal-error envelope — the forge
// could then no longer read the typed status (subscriptions_stale, partial)
// the answer contract pins.
func writeJSONStatusRaw(w http.ResponseWriter, status int, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// spineDigestStale recomputes the projection digest from the live catalogue
// — the bot set forgetrigger.DigestBots names, each bot's issue routes and
// manifest launch vars, and the role bots, the SAME arguments the projector
// was given — and holds it against every bound row's stamp: a mismatch, or
// two digests among the rows, is stale. On an integration binding the bot
// set is the integration's own, read here: the config's bot scope is
// operator-patchable apart from it, and a digest over that scope would hold
// the rows stale with no sync able to cure it. A webhook binding has no
// integration and no projector writes its rows: its set is the config's
// scope with the rows' bots. A read error is returned, never read as stale.
func (s *Server) spineDigestStale(ctx context.Context, cfg webhooks.Config, bound []trigger.Subscription) (bool, error) {
	var bots []string
	if cfg.ProvisionIntegrationID != "" {
		if s.forgeIntegrations == nil {
			return false, fmt.Errorf("integration %s: no integration store is wired to read the bots its digest hashes", cfg.ProvisionIntegrationID)
		}
		ri, err := s.forgeIntegrations.Get(ctx, cfg.ProvisionIntegrationID)
		if err != nil {
			return false, fmt.Errorf("read integration %s, whose bots the projection digest hashes: %w", cfg.ProvisionIntegrationID, err)
		}
		if ri.TenantID != cfg.TenantID {
			return false, fmt.Errorf("integration %s belongs to another team than webhook %s", cfg.ProvisionIntegrationID, cfg.ID)
		}
		bots = forgetrigger.DigestBots(ri)
	} else {
		bots = slices.Clone(cfg.BotIDs)
		for _, row := range bound {
			if !slices.Contains(bots, row.BotID) {
				bots = append(bots, row.BotID)
			}
		}
		slices.Sort(bots)
		bots = slices.Compact(bots)
	}
	entries, err := s.effectiveEntriesFor(ctx, cfg.TenantID)
	if err != nil {
		return false, fmt.Errorf("read the catalogue the rows project from: %w", err)
	}
	bound2 := map[string]bool{}
	for _, bot := range bots {
		bound2[bot] = true
	}
	invByBot := map[string][]bundle.Invocation{}
	frByBot := map[string]*bundle.ForgeRequirements{}
	for _, e := range entries {
		if bound2[e.Name] {
			invByBot[e.Name] = e.Invocations
			frByBot[e.Name] = e.Forge
		}
	}
	digest := forgetrigger.ManifestDigest(bots, invByBot, frByBot, s.forgeRoleBots())
	seen := map[string]bool{}
	for _, row := range bound {
		seen[row.ProjectionDigest] = true
	}
	return len(seen) > 1 || !seen[digest], nil
}

// issueLaneSpineHalf is the spine half a LEGACY issue-lane handler covers
// once its integration has ever routed by subscription (cfg.SpineSince set):
// the elected bot whose claims the lane read covers, and per added label that
// passes the repository gate its bot-scoped base and bot lane. A never-flipped
// config passes no half, and its reads stay byte-identical.
func issueLaneSpineHalf(cfg webhooks.Config, d forgeIssueDelivery, bot string) laneSpineHalf {
	half := laneSpineHalf{Bot: bot}
	for _, label := range d.Added {
		half.Bases = append(half.Bases, botLaneBase(d.legacyBaseString(cfg, label), bot))
		half.Triggers = append(half.Triggers, label)
	}
	return half
}
