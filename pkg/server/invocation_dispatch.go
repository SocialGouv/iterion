package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// commandDiscovery is the live CommandDiscovery fallback for
// webhooks.ResolveCommandRoute: it scans the bot registry for an ENABLED bot
// whose manifest invocations claim the slash-command. Used only for a
// wildcard webhook with no provisioned CommandMap (a hand-created webhook);
// orchestrator-provisioned webhooks carry an authoritative CommandMap and
// never reach this.
//
// teamID is the tenant the resolved command will LAUNCH for, so the scan
// reads the same three tiers the launch will: a fork that renames its
// command, or moves its args var, routes and stamps as itself, and a bot the
// team authored is reachable at all. Empty only where a surface genuinely has
// no tenant (the native board, one local store).
//
// The ctx is carried on the struct because webhooks.CommandDiscovery takes
// none: the adapter is built per delivery and used within it, so the request's
// cancellation still reaches the store read.
type commandDiscovery struct {
	s      *Server
	ctx    context.Context
	teamID string
}

func (d commandDiscovery) LookupCommand(cmd string) (webhooks.CommandRoute, bool) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := d.s.effectiveEntriesFor(ctx, d.teamID)
	if err != nil {
		d.s.logWarn("command discovery: cannot read the bot catalog, /%s will not resolve: %v", cmd, err)
		return webhooks.CommandRoute{}, false
	}
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		for _, inv := range e.Invocations {
			if inv.Kind != bundle.InvocationKindCommand || inv.Command == nil {
				continue
			}
			for _, name := range append([]string{inv.Command.Name}, inv.Command.Aliases...) {
				if strings.EqualFold(strings.TrimSpace(name), cmd) {
					// A command whose mode no lane honours routes nowhere
					// (bundle.ResolveMode): it is never a launch.
					return commandRouteFromInvocation(e.Name, inv)
				}
			}
		}
	}
	return webhooks.CommandRoute{}, false
}

// commandRouteFromInvocation flattens a bundle command invocation into a
// webhooks.CommandRoute. Mirrors forge.Orchestrator.buildCommandMap so the
// live-discovery fallback and the provisioned CommandMap agree.
func commandRouteFromInvocation(botID string, inv bundle.Invocation) (webhooks.CommandRoute, bool) {
	mode, err := inv.EffectiveMode()
	if err != nil {
		return webhooks.CommandRoute{}, false
	}
	return webhooks.CommandRoute{
		BotID:          botID,
		Mode:           string(mode),
		ArgsVar:        inv.ArgsVar,
		ContextVars:    inv.ContextVars,
		Scope:          inv.Command.Scope,
		MinReplierRole: inv.Command.MinReplierRole,
		Disambiguator:  inv.Command.Disambiguator,
		OpensMR:        inv.Command.OpensMR,
	}, true
}

// cmdDiscoveryFor returns the live command-discovery fallback bound to this
// server and to the tenant the resolved command will launch for (nil-safe —
// commandDiscovery handles registry errors internally).
func (s *Server) cmdDiscoveryFor(ctx context.Context, teamID string) webhooks.CommandDiscovery {
	return commandDiscovery{s: s, ctx: ctx, teamID: teamID}
}

