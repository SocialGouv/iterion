package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/botregistry"
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
type commandDiscovery struct{ s *Server }

func (d commandDiscovery) LookupCommand(cmd string) (webhooks.CommandRoute, bool) {
	entries, err := botregistry.List(botregistry.ListOptions{Paths: d.s.effectivePaths()})
	if err != nil {
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
					return commandRouteFromInvocation(e.Name, inv), true
				}
			}
		}
	}
	return webhooks.CommandRoute{}, false
}

// commandRouteFromInvocation flattens a bundle command invocation into a
// webhooks.CommandRoute. Mirrors forge.Orchestrator.buildCommandMap so the
// live-discovery fallback and the provisioned CommandMap agree.
func commandRouteFromInvocation(botID string, inv bundle.Invocation) webhooks.CommandRoute {
	return webhooks.CommandRoute{
		BotID:          botID,
		Mode:           string(inv.EffectiveMode()),
		ArgsVar:        inv.ArgsVar,
		ContextVars:    inv.ContextVars,
		Scope:          inv.Command.Scope,
		MinReplierRole: inv.Command.MinReplierRole,
		Disambiguator:  inv.Command.Disambiguator,
		OpensMR:        inv.Command.OpensMR,
	}
}

// cmdDiscovery returns the live command-discovery fallback bound to this
// server (nil-safe — commandDiscovery handles registry errors internally).
func (s *Server) cmdDiscovery() webhooks.CommandDiscovery { return commandDiscovery{s: s} }

// boardRouteForLabel builds a synthetic board-mode CommandRoute for a
// label-triggered launch (an issue gains a trigger label → run the bot).
// It carries no slash-command — only the pieces ensureBoardCard/dispatchInvocation
// consume: the resolved bot, board mode, and the bot's issue args var + opens-MR
// behaviour. Those are taken from the bot's `command` invocation when present
// (so a labeled issue dispatches the bot exactly like its `/command` would),
// defaulting to the implementer contract (feature_prompt + opens an MR) — the
// shape every label-triggered bot (featurly et al.) follows.
func (s *Server) boardRouteForLabel(botID string) webhooks.CommandRoute {
	route := webhooks.CommandRoute{
		BotID:   botID,
		Mode:    string(bundle.ExecutionBoard),
		ArgsVar: "feature_prompt",
		OpensMR: true,
	}
	entries, err := botregistry.List(botregistry.ListOptions{Paths: s.effectivePaths()})
	if err != nil {
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
func (s *Server) dispatchInvocation(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	cfg webhooks.Config, meta webhookEventMeta, idemKey string,
	route webhooks.CommandRoute, vars map[string]string,
	repoURL, repoRef, payloadHash, srcIP string,
) {
	// Seed the declared hand-off vars BEFORE the mode switch. A board-mode
	// command with a dispatcher never reaches the launch tail — the card IS the
	// launch — so stamping only there would silently drop the seed on exactly
	// the path most commands take. The launch tail keeps its own call for the
	// lanes that skip this function (the PR-event fan-out, the auto-heal); it
	// no-ops when the var is already set here.
	s.stampPriorReview(ctx, cfg, route.BotID, vars, priorReviewQuery{
		PRURL:   vars["pr_url"],
		HeadSHA: vars["head_sha"],
	})
	if route.Mode == string(bundle.ExecutionBoard) && s.cfg.CloudBoardFor != nil {
		if s.cfg.CloudBoardCoordinator != nil {
			// Dispatcher active: gate (per-org quota), create the card in the
			// eligible state, and let the dispatcher own execution + state
			// transitions — no direct launch (else the card would run twice).
			//
			// Idempotency BEFORE metering: gateLaunch performs the per-org
			// quota CAS increment, and ensureBoardCard is idempotent on the
			// per-comment label only AFTER that. So a webhook redelivery would
			// re-charge the quota while creating no new card. Short-circuit to
			// a "carded" replay when the card already exists, before gating.
			if s.boardCardExists(cfg, meta) {
				s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
				writeJSONStatus(w, http.StatusOK, map[string]string{"status": "carded", "bot": route.BotID})
				return
			}
			if _, d := s.gateLaunch(ctx); d != nil {
				s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, d.reason)
				s.writeLaunchDenial(w, r, d)
				return
			}
			s.ensureBoardCard(ctx, cfg, route, vars, meta, native.StateReady, repoURL, repoRef)
			s.markWebhookOutcome(cfg.Provider, webhooks.StatusAccepted)
			writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "carded", "bot": route.BotID})
			return
		}
		// No dispatcher: a tracking card (default inbox state) + direct launch.
		s.ensureBoardCard(ctx, cfg, route, vars, meta, "", repoURL, repoRef)
	}
	s.insertAndLaunchWebhook(ctx, w, r, cfg, meta, idemKey, route.BotID, vars, repoURL, repoRef, payloadHash, srcIP)
}

// ensureBoardCard materialises a tracking kanban card for a board-mode
// command on the tenant's cloud board, idempotently: a card carrying the
// per-comment label is created at most once, so a webhook retry doesn't
// duplicate it. Best-effort — a board error never fails the command (the run
// still launches). The card is assigned to the bot (Assignee + Bot) and
// carries the command args as bot_args.
// boardCardLabel is the per-comment idempotency label a board-mode command
// card carries — the single dedupe key shared by boardCardExists and
// ensureBoardCard.
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

func (s *Server) ensureBoardCard(ctx context.Context, cfg webhooks.Config, route webhooks.CommandRoute, vars map[string]string, meta webhookEventMeta, initialState, repoURL, repoRef string) {
	store := s.cfg.CloudBoardFor(cfg.TenantID)
	if store == nil {
		return
	}
	label := boardCardLabel(meta)
	if existing, err := store.List(native.ListFilter{Labels: []string{label}}); err == nil && len(existing) > 0 {
		return // already materialised for this comment
	}
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
	for _, c := range s.handoffConsumersFor(route.BotID) {
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
	body := boardCardBody(route, vars, meta)
	if _, err := store.Create(native.Issue{
		Title:    truncate(title, 120),
		Body:     body,
		State:    initialState, // "" → the board's first state (inbox)
		Assignee: route.BotID,
		Bot:      route.BotID,
		Labels:   []string{label, "source:command", "provider:" + string(cfg.Provider)},
		BotArgs:  botArgs,
	}); err != nil && s.logger != nil {
		s.logger.Warn("webhooks: board card create failed (tenant=%s bot=%s): %v", cfg.TenantID, route.BotID, err)
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