// boardRouteForLabel builds a synthetic board-mode CommandRoute for a
// label-triggered launch (an issue gains a trigger label → run the bot).
// It carries no slash-command — only the pieces ensureBoardCard/dispatchInvocation
// consume: the resolved bot, board mode, and the bot's issue args var + opens-MR
// behaviour. Those are taken from the bot's `command` invocation when present
// (so a labeled issue dispatches the bot exactly like its `/command` would),
// defaulting to the implementer contract (feature_prompt + opens an MR) — the
// shape every label-triggered bot (featurly et al.) follows.
//
// teamID is the tenant the card will launch for: the invocation is read from
// the tier that will serve it, so a fork's own args var is what the issue text
// lands in.
func (s *Server) boardRouteForLabel(ctx context.Context, teamID, botID string) webhooks.CommandRoute {
	route := webhooks.CommandRoute{
		BotID:   botID,
		Mode:    string(bundle.ExecutionBoard),
		ArgsVar: "feature_prompt",
		OpensMR: true,
	}
	entries, err := s.effectiveEntriesFor(ctx, teamID)
	if err != nil {
		s.logWarn("board route: cannot read the bot catalog, %s falls back to the default label contract: %v", botID, err)
		return route
	}
	for _, e := range entries {
		if e.Name != botID {
			continue
		}
		for _, inv := range e.Invocations {
			if inv.Kind != bundle.InvocationKindCommand || inv.Command == nil {
				continue
			}
			if inv.ArgsVar != "" {
				route.ArgsVar = inv.ArgsVar
			}
			route.OpensMR = inv.Command.OpensMR
			return route
		}
	}
	return route
}

// dispatchInvocation is the shared sink a comment handler calls once it has a
// resolved command route + composed vars. It launches by execution mode:
//
//	direct → launch the run immediately (the Revi path).
//	board  → when a cloud board is wired (CloudBoardFor), materialise a
//	         tracked kanban card on the tenant's board (idempotent per comment)
//	         AND launch the run, so the operator gets a visible card linking
//	         the command to its work. Auto-dispatch of the card by a cloud
//	         dispatcher (retry/stall/human-gates) is the remaining enhancement;
//	         until then the card is a tracking record + the run executes via
//	         the normal queue. Without a cloud board (self-hosted/local) it
//	         simply launches.
//
// Keeping the switch here means a comment handler is mode-agnostic.
//
// lane is the issue trigger the delivery answers (webhooks_trigger_lane.go),
// nil for a comment command. With a lane, idemKey is the lane's generation
// zero and both modes re-arm once the lane's latest claim has ended; without
// one, idemKey and the subject's card label are each claimed once.
func (s *Server) dispatchInvocation(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	cfg webhooks.Config, meta webhookEventMeta, idemKey string,
	route webhooks.CommandRoute, vars map[string]string,
	repoURL, repoRef, payloadHash, srcIP string,
	prov launchProvenance, lane *triggerLane,
) {
	// A stored route whose mode no lane honours is refused typed and
	// launches nothing — the board branch below is the only other effect,
	// and everything else used to fall through to a launch.
	if _, err := bundle.ResolveMode(bundle.InvocationKindCommand, bundle.ExecutionMode(route.Mode), "webhook dispatch"); err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, err.Error())
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": err.Error()})
		return
	}
	// Seeding the declared hand-off vars has to happen HERE rather than only in
	// the launch tail: a board-mode command with a dispatcher never reaches the
	// tail — the card IS the launch — so the seed would be dropped on exactly
	// the path most commands take. The tail keeps its own call for the lanes
	// that skip this function (the PR-event fan-out, the auto-heal).
	//
	// Placed after each early return below, never before: resolving a hand-off
	// reads the bot catalog and lists the pull request's runs, and a
	// redelivery or a quota denial launches nothing.
	seed := func() {
		s.stampHandoffs(ctx, cfg, route.BotID, vars, handoffQuery{
			PRURL:   vars["pr_url"],
			HeadSHA: vars["head_sha"],
		}, prov.Trust)
	}
	if route.Mode == string(bundle.ExecutionBoard) && s.cfg.CloudBoardFor != nil {
		// Every card this branch materialises answers the tail's provenance
		// precondition FIRST, the same function (webhookProvenanceRefusal):
		// with a coordinator the card IS the launch and the tail is never
		// reached; without one the tracking card is written before the tail
		// refuses, and would track a launch that never was. So a fork-lane
		// config never cards a same-repo target, whichever of its entries —
		// a /command, an issue label, a zero-touch open — asks, and an
		// ordinary config never cards a fork one.
		if reason := webhookProvenanceRefusal(cfg, prov.Trust, prov.ExpectedSHA); reason != "" {
			s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
			if s.logger != nil {
				s.logger.Warn("webhooks: %s/%s refused for %s: %s", cfg.Provider, meta.ProjectPath, route.BotID, reason)
			}
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
			return
		}
		if s.cfg.CloudBoardCoordinator != nil {
			// This branch RETURNS without reaching the launch tail — the card
			// is the launch. The card has no seat for provenance: it carries
			// only BotArgs, which processBoardCard lifts into a LaunchSpec
			// that hard-codes the trusted default. A launch that knows
			// something about its code and cannot carry it must not be
			// laundered into one that claims to know nothing.
			//
			// Only THIS branch: a direct-mode route, and a board route with
			// no coordinator, both fall through to the tail carrying prov,
			// and refusing them would close the path the fork review lane
			// needs.
			if !prov.Trust.Trusted() || prov.ExpectedSHA != "" {
				reason := fmt.Sprintf("board-mode dispatch cannot carry this launch's provenance (trust=%q, pinned_commit=%v): the card holds only bot args, so the run it produces would read as trusted and unpinned",
					string(prov.Trust), prov.ExpectedSHA != "")
				s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
				if s.logger != nil {
					s.logger.Warn("webhooks: %s/%s refused for %s: %s", cfg.Provider, meta.ProjectPath, route.BotID, reason)
				}
				writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
				return
			}
			// Dispatcher active: pre-check the org gate, create the card in
			// the eligible state, and let the dispatcher own execution + state
			// transitions — no direct launch (else the card would run twice).
			//
			// Idempotency BEFORE the gate: gateLaunch performs the per-org
			// quota CAS increment, and ensureBoardCard is idempotent on the
			// per-comment label only AFTER that. Short-circuit to a "carded"
			// replay when the card already exists, before gating.
			if lane != nil {
				s.cardTriggerLane(ctx, w, r, cfg, meta, idemKey, route, vars, repoURL, repoRef, payloadHash, srcIP, *lane, seed)
				return
			}
			if s.boardCardExists(cfg, meta) {
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				writeJSONStatus(w, http.StatusOK, map[string]string{"status": "carded", "bot": route.BotID})
				return
			}
			adm, d := s.gateLaunch(ctx, launchSubject{BotID: route.BotID, Repo: meta.ProjectPath})
			if d != nil {
				s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, d.reason)
				s.writeLaunchDenial(w, r, d)
				return
			}
			// A pre-check, not the metering: a card is not a run. The
			// dispatcher passes the same gate — and meters — when it claims
			// the card (processBoardCard), so the slot taken here is handed
			// back at once; keeping it would charge one command two runs.
			adm.rollback(s.logger)
			seed()
			s.ensureBoardCard(ctx, cfg, route, vars, meta, native.StateReady, repoURL, repoRef)
			s.markWebhookOutcome(cfg.Provider, webhooks.StatusAccepted)
			writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "carded", "bot": route.BotID})
			return
		}
	}
	// The launch tail claims a delivery row. A trigger lane decides WHICH row
	// first — before the tracking card below, so a delivery the lane already
	// answers writes nothing.
	var onClaimDecided func(bool, *webhooks.Delivery)
	if lane != nil {
		ev := laneEvent{PayloadHash: payloadHash, DeliveryID: meta.DeliveryID}
		claim, err := s.claimTriggerLane(ctx, cfg, idemKey, ev)
		if err != nil {
			s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, err)
			return
		}
		if claim.answered != nil {
			// The receipt names the row that answers, like the tail's own
			// duplicate: the serving claim, or the absorption recorded earlier.
			s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
			if claim.absorb {
				s.recordLaneAbsorbed(ctx, cfg, meta, idemKey, route.BotID, ev, srcIP,
					fmt.Sprintf("claim %s (run %q)", claim.answered.ID, claim.answered.RunID))
			}
			writeJSONStatus(w, http.StatusOK, map[string]string{
				"status": webhooks.StatusDuplicate, "run_id": claim.answered.RunID, "delivery_id": claim.answered.ID,
			})
			return
		}
		// What the tail decides is recorded once it is decided: the event a
		// takeover displaces only once the takeover holds, and this event as
		// absorbed when it loses the claim to ANOTHER event racing it.
		base, displaced := idemKey, claim.displaced
		onClaimDecided = func(claimed bool, existing *webhooks.Delivery) {
			switch {
			case claimed && displaced != nil:
				s.recordLaneAbsorbed(ctx, cfg, meta, base, route.BotID, *displaced, srcIP,
					"the delivery that retries its failed claim")
			case !claimed && existing != nil && !ev.claimedBy(*existing):
				s.recordLaneAbsorbed(ctx, cfg, meta, base, route.BotID, ev, srcIP,
					fmt.Sprintf("claim %s (run %q)", existing.ID, existing.RunID))
			}
		}
		idemKey = claim.key
	}
	if route.Mode == string(bundle.ExecutionBoard) && s.cfg.CloudBoardFor != nil {
		// No dispatcher: a tracking card (default inbox state) + direct launch.
		seed()
		s.ensureBoardCard(ctx, cfg, route, vars, meta, "", repoURL, repoRef)
	}
	s.acceptWebhookTargets(ctx, w, r, cfg, meta, []forgeLaunchTarget{{
		BotID: route.BotID, IdemKey: idemKey, Vars: vars, RepoURL: repoURL, RepoRef: repoRef,
		Trust: prov.Trust, ExpectedSHA: prov.ExpectedSHA, ForkAuthor: prov.ForkAuthor,
		onClaimDecided: onClaimDecided,
	}}, payloadHash, srcIP)
}

// cardTriggerLane is the board-dispatcher branch of dispatchInvocation for an
// issue trigger: the card is the claim. A delivery the lane already answers
// — redelivered, or absorbed by a card still serving — writes no card; a
// fresh one cards under the lane's next deterministic id, so a racing twin
// collides on it instead of carding twice.
func (s *Server) cardTriggerLane(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	cfg webhooks.Config, meta webhookEventMeta, base string,
	route webhooks.CommandRoute, vars map[string]string,
	repoURL, repoRef, payloadHash, srcIP string,
	lane triggerLane, seed func(),
) {
	ev := laneEvent{PayloadHash: payloadHash, DeliveryID: meta.DeliveryID}
	duplicate := func(cardID string) {
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": "carded", "bot": route.BotID, "card_id": cardID})
	}
	absorb := func(card *native.Issue) {
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
		s.recordLaneAbsorbed(ctx, cfg, meta, base, route.BotID, ev, srcIP, "card "+card.ID)
		duplicate(card.ID)
	}
	bs := s.cfg.CloudBoardFor(cfg.TenantID)
	if bs == nil {
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, fmt.Errorf("no board for team %s", cfg.TenantID))
		return
	}
	if s.webhookDeliveries != nil {
		absorbed, err := s.laneAbsorbedRow(ctx, base, ev)
		if err != nil {
			s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, err)
			return
		}
		if absorbed != nil {
			s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": "carded", "bot": route.BotID, "delivery_id": absorbed.ID})
			return
		}
	}
	answer, err := boardLaneVerdict(bs, cfg.TenantID, lane, ev)
	if err != nil {
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, err)
		return
	}
	if answer.card != nil {
		if answer.absorb {
			absorb(answer.card)
			return
		}
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
		duplicate(answer.card.ID)
		return
	}
	adm, d := s.gateLaunch(ctx, launchSubject{BotID: route.BotID, Repo: meta.ProjectPath})
	if d != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, d.reason)
		s.writeLaunchDenial(w, r, d)
		return
	}
	// A pre-check, not the metering: the dispatcher meters when it claims
	// the card (see the comment-command branch above).
	adm.rollback(s.logger)
	seed()
	card := s.buildBoardCard(ctx, cfg, route, vars, meta, native.StateReady, repoURL, repoRef)
	card.ID = answer.nextID
	card.Labels = append(card.Labels, lane.boardLabel(), boardDeliveryLabel(ev.key()))
	if _, err := bs.Create(card); err != nil {
		// Refused on the deterministic id: a racing delivery carded this
		// lane a moment ago. Any other failure is a real board problem.
		if won, gerr := bs.Get(answer.nextID); gerr == nil && won != nil {
			if ev.cardedFor(won) {
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				duplicate(won.ID)
				return
			}
			absorb(won)
			return
		}
		s.laneUnavailable(ctx, w, cfg, meta, payloadHash, srcIP, fmt.Errorf("card the trigger: %w", err))
		return
	}
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusAccepted)
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "carded", "bot": route.BotID, "card_id": answer.nextID})
}

// boardCardLabel is the subject label every board-mode card carries. For a
// comment command it is the per-comment idempotency key, the single dedupe
// key shared by boardCardExists and ensureBoardCard; a trigger lane lists its
// cards under it and claims by its own lane label instead (boardLaneVerdict).
func boardCardLabel(meta webhookEventMeta) string { return "cmd:" + meta.SubjectID }

// boardCardExists reports whether a tracking card for this comment already
// exists on the tenant's cloud board. Used to short-circuit a webhook
// redelivery BEFORE it charges the per-org quota. A store/query error is
// treated as "does not exist" so a transient blip never suppresses a genuine
// first launch (ensureBoardCard's own label check remains the backstop).
func (s *Server) boardCardExists(cfg webhooks.Config, meta webhookEventMeta) bool {
	if s.cfg.CloudBoardFor == nil {
		return false
	}
	store := s.cfg.CloudBoardFor(cfg.TenantID)
	if store == nil {
		return false
	}
	existing, err := store.List(native.ListFilter{Labels: []string{boardCardLabel(meta)}})
	return err == nil && len(existing) > 0
}

// ensureBoardCard materialises a tracking kanban card for a board-mode
// command on the tenant's cloud board, idempotently: a card carrying the
// per-comment label is created at most once, so a webhook retry doesn't
// duplicate it. Best-effort — a board error never fails the command (the run
// still launches). The card is assigned to the bot (Assignee + Bot) and
// carries the command args as bot_args.
func (s *Server) ensureBoardCard(ctx context.Context, cfg webhooks.Config, route webhooks.CommandRoute, vars map[string]string, meta webhookEventMeta, initialState, repoURL, repoRef string) {
	store := s.cfg.CloudBoardFor(cfg.TenantID)
	if store == nil {
		return
	}
	label := boardCardLabel(meta)
	if existing, err := store.List(native.ListFilter{Labels: []string{label}}); err == nil && len(existing) > 0 {
		return // already materialised for this comment
	}
	if _, err := store.Create(s.buildBoardCard(ctx, cfg, route, vars, meta, initialState, repoURL, repoRef)); err != nil && s.logger != nil {
		s.logger.Warn("webhooks: board card create failed (tenant=%s bot=%s): %v", cfg.TenantID, route.BotID, err)
	}
}

// buildBoardCard composes the card a board-mode command or trigger cards: its
// title and body, the bot args its launch needs, and the subject's "cmd:"
// label. It writes nothing.
func (s *Server) buildBoardCard(ctx context.Context, cfg webhooks.Config, route webhooks.CommandRoute, vars map[string]string, meta webhookEventMeta, initialState, repoURL, repoRef string) native.Issue {
	title := boardCardTitle(route, vars)
	botArgs := map[string]string{}
	if route.ArgsVar != "" {
		if v, ok := vars[route.ArgsVar]; ok && v != "" {
			botArgs[route.ArgsVar] = v
		}
	}
	// PR/MR-context vars stamped by the command handlers (resolved PR head/
	// base, push-back routing, the PR to comment the verdict on). They must
	// ride BotArgs for the same reason as the opens_mr stamp below: cloud's
	// processBoardCard launches with iss.BotArgs ONLY, so a var left in the
	// webhook launch vars never reaches a board-mode run — the bot then works
	// off its DSL defaults (e.g. mr_gate.push_back=false strands the campaign
	// commits on the runner's storage branch instead of the PR).
	carry := []string{"pr_url", "base_ref", "target_branch", "source_branch", "pr_author", "push_branch", "open_mr", "mr_base", "head_sha"}
	// Plus whatever THIS bot declared it consumes from an earlier run on the
	// same PR. Derived rather than listed: a fixed list would mean a bot that
	// declares a new hand-off gets it on the direct lane and silently loses it
	// on the board lane — the failure this whole carry-list exists to prevent.
	for _, c := range s.handoffConsumersFor(ctx, cfg.TenantID, route.BotID) {
		carry = append(carry, c.Var)
	}
	for _, k := range carry {
		if v, ok := vars[k]; ok && v != "" {
			botArgs[k] = v
		}
	}
	// opens_mr stamp: a command whose bot opens an MR + back-links the issue
	// the human commented on. Stamped into BotArgs (NOT just launch vars) so it
	// survives BOTH board-mode backends: the local dispatcher's buildSpec
	// merges iss.BotArgs over dispatch_vars (BotArgs wins), and cloud's
	// processBoardCard launches with iss.BotArgs ONLY (ignores dispatch_vars).
	// The three improvement bots declare open_mr / source_issue_ref as vars; the
	// stamp is gated by route.OpensMR so unrelated board commands aren't stamped.
	if route.OpensMR && meta.SubjectURL != "" {
		botArgs["open_mr"] = "true"
		botArgs["source_issue_ref"] = meta.SubjectURL
	}
	// Repo-bound webhook command (issue-comment → MR): the cloud board
	// coordinator launches from the card with no RepoURL of its own, so stamp
	// the clone URL/ref into BotArgs under reserved keys. processBoardCard lifts
	// them into LaunchSpec.RepoURL/RepoRef and strips them from the bot's vars,
	// so the runner clones the repo before sandboxing.
	if repoURL != "" {
		botArgs[boardRepoURLKey] = repoURL
		if repoRef != "" {
			botArgs[boardRepoRefKey] = repoRef
		}
	}
	// The forge slug alongside (#2371, D33): the payload's OWN project path,
	// byte-for-byte the identity the direct lane stamps and the meter keys
	// on — no URL re-derivation, so a forge under a sub-path base or a slug
	// whose case a canonicaliser would fold still lands on the very key its
	// quota is read from. Stamped whenever the payload names it, not only
	// when it carries a clone URL: the slug does not depend on the clone,
	// and an event without one would otherwise leave the card — and the run
	// it launches — without the repository identity its quota reads. The
	// launch honors it only under the provenance boardCardRepoIdentity
	// spells out; the write routes a client drives never persist it
	// (native.DropReservedLaunchArgs).
	if meta.ProjectPath != "" {
		botArgs[boardProjectPathKey] = meta.ProjectPath
	}
	// Carry the webhook's BYOK key / secret overrides onto the card so the
	// board coordinator's launch applies them (it resolves nothing from the
	// webhook itself). forge_token also resolves via a (tenant,bot) binding —
	// this is the belt to that braces and the only route for a per-webhook
	// KeyOverride, which has no binding equivalent. JSON so a map survives the
	// string→string BotArgs.
	if len(cfg.KeyOverrides) > 0 {
		if raw, err := json.Marshal(cfg.KeyOverrides); err == nil {
			botArgs[boardKeyOverridesKey] = string(raw)
		}
	}
	if len(cfg.SecretOverrides) > 0 {
		if raw, err := json.Marshal(cfg.SecretOverrides); err == nil {
			botArgs[boardSecretOverridesKey] = string(raw)
		}
	}
	// Name the integration whose LAUNCH-POLICY layers the card's launch must
	// resolve — its routing block (ADR-129's per-integration cells and its
	// `providers:` allowlist) and its retry block. The reason is stronger
	// than for the keys above: a board-mode run that applies this
	// integration's BYOK credential while resolving no routing layer spends
	// that credential with the integration's own lock on where it may be
	// spent ABSENT. The direct lane passes both layers to the resolvers
	// (launchWebhookBot); a card lane reads them back from this id
	// (cardLaunchPolicyLayers), so the live config answers and the level the
	// run records is the level that decided.
	if cfg.ID != "" {
		botArgs[boardWebhookIDKey] = cfg.ID
	} else if s.logger != nil {
		s.logger.Warn("webhooks: board card for %s/%s names no integration (the config carries no id), so its launch resolves no integration-level routing or retry block", cfg.TenantID, route.BotID)
	}
	return native.Issue{
		Title:    truncate(title, 120),
		Body:     boardCardBody(route, vars, meta),
		State:    initialState, // "" → the board's first state (inbox)
		Assignee: route.BotID,
		Bot:      route.BotID,
		Labels:   []string{boardCardLabel(meta), "source:command", "provider:" + string(cfg.Provider)},
		BotArgs:  botArgs,
	}
}

// boardCardMission returns the human-readable "what was the bot asked to do"
// text for a board card, from what the trigger actually put in the launch vars:
// an explicit `scope_notes` when set (the slash-command path stamps it), else
// the route's args var (the issue-labeled path puts the issue title+body there
// under `feature_prompt`). Empty when neither is populated.
func boardCardMission(route webhooks.CommandRoute, vars map[string]string) string {
	if sn := strings.TrimSpace(vars["scope_notes"]); sn != "" {
		return sn
	}
	if route.ArgsVar != "" {
		if v := strings.TrimSpace(vars[route.ArgsVar]); v != "" {
			return v
		}
	}
	return ""
}

// boardCardTitle derives the card title from the mission's first line so a
// labeled issue's card is titled by the issue (not just the bot id), falling
// back to the bare bot id when no mission text is available.
func boardCardTitle(route webhooks.CommandRoute, vars map[string]string) string {
	if m := boardCardMission(route, vars); m != "" {
		return route.BotID + " — " + firstLine(m)
	}
	return route.BotID
}

// boardCardBody composes the card body from what is actually available: a
// markdown link to the triggering subject (issue/MR) when its URL is known,
// then the mission text so the operator can read WHAT the bot was asked to do
// and click through to the source. When no subject URL is available it falls
// back to a provenance trigger line so the card is never bodyless.
func boardCardBody(route webhooks.CommandRoute, vars map[string]string, meta webhookEventMeta) string {
	var b strings.Builder
	if meta.SubjectURL != "" {
		label := strings.TrimSpace(strings.TrimSuffix(meta.ProjectPath+"/"+meta.SubjectID, "/"))
		if label == "" {
			label = meta.SubjectURL
		}
		fmt.Fprintf(&b, "[%s](%s)", label, meta.SubjectURL)
	} else {
		fmt.Fprintf(&b, "Triggered by a /%s-style command on %s/%s.", route.BotID, meta.ProjectPath, meta.SubjectID)
	}
	if m := boardCardMission(route, vars); m != "" {
		b.WriteString("\n\n")
		// Bounded: the mission can be a full issue body (~65k chars on
		// GitHub) and the card body rides every board-list payload; the
		// canonical text stays in bot_args / the linked issue.
		b.WriteString(truncate(m, 4000))
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// truncate caps s at n bytes, ending on a rune boundary. Slicing bytes blindly
// splits a multi-byte rune, and the result — a card title, a prompt var — is
// then invalid UTF-8 wherever it lands.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
